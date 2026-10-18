package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

type proxyTransport struct {
	index  int
	name   string
	pool   bool
	client *http.Client
	// A fresh proxy connection lets a rotating gateway select a new exit.
	freshConnections bool
	healthy          atomic.Bool
	checking         atomic.Bool
}

type transportPool struct {
	items []*proxyTransport
}

// anonymousPool gives the shared OpenCode "public" credential an independent
// cooldown per proxy. Changing proxy is the failover mechanism because
// OpenCode rate-limits anonymous requests by exit IP.
type anonymousPool struct {
	nodes    []*anonymousNode
	next     atomic.Uint64
	cooldown time.Duration
	hasPool  bool
	rotating bool
}

type anonymousNode struct {
	proxy         *proxyTransport
	failures      atomic.Uint32
	cooldownUntil atomic.Int64
}

type anonymousCursor struct {
	pool   *anonymousPool
	start  int
	offset int
}

func newAnonymousPool(transports *transportPool, cooldown time.Duration, rotating bool) *anonymousPool {
	pool := &anonymousPool{cooldown: cooldown, rotating: rotating}
	if transports == nil {
		return pool
	}
	pool.nodes = make([]*anonymousNode, 0, len(transports.items))
	for _, proxy := range transports.items {
		if pool.isRotating(proxy) {
			pool.hasPool = true
		}
		pool.nodes = append(pool.nodes, &anonymousNode{proxy: proxy})
	}
	return pool
}

func (p *anonymousPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.nodes)
}

func (p *anonymousPool) HasPool() bool {
	return p != nil && p.hasPool
}

// isRotating recognizes both explicit #pool entries and the original global
// fresh-connection mode when its anonymous retry budget is enabled.
func (p *anonymousPool) isRotating(proxy *proxyTransport) bool {
	return proxy != nil && (proxy.pool || (p != nil && p.rotating && proxy.freshConnections))
}

func (p *anonymousPool) CursorFor(affinity string) anonymousCursor {
	if p == nil || len(p.nodes) == 0 {
		return anonymousCursor{pool: p}
	}
	start := 0
	if affinity == "" {
		start = int((p.next.Add(1) - 1) % uint64(len(p.nodes)))
	} else {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(affinity))
		start = int(hash.Sum64() % uint64(len(p.nodes)))
	}
	return anonymousCursor{pool: p, start: start}
}

// Next visits healthy, non-cooling fixed exits at most once. Rotating
// gateways may be revisited within the caller's finite attempt budget; each
// visit creates a fresh connection instead of reusing a failed exit.
func (c *anonymousCursor) Next() *anonymousNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	n := len(c.pool.nodes)
	now := time.Now().UnixNano()
	for range n {
		node := c.pool.nodes[(c.start+c.offset)%n]
		revisited := c.offset >= n
		c.offset++
		if c.pool.isRotating(node.proxy) {
			return node
		}
		if !revisited && node.proxy.healthy.Load() && node.cooldownUntil.Load() <= now {
			return node
		}
	}
	return nil
}

func (p *anonymousPool) MarkSuccess(node *anonymousNode) {
	if node == nil {
		return
	}
	if node.failures.Load() != 0 {
		node.failures.Store(0)
	}
	if node.cooldownUntil.Load() != 0 {
		node.cooldownUntil.Store(0)
	}
}

func (p *anonymousPool) RestoreProxy(proxy *proxyTransport) int {
	if p == nil || proxy == nil {
		return 0
	}
	cleared := 0
	for _, node := range p.nodes {
		if node.proxy != proxy {
			continue
		}
		if node.cooldownUntil.Load() == 0 && node.failures.Load() == 0 {
			continue
		}
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		cleared++
	}
	return cleared
}

const maxAnonymousRetryAfterMultiplier = 8

func (p *anonymousPool) MarkFailure(node *anonymousNode, resp *http.Response, err error) {
	if node == nil || p.isRotating(node.proxy) {
		return
	}
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	delay := p.cooldown * time.Duration(1<<min(failures-1, 3))
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = min(retryAfter, p.cooldown*maxAnonymousRetryAfterMultiplier)
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

func (p *transportPool) hasHealthy() bool {
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			return true
		}
	}
	return false
}

func (p *transportPool) healthCounts() (total, healthy int) {
	if p == nil {
		return 0, 0
	}
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			healthy++
		}
	}
	return len(p.items), healthy
}

func newTransportPool(proxies []string, cfg config.PerformanceConfig, responseHeaderTimeout time.Duration) (*transportPool, error) {
	p := &transportPool{items: make([]*proxyTransport, 0, len(proxies))}
	for _, raw := range proxies {
		clean, isPool := config.ParseProxyEntry(raw)
		fresh := clean != "direct" && (isPool || cfg.DisableProxyKeepAlives)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = cfg.MaxIdleConns
		transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
		transport.MaxConnsPerHost = cfg.MaxConnsPerHost
		transport.IdleConnTimeout = time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		transport.WriteBufferSize = 64 << 10
		transport.ReadBufferSize = 64 << 10
		transport.ForceAttemptHTTP2 = !fresh
		transport.DisableKeepAlives = fresh
		if fresh {
			transport.Protocols = new(http.Protocols)
			transport.Protocols.SetHTTP1(true)
			// Cloning DefaultTransport after its first dial inherits the
			// already-configured HTTP/2 ALPN and TLSNextProto map, which ignores
			// DisableKeepAlives and req.Close and multiplexes every request over
			// one CONNECT tunnel. An explicit empty TLSNextProto map and HTTP/1.1
			// ALPN force a brand-new TCP + CONNECT tunnel per attempt.
			transport.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
			} else {
				tlsCfg := transport.TLSClientConfig.Clone()
				tlsCfg.NextProtos = []string{"http/1.1"}
				transport.TLSClientConfig = tlsCfg
			}
		} else {
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(256)}
			} else if transport.TLSClientConfig.ClientSessionCache == nil {
				tlsCfg := transport.TLSClientConfig.Clone()
				tlsCfg.ClientSessionCache = tls.NewLRUClientSessionCache(256)
				transport.TLSClientConfig = tlsCfg
			}
		}
		transport.DialContext = (&net.Dialer{
			Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		if clean == "direct" {
			transport.Proxy = nil
		} else {
			u, err := url.Parse(clean)
			if err != nil {
				return nil, fmt.Errorf("parse proxy %s: %w", config.RedactURL(raw), err)
			}
			transport.Proxy = http.ProxyURL(u)
		}
		proxy := &proxyTransport{index: len(p.items), name: raw, pool: isPool, freshConnections: fresh, client: &http.Client{Transport: transport}}
		proxy.healthy.Store(true)
		p.items = append(p.items, proxy)
	}
	return p, nil
}

type proxyHealthResult struct {
	proxy      *proxyTransport
	err        error
	failed     bool
	wasHealthy bool
}

func (p *transportPool) CheckHealth(ctx context.Context, target string, timeout time.Duration) []proxyHealthResult {
	results := make(chan proxyHealthResult, len(p.items))
	checks := 0
	for _, proxy := range p.items {
		if proxy.pool || proxy.healthy.Load() || !proxy.checking.CompareAndSwap(false, true) {
			continue
		}
		if proxy.healthy.Load() {
			proxy.checking.Store(false)
			continue
		}
		checks++
		go func(proxy *proxyTransport) {
			results <- p.checkClaimedProxy(ctx, proxy, target, timeout)
		}(proxy)
	}
	out := make([]proxyHealthResult, 0, checks)
	for range checks {
		out = append(out, <-results)
	}
	return out
}

func (p *transportPool) checkClaimedProxy(ctx context.Context, proxy *proxyTransport, target string, timeout time.Duration) proxyHealthResult {
	defer proxy.checking.Store(false)
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("User-Agent", httpx.UserAgent())
		resp, requestErr := proxy.client.Do(req)
		err = requestErr
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		}
	}
	result := proxyHealthResult{proxy: proxy, err: err, wasHealthy: proxy.healthy.Load()}
	if err == nil {
		result.wasHealthy = proxy.healthy.Swap(true)
	} else if isProxyFailure(err) {
		result.failed = true
		result.wasHealthy = proxy.healthy.Swap(false)
	}
	return result
}

func isProxyFailure(err error) bool {
	if errors.Is(err, errFirstEventTimeout) || upstreamAccepted(err) {
		return false
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}

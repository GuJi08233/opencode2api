package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

// This proxy counts real TCP tunnels, including TLS and HTTP/2 traffic.
func newSOCKSProxy(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	connections := new(atomic.Int32)
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer client.Close()
				var greeting [2]byte
				if _, err := io.ReadFull(client, greeting[:]); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, client, int64(greeting[1])); err != nil {
					return
				}
				if _, err := client.Write([]byte{5, 0}); err != nil {
					return
				}
				var request [4]byte
				if _, err := io.ReadFull(client, request[:]); err != nil || request[1] != 1 {
					return
				}
				size := 0
				switch request[3] {
				case 1:
					size = net.IPv4len
				case 4:
					size = net.IPv6len
				case 3:
					var length [1]byte
					if _, err := io.ReadFull(client, length[:]); err != nil {
						return
					}
					size = int(length[0])
				default:
					return
				}
				address := make([]byte, size+2)
				if _, err := io.ReadFull(client, address); err != nil {
					return
				}
				host := string(address[:size])
				if request[3] != 3 {
					host = net.IP(address[:size]).String()
				}
				port := strconv.Itoa(int(binary.BigEndian.Uint16(address[size:])))
				upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 2*time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				done := make(chan struct{})
				go func() {
					io.Copy(upstream, client)
					upstream.Close()
					close(done)
				}()
				io.Copy(client, upstream)
				client.Close()
				<-done
			}()
		}
	}()
	return "socks5h://" + listener.Addr().String(), connections
}

func testPerformance(fresh bool) config.PerformanceConfig {
	return config.PerformanceConfig{
		DisableProxyKeepAlives: fresh, MaxIdleConns: 8, MaxIdleConnsPerHost: 4,
		IdleConnTimeoutSeconds: 10, ConnectTimeoutSeconds: 2, FailureCooldownSeconds: 15,
	}
}

func TestProxyConnectionReuse(t *testing.T) {
	for _, tc := range []struct {
		name, protocol string
		fresh, direct  bool
		pool           bool
		connections    int32
	}{
		{name: "default SOCKS tunnel reused", protocol: "HTTP/2.0", connections: 1},
		{name: "fresh SOCKS tunnel per request", protocol: "HTTP/1.1", fresh: true, connections: 2},
		{name: "marked pool overrides reusable proxy setting", protocol: "HTTP/1.1", pool: true, connections: 2},
		{name: "direct HTTP2 remains reusable", protocol: "HTTP/2.0", fresh: true, direct: true, connections: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connections := new(atomic.Int32)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Test-Protocol", r.Proto)
				io.WriteString(w, "ok")
			}))
			upstream.EnableHTTP2 = true
			upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			upstream.StartTLS()
			t.Cleanup(upstream.Close)
			roots := x509.NewCertPool()
			roots.AddCert(upstream.Certificate())
			// An already-used default transport carries HTTP/2 ALPN state into
			// Clone. Exercise that production case before testing fresh tunnels.
			originalDefault := http.DefaultTransport
			initializedDefault := originalDefault.(*http.Transport).Clone()
			initializedDefault.TLSClientConfig = &tls.Config{RootCAs: roots}
			warmup, err := (&http.Client{Transport: initializedDefault}).Get(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, warmup.Body)
			warmup.Body.Close()
			if warmup.ProtoMajor != 2 {
				t.Fatalf("warmup protocol = %s, want HTTP/2.0", warmup.Proto)
			}
			initializedDefault.CloseIdleConnections()
			connections.Store(0)
			http.DefaultTransport = initializedDefault
			t.Cleanup(func() {
				http.DefaultTransport = originalDefault
				initializedDefault.CloseIdleConnections()
			})
			proxyURL, tunnels := newSOCKSProxy(t)
			if tc.direct {
				proxyURL = "direct"
			} else if tc.pool {
				proxyURL += "#pool"
			}
			pool, err := newTransportPool([]string{proxyURL}, testPerformance(tc.fresh), 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			client := pool.items[0].client
			transport := client.Transport.(*http.Transport)
			t.Cleanup(transport.CloseIdleConnections)
			// Preserve the cloned transport's ALPN configuration, as production
			// does. Replacing it would hide inherited HTTP/2 negotiation bugs.
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{}
			}
			transport.TLSClientConfig.RootCAs = roots
			for range 2 {
				resp, err := client.Get(upstream.URL)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if got := resp.Header.Get("X-Test-Protocol"); got != tc.protocol {
					t.Fatalf("protocol = %s, want %s", got, tc.protocol)
				}
			}
			if got := connections.Load(); got != tc.connections {
				t.Fatalf("upstream TCP connections = %d, want %d", got, tc.connections)
			}
			if !tc.direct && tunnels.Load() != tc.connections {
				t.Fatalf("SOCKS tunnels = %d, want %d", tunnels.Load(), tc.connections)
			}
		})
	}
}

// anonymousMockTransport keeps both anonymous lanes inside the local proxy
// fixture. In production the legacy lane uses HTTPS; downgrading only this
// test request avoids a CONNECT handshake while retaining real proxy TCP
// connection accounting. Reject every other destination before any dial.
type anonymousMockTransport struct {
	base *http.Transport
}

func (transport anonymousMockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), models.LegacyZenBase+"/") {
		req = req.Clone(req.Context())
		req.URL.Scheme = "http"
		req.URL.Host = "upstream.invalid"
		req.Host = req.URL.Host
	} else if req.URL.Scheme != "http" || req.URL.Host != "upstream.invalid" {
		return nil, fmt.Errorf("unexpected mock upstream: %s", req.URL)
	}
	return transport.base.RoundTrip(req)
}

func TestAnonymousRotatingProxyRetries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statuses   []int
		budget     int
		attempts   int
		wantStatus int
		pool       bool
		wantLanes  []string
	}{
		{name: "legacy lane succeeds first", statuses: []int{200}, budget: 3, attempts: 1, wantStatus: 200, wantLanes: []string{"legacy"}},
		{name: "quota then server failure then success", statuses: []int{429, 503, 200}, budget: 3, attempts: 3, wantStatus: 200, wantLanes: []string{"legacy", "inference", "legacy"}},
		{name: "dropped tunnel then success", statuses: []int{0, 200}, budget: 3, attempts: 2, wantStatus: 200, wantLanes: []string{"legacy", "inference"}},
		{name: "finite retry budget", statuses: []int{429}, budget: 3, attempts: 3, wantStatus: 429, wantLanes: []string{"legacy", "inference", "legacy"}},
		{name: "one attempt cannot enter fallback lane", statuses: []int{429, 200}, budget: 1, attempts: 1, wantStatus: 429, wantLanes: []string{"legacy"}},
		{name: "two attempts cover both lanes once", statuses: []int{429, 503, 200}, budget: 2, attempts: 2, wantStatus: 503, wantLanes: []string{"legacy", "inference"}},
		{name: "marked pool rotates without global fresh setting", statuses: []int{429, 503, 200}, attempts: 3, wantStatus: 200, pool: true, wantLanes: []string{"legacy", "inference", "legacy"}},
		{name: "marked pool default budget counts proxy visits", statuses: []int{429}, attempts: 6, wantStatus: 429, pool: true, wantLanes: []string{"legacy", "inference", "legacy", "inference", "legacy", "inference"}},
		{name: "marked pool respects explicit HTTP budget", statuses: []int{429}, budget: 3, attempts: 3, wantStatus: 429, pool: true, wantLanes: []string{"legacy", "inference", "legacy"}},
		{name: "invalid request not replayed", statuses: []int{400, 200}, budget: 3, attempts: 1, wantStatus: 400, wantLanes: []string{"legacy"}},
		{name: "missing legacy lane falls back", statuses: []int{404, 200}, budget: 3, attempts: 2, wantStatus: 200, wantLanes: []string{"legacy", "inference"}},
		{name: "fallback invalid request not replayed", statuses: []int{404, 400, 200}, budget: 3, attempts: 2, wantStatus: 400, wantLanes: []string{"legacy", "inference"}},
		{name: "single fixed proxy keeps lane fallback", statuses: []int{429, 200}, attempts: 2, wantStatus: 200, wantLanes: []string{"legacy", "inference"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, connections := new(atomic.Int32), new(atomic.Int32)
			var pathsMu sync.Mutex
			var paths []string
			proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pathsMu.Lock()
				paths = append(paths, r.URL.Path)
				pathsMu.Unlock()
				i := int(calls.Add(1)) - 1
				status := tc.statuses[min(i, len(tc.statuses)-1)]
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(status)
				io.WriteString(w, `{"ok":true}`)
			}))
			proxy.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			proxy.Start()
			t.Cleanup(proxy.Close)
			fresh := tc.budget > 0
			proxyURL := proxy.URL
			if tc.pool {
				proxyURL += "#pool"
			}
			pool, err := newTransportPool([]string{proxyURL}, testPerformance(fresh), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			transport := pool.items[0].client.Transport.(*http.Transport)
			t.Cleanup(transport.CloseIdleConnections)
			pool.items[0].client.Transport = anonymousMockTransport{base: transport}
			g := &Gateway{
				cfg:    config.Config{Retry: config.RetryConfig{MaxAttempts: 3, AnonymousMaxAttempts: tc.budget}, Upstream: config.UpstreamConfig{Anonymous: "http://upstream.invalid/inference"}},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transports: pool,
				anonymous: newAnonymousPool(pool, 15*time.Second, fresh),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			route := models.Route{ID: "test-free", Protocol: wire.Chat}
			body := []byte(`{"model":"test-free","messages":[{"role":"user","content":"hello"}]}`)
			resp, err, attempts := g.doAnonymousUpstream(ctx, route, body, identity.RequestIDs{}, 0)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if attempts != tc.attempts || int(calls.Load()) != tc.attempts || resp.StatusCode != tc.wantStatus {
				t.Fatalf("attempts=%d calls=%d status=%d; want %d attempts, status %d", attempts, calls.Load(), resp.StatusCode, tc.attempts, tc.wantStatus)
			}
			pathsMu.Lock()
			defer pathsMu.Unlock()
			for i, lane := range tc.wantLanes {
				prefix := "/zen"
				if lane == "inference" {
					prefix = "/inference"
				}
				if want := prefix + wire.Path(wire.Chat); paths[i] != want {
					t.Fatalf("attempt %d path = %q, want %q (%s lane)", i+1, paths[i], want, lane)
				}
			}
			if (fresh || tc.pool) && int(connections.Load()) != tc.attempts {
				t.Fatalf("connections=%d, want one per attempt (%d)", connections.Load(), tc.attempts)
			}
			if (fresh || tc.pool) && (!pool.items[0].healthy.Load() || g.anonymous.nodes[0].cooldownUntil.Load() != 0) {
				t.Fatal("one exit failure cooled down the entire rotating gateway")
			}
		})
	}
}

func TestAnonymousFixedExitStillCoolsDown(t *testing.T) {
	proxy := &proxyTransport{}
	proxy.healthy.Store(true)
	pool := newAnonymousPool(&transportPool{items: []*proxyTransport{proxy}}, time.Second, true)
	node := pool.nodes[0]
	pool.MarkFailure(node, &http.Response{StatusCode: 429, Header: make(http.Header)}, nil)
	cursor := pool.CursorFor("")
	if cursor.Next() != nil {
		t.Fatal("fixed exit in cooldown was selected")
	}
}

func TestReasoningRepairSharesAnonymousAttemptBudget(t *testing.T) {
	for _, tc := range []struct {
		budget, wantCalls, wantStatus int
	}{
		{budget: 1, wantCalls: 1, wantStatus: http.StatusBadRequest},
		{budget: 2, wantCalls: 2, wantStatus: http.StatusBadRequest},
		{budget: 3, wantCalls: 3, wantStatus: http.StatusOK},
	} {
		t.Run(strconv.Itoa(tc.budget), func(t *testing.T) {
			calls := new(atomic.Int32)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch calls.Add(1) {
				case 1:
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":{"message":"reasoning item expired"}}`)
				case 2:
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, `{"error":{"message":"rate limited"}}`)
				default:
					io.WriteString(w, `{"ok":true}`)
				}
			}))
			defer upstream.Close()
			pool, err := newTransportPool([]string{upstream.URL + "#pool"}, testPerformance(true), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.items[0].client.CloseIdleConnections()
			g := &Gateway{
				cfg: config.Config{
					Retry:    config.RetryConfig{MaxAttempts: 3, AnonymousMaxAttempts: tc.budget},
					Upstream: config.UpstreamConfig{Zen: "http://upstream.invalid/zen", Anonymous: "http://upstream.invalid/inference"},
				},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transports: pool,
				anonymous: newAnonymousPool(pool, time.Second, true),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			resp, err := g.doUpstream(ctx, models.Route{ID: "test-free", Protocol: wire.Responses},
				[]byte(`{"model":"test-free","previous_response_id":"rs_old","input":[{"role":"user","content":"hello"}]}`), identity.RequestIDs{})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if int(calls.Load()) != tc.wantCalls || resp.StatusCode != tc.wantStatus {
				t.Fatalf("calls=%d status=%d, want calls=%d status=%d", calls.Load(), resp.StatusCode, tc.wantCalls, tc.wantStatus)
			}
		})
	}
}

func TestAnonymousCursorSkipsCoolingFixedExitBesidePool(t *testing.T) {
	fixed, rotating := &proxyTransport{}, &proxyTransport{pool: true}
	fixed.healthy.Store(true)
	rotating.healthy.Store(true)
	pool := newAnonymousPool(&transportPool{items: []*proxyTransport{fixed, rotating}}, time.Second, false)
	quota := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}
	pool.MarkFailure(pool.nodes[0], quota, nil)
	pool.MarkFailure(pool.nodes[1], quota, nil)
	if pool.nodes[0].cooldownUntil.Load() == 0 {
		t.Fatal("fixed exit did not enter cooldown")
	}
	if pool.nodes[1].cooldownUntil.Load() != 0 {
		t.Fatal("one pool exit failure cooled down the rotating gateway")
	}
	cursor := pool.CursorFor("")
	for attempt := range 4 {
		if node := cursor.Next(); node != pool.nodes[1] {
			t.Fatalf("attempt %d selected %p, want rotating node %p", attempt+1, node, pool.nodes[1])
		}
	}
}

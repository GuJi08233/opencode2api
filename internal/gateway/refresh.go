package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
)

const (
	proxyHealthCheckURL      = "https://cloudflare.com/cdn-cgi/trace"
	proxyHealthCheckInterval = 15 * time.Minute
	proxyHealthCheckTimeout  = 10 * time.Second
)

func (g *Gateway) syncProxyResult(ctx context.Context, proxy *proxyTransport, status int, err error) bool {
	if proxy == nil || g.anonymous.isRotating(proxy) {
		return false
	}
	if isProxyFailure(err) {
		wasHealthy := proxy.healthy.Swap(false)
		if wasHealthy {
			g.logger.Warn("proxy became unavailable", "component", "proxy", "event", "proxy_unavailable", "proxy", config.RedactURL(proxy.name))
		}
		g.verifyProxyAfterError(ctx, proxy, status)
		return true
	}
	if status >= 200 && status < 600 && err == nil {
		wasHealthy := proxy.healthy.Swap(true)
		if !wasHealthy {
			g.restoreProxy(proxy)
		}
		return false
	}
	if err != nil {
		g.verifyProxyAfterError(ctx, proxy, status)
	}
	return false
}

func (g *Gateway) verifyProxyAfterError(ctx context.Context, proxy *proxyTransport, status int) {
	if !proxy.checking.CompareAndSwap(false, true) {
		return
	}
	checkCtx := context.WithoutCancel(ctx)
	go func() {
		result := g.transports.checkClaimedProxy(checkCtx, proxy, proxyHealthCheckURL, proxyHealthCheckTimeout)
		g.applyProxyHealthResult(result, "upstream HTTP response", status)
	}()
}

func (g *Gateway) restoreProxy(proxy *proxyTransport) {
	if proxy == nil {
		return
	}
	anonCleared := g.anonymous.RestoreProxy(proxy)
	if anonCleared > 0 {
		g.logger.Info("proxy connectivity restored", "component", "proxy", "event", "proxy_restored", "proxy", config.RedactURL(proxy.name), "anonymous_cleared", anonCleared)
	}
}

func (g *Gateway) StartProxyHealthChecks(ctx context.Context) {
	check := func() {
		results := g.transports.CheckHealth(ctx, proxyHealthCheckURL, proxyHealthCheckTimeout)
		for _, result := range results {
			g.applyProxyHealthResult(result, "scheduled health check", 0)
		}
	}
	go func() {
		ticker := time.NewTicker(proxyHealthCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}

func (g *Gateway) applyProxyHealthResult(result proxyHealthResult, source string, upstreamStatus int) {
	if result.err == nil {
		if !result.wasHealthy {
			g.restoreProxy(result.proxy)
		}
		g.logger.Debug("proxy health check passed", "component", "proxy", "event", "health_check_passed", "source", source, "upstream_status", upstreamStatus, "proxy", config.RedactURL(result.proxy.name))
		return
	}
	if !result.failed {
		g.logger.Debug("proxy health check was inconclusive", "component", "proxy", "event", "health_check_inconclusive", "source", source, "upstream_status", upstreamStatus, "proxy", config.RedactURL(result.proxy.name), "error", result.err)
		return
	}
	if g.transports.hasHealthy() && result.wasHealthy {
		g.logger.Warn("proxy health check failed", "component", "proxy", "event", "health_check_failed", "source", source, "upstream_status", upstreamStatus, "proxy", config.RedactURL(result.proxy.name), "error", result.err)
		return
	}
	g.logger.Debug("proxy health check is still failing", "component", "proxy", "event", "health_check_still_failing", "source", source, "upstream_status", upstreamStatus, "proxy", config.RedactURL(result.proxy.name), "error", result.err)
}

func (g *Gateway) StartModelRefresh(ctx context.Context) {
	refresh := func() {
		var zen []string
		var capabilities modelcatalog.Capabilities
		var capabilitiesErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); zen = g.refreshAnonymousTier(ctx) }()
		go func() {
			defer wg.Done()
			capabilityCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			capabilities, capabilitiesErr = g.refreshProtocolCapabilities(capabilityCtx)
		}()
		wg.Wait()
		if ctx.Err() != nil {
			return
		}
		if capabilitiesErr != nil {
			g.logger.Warn("OpenCode capability catalog refresh failed", "component", "models", "event", "capability_refresh_failed", "error", capabilitiesErr)
		}
		if zen != nil {
			g.catalog.ReplaceWithCapabilities(zen, capabilities.Protocols, capabilities.Unsupported, capabilities.Metadata)
			if ctx.Err() == nil {
				if err := g.catalog.SaveCache(); err != nil {
					g.logger.Warn("model catalog cache write failed", "component", "models", "event", "catalog_cache_write_failed", "error", err)
				}
			}
			g.logger.Info("model catalog refreshed", "component", "models", "event", "catalog_refreshed", "models", len(g.catalog.List()))
		}
	}
	go func() {
		refresh()
		ticker := time.NewTicker(time.Duration(g.cfg.Models.RefreshSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

func (g *Gateway) refreshProtocolCapabilities(ctx context.Context) (modelcatalog.Capabilities, error) {
	if g.transports == nil || len(g.transports.items) == 0 {
		return modelcatalog.FetchCapabilities(ctx, &http.Client{Timeout: 30 * time.Second}, modelcatalog.CapabilitiesURL)
	}
	var lastErr error
	for _, proxy := range g.transports.items {
		if proxy == nil || !proxy.healthy.Load() {
			continue
		}
		capabilities, err := modelcatalog.FetchCapabilities(ctx, proxy.client, modelcatalog.CapabilitiesURL)
		if err == nil {
			return capabilities, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy proxy available for OpenCode capability catalog")
	}
	return modelcatalog.Capabilities{}, lastErr
}

func (g *Gateway) refreshAnonymousTier(ctx context.Context) []string {
	cursor := g.anonymous.CursorFor("")
	limit := g.anonymous.Len()
	attemptLimit := g.cfg.Retry.AnonymousMaxAttempts
	if attemptLimit > 0 {
		limit = attemptLimit
	} else if g.anonymous.HasPool() && g.cfg.Retry.MaxAttempts > limit {
		limit = g.cfg.Retry.MaxAttempts
	}
	requests := 0
	for attempt := 1; attempt <= limit && (attemptLimit == 0 || requests < attemptLimit); attempt++ {
		node := cursor.Next()
		if node == nil {
			break
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var models []string
		var status int
		var err error
		for _, upstream := range g.anonymousUpstreams() {
			if attemptLimit > 0 && requests >= attemptLimit {
				break
			}
			requests++
			models, status, err = modelcatalog.FetchModels(refreshCtx, node.proxy.client, upstream, anonymousZenKey)
			if err == nil {
				break
			}
		}
		g.syncProxyResult(refreshCtx, node.proxy, status, err)
		cancel()
		if err == nil {
			g.anonymous.MarkSuccess(node)
			return models
		}
		g.anonymous.MarkFailure(node, nil, err)
		g.logger.Debug("anonymous model catalog refresh attempt failed", "component", "models", "event", "anonymous_refresh_attempt_failed", "attempt", attempt, "proxy", config.RedactURL(node.proxy.name), "error", err)
	}
	g.logger.Warn("anonymous model catalog refresh failed", "component", "models", "event", "anonymous_refresh_failed")
	return nil
}

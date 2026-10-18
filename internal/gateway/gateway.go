// Package gateway routes inference requests through the anonymous free-tier pool.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

const maxRequestBody = 32 << 20

const anonymousZenKey = "public"

var requestBufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 16<<10))
	},
}

func attemptDeadline(cfg config.Config) time.Duration {
	if cfg.Performance.AttemptTimeoutSeconds <= 0 {
		return 0
	}
	return cfg.Performance.AttemptTimeout(time.Duration(cfg.Retry.TimeoutSeconds) * time.Second)
}

const statusClientClosedRequest = 499

const proxyUnavailableMessage = "all proxies are unavailable; the upstream could not be reached"

type Gateway struct {
	cfg            config.Config
	logger         *slog.Logger
	transports     *transportPool
	anonymous      *anonymousPool
	catalog        *models.Catalog
	monitor        *telemetry.Monitor
	availability   *models.AvailabilityStore
	redactor       *config.SecretRedactor
	attemptTimeout time.Duration
}

func New(cfg config.Config, logger *slog.Logger, monitor *telemetry.Monitor, redactor *config.SecretRedactor) (*Gateway, error) {
	transports, err := newTransportPool(cfg.RuntimeProxies(), cfg.Performance, cfg.Performance.AttemptTimeout(time.Duration(cfg.Retry.TimeoutSeconds)*time.Second))
	if err != nil {
		return nil, err
	}
	cooldown := time.Duration(cfg.Performance.FailureCooldownSeconds) * time.Second
	catalog := models.NewCatalog(cfg.Models.Protocols)
	catalog.SetRefreshInterval(time.Duration(cfg.Models.RefreshSeconds) * time.Second)
	return &Gateway{
		cfg:            cfg,
		logger:         logger,
		transports:     transports,
		anonymous:      newAnonymousPool(transports, cooldown, cfg.Retry.AnonymousMaxAttempts > 0),
		catalog:        catalog,
		monitor:        monitor,
		redactor:       redactor,
		attemptTimeout: attemptDeadline(cfg),
	}, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.authenticate(g.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", g.authenticate(g.handleInference(wire.Chat)))
	mux.HandleFunc("POST /v1/responses", g.authenticate(g.handleInference(wire.Responses)))
	mux.HandleFunc("POST /v1/messages", g.authenticate(g.handleInference(wire.Anthropic)))
	mux.HandleFunc("POST /v1/systemone", g.authenticate(g.handleSystemOne))
	mux.HandleFunc("GET /healthz", g.handleHealth)
	return telemetry.Recover(g.logger, mux)
}

func (g *Gateway) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var keyFP string
		var ok bool
		if apiKey := strings.TrimSpace(r.Header.Get("x-api-key")); apiKey != "" {
			keyFP, ok = g.cfg.AuthenticateKey(apiKey)
		}
		if !ok {
			if auth := r.Header.Get("Authorization"); len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
				keyFP, ok = g.cfg.AuthenticateKey(strings.TrimSpace(auth[7:]))
			}
		}
		if !ok {
			protocol := wire.Chat
			if r.URL.Path == "/v1/messages" {
				protocol = wire.Anthropic
			}
			wire.WriteError(w, protocol, http.StatusUnauthorized, "invalid local API key", "authentication_error", "")
			return
		}
		if meta := telemetry.MetaFromRequest(r); meta != nil {
			meta.ServerKeyID = keyFP
		}
		next(w, r)
	}
}

func readRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, func(), error) {
	buf := requestBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	release := func() {
		if buf.Cap() <= 1<<20 {
			requestBufferPool.Put(buf)
		}
	}
	_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		release()
		return nil, nil, err
	}
	return buf.Bytes(), release, nil
}

func (g *Gateway) handleInference(external wire.Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, releaseBody, err := readRequestBody(w, r)
		if err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request_error", "")
			return
		}
		defer releaseBody()

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "")
			return
		}
		model := jsonutil.StringAt(payload, "model")
		meta := telemetry.MetaFromRequest(r)
		if meta != nil {
			meta.Model = model
		}
		if model == "" {
			wire.WriteError(w, external, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
			return
		}
		if !g.catalog.Supported(model) {
			wire.WriteError(w, external, http.StatusBadRequest, "the model uses an upstream protocol that opencode2api does not expose", "invalid_request_error", "model")
			return
		}
		route, err := g.catalog.Route(model)
		if err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
			return
		}
		if meta != nil {
			meta.Protocol = route.Protocol
		}
		if route.Protocol == wire.SystemOne {
			if len(jsonutil.MapAt(payload, "questions")) == 0 || payload["state"] == nil {
				wire.WriteError(w, external, http.StatusBadRequest, "this model uses System One decisions, not chat; send state and typed questions to /v1/systemone", "invalid_request_error", "model")
				return
			}
			g.forwardSystemOne(w, r, append([]byte(nil), body...), payload, model, route)
			return
		}
		preparedBody, err := g.prepareRouteBody(external, route, payload)
		if err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
			return
		}
		ids := identity.DeriveRequestIDs(r, payload)
		if meta != nil {
			meta.Request = ids.Request
			meta.Session = ids.Session
		}
		stream := jsonutil.BoolAt(payload, "stream")
		requestCtx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
		defer cancel()
		resp, err := g.doUpstream(requestCtx, route, preparedBody, ids)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) && wire.ClientCanceled(r.Context(), err) {
				g.logger.Debug("client canceled before upstream response", "component", "upstream", "event", "request_canceled", "request_id", ids.Request)
				if meta != nil {
					meta.Outcome = "client_canceled"
				}
				wire.WriteError(w, external, statusClientClosedRequest, "client closed request", "client_canceled", ids.Request)
				return
			}
			g.logger.Warn("all upstream attempts failed", "component", "upstream", "event", "request_failed", "request_id", ids.Request, "error", err)
			if errors.Is(err, errProxyUnavailable) {
				wire.WriteError(w, external, http.StatusServiceUnavailable, proxyUnavailableMessage, "proxy_error", ids.Request)
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				wire.WriteError(w, external, http.StatusGatewayTimeout, "upstream request timed out", "upstream_timeout", ids.Request)
				return
			}
			wire.WriteError(w, external, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("x-request-id", ids.Request)
		if resp.StatusCode/100 != 2 {
			copyErrorResponse(w, external, resp, ids.Request)
			return
		}
		var toolAlias string
		if meta != nil {
			toolAlias = meta.ToolAlias
		}
		if stream {
			if meta != nil {
				meta.Stream = true
			}
			if g.monitor != nil {
				g.monitor.BeginStream()
				defer g.monitor.EndStream()
			}
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(resp.StatusCode)
			streamBody := wire.RenameStreamReader(resp.Body, route.Protocol, legacyAnonymousReadTool, toolAlias)
			var usage wire.Usage
			var usageReported bool
			if external == route.Protocol {
				usage, usageReported, err = wire.ForwardStream(r.Context(), w, streamBody, route.Protocol, model)
			} else {
				usage, usageReported, err = wire.TranscodeStream(r.Context(), w, streamBody, route.Protocol, external, model)
			}
			if meta != nil {
				meta.Usage, meta.UsageReported = usage, usageReported
				if err != nil {
					meta.Outcome = "stream_error"
					if wire.ClientCanceled(r.Context(), err) {
						meta.Outcome = "client_canceled"
					}
				}
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Warn("downstream stream ended with an error", "component", "stream", "event", "stream_failed", "request_id", ids.Request, "model", model, "error", err)
			}
			return
		}
		responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			wire.WriteError(w, external, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
			return
		}
		collapsed, err := wire.CollapseStream(bytes.NewReader(responseBody), route.Protocol, model)
		if err != nil {
			g.logger.Warn("anonymous stream collapse failed", "component", "conversion", "event", "anonymous_collapse_failed", "request_id", ids.Request, "model", model, "source_protocol", route.Protocol, "error", err)
			wire.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
			return
		}
		responseBody = wire.RenameResponseTool(collapsed, route.Protocol, legacyAnonymousReadTool, toolAlias)
		if usage, reported := wire.ResponseUsage(route.Protocol, responseBody); meta != nil {
			meta.Usage, meta.UsageReported = usage, reported
		}
		if external != route.Protocol {
			responseBody, err = wire.ConvertResponse(route.Protocol, external, responseBody)
			if err != nil {
				g.logger.Warn("response protocol conversion failed", "component", "conversion", "event", "response_conversion_failed", "request_id", ids.Request, "model", model, "source_protocol", route.Protocol, "target_protocol", external, "error", err)
				wire.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
	}
}

func (g *Gateway) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	body, releaseBody, err := readRequestBody(w, r)
	if err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request_error", "")
		return
	}
	defer releaseBody()

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "")
		return
	}
	model := jsonutil.StringAt(payload, "model")
	if meta := telemetry.MetaFromRequest(r); meta != nil {
		meta.Model = model
	}
	if model == "" {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
		return
	}
	if !g.catalog.Supported(model) {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "the model uses an upstream protocol that opencode2api does not expose", "invalid_request_error", "model")
		return
	}
	route, err := g.catalog.Route(model)
	if err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
		return
	}
	if route.Protocol != wire.SystemOne {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, fmt.Sprintf("the model does not use the %s protocol", wire.SystemOne), "invalid_request_error", "model")
		return
	}
	g.forwardSystemOne(w, r, append([]byte(nil), body...), payload, model, route)
}

func (g *Gateway) forwardSystemOne(w http.ResponseWriter, r *http.Request, body []byte, payload map[string]any, model string, route models.Route) {
	meta := telemetry.MetaFromRequest(r)
	if meta != nil {
		meta.Model = model
		meta.Protocol = route.Protocol
	}
	ids := identity.DeriveRequestIDs(r, payload)
	if meta != nil {
		meta.Request = ids.Request
		meta.Session = ids.Session
	}
	requestCtx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
	defer cancel()
	resp, err := g.doUpstream(requestCtx, route, body, ids)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && wire.ClientCanceled(r.Context(), err) {
			g.logger.Debug("client canceled before upstream response", "component", "upstream", "event", "request_canceled", "request_id", ids.Request)
			if meta != nil {
				meta.Outcome = "client_canceled"
			}
			wire.WriteError(w, wire.SystemOne, statusClientClosedRequest, "client closed request", "client_canceled", ids.Request)
			return
		}
		g.logger.Warn("all upstream attempts failed", "component", "upstream", "event", "request_failed", "request_id", ids.Request, "error", err)
		if errors.Is(err, errProxyUnavailable) {
			wire.WriteError(w, wire.SystemOne, http.StatusServiceUnavailable, proxyUnavailableMessage, "proxy_error", ids.Request)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			wire.WriteError(w, wire.SystemOne, http.StatusGatewayTimeout, "upstream request timed out", "upstream_timeout", ids.Request)
			return
		}
		wire.WriteError(w, wire.SystemOne, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("x-request-id", ids.Request)
	if resp.StatusCode/100 != 2 {
		copyErrorResponse(w, wire.SystemOne, resp, ids.Request)
		return
	}
	if contentType := resp.Header.Get("Content-Type"); strings.HasPrefix(contentType, "text/event-stream") {
		if meta != nil {
			meta.Stream = true
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
		return
	}
	if usage, reported := wire.ResponseUsage(route.Protocol, responseBody); meta != nil {
		meta.Usage, meta.UsageReported = usage, reported
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}

func (g *Gateway) prepareRouteBody(from wire.Protocol, route models.Route, input map[string]any) ([]byte, error) {
	upstreamPayload, err := wire.PrepareRequest(from, route.Protocol, input, g.cfg.Upstream.Zen)
	if err != nil {
		return nil, fmt.Errorf("prepare upstream request: %w", err)
	}
	if effort := g.resolvedEffort(jsonutil.StringAt(upstreamPayload, "model"), route.Protocol); effort != "" {
		wire.ForcedEffort(route.Protocol, upstreamPayload, effort)
	}
	encoded, err := json.Marshal(upstreamPayload)
	if err != nil {
		return nil, errors.New("request contains unsupported JSON values")
	}
	return encoded, nil
}

func copyErrorResponse(w http.ResponseWriter, protocol wire.Protocol, resp *http.Response, requestID string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	message := http.StatusText(resp.StatusCode)
	var value map[string]any
	if json.Unmarshal(body, &value) == nil {
		message = jsonutil.FirstString(jsonutil.StringAt(value, "error", "message"), jsonutil.StringAt(value, "message"), message)
	}
	wire.WriteError(w, protocol, resp.StatusCode, message, "upstream_error", requestID)
}

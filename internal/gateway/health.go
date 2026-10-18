package gateway

import (
	"net/http"
	"slices"
	"time"

	"opencode2api/internal/buildinfo"
	"opencode2api/internal/httpx"
	modelcatalog "opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

type healthResponse struct {
	Status  string        `json:"status"`
	Ready   bool          `json:"ready"`
	Version string        `json:"version"`
	Models  healthModels  `json:"models"`
	Proxies healthProxies `json:"proxies"`
	Issues  []string      `json:"issues,omitempty"`
}

type healthModels struct {
	Status            string     `json:"status"`
	Total             int        `json:"total"`
	Exposed           int        `json:"exposed"`
	LastRefresh       *time.Time `json:"last_refresh,omitempty"`
	StaleAfterSeconds int        `json:"stale_after_seconds"`
	CacheSource       string     `json:"cache_source,omitempty"`
	Stale             bool       `json:"stale"`
}

type healthProxies struct {
	Total     int `json:"total"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
}

func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	_, models := g.availableModels()
	proxyTotal, proxyHealthy := g.transports.healthCounts()
	staleAfter := max(2*time.Duration(g.cfg.Models.RefreshSeconds)*time.Second, time.Minute)

	modelStatus := "ready"
	var lastRefresh *time.Time
	issues := make([]string, 0, 2)
	if models.UpdatedAt.IsZero() {
		modelStatus = "pending"
		issues = append(issues, "model_catalog_pending")
	} else {
		updatedAt := models.UpdatedAt.UTC()
		lastRefresh = &updatedAt
		if models.Exposed == 0 {
			modelStatus = "empty"
			issues = append(issues, "model_catalog_empty")
		} else if models.Stale || time.Since(models.UpdatedAt) > staleAfter {
			modelStatus = "stale"
			issues = append(issues, "model_catalog_stale")
		}
	}
	if proxyHealthy == 0 {
		issues = append(issues, "no_healthy_proxies")
	}

	status := "ok"
	if len(issues) > 0 {
		status = "degraded"
	}
	blocking := modelStatus == "pending" || modelStatus == "empty" || proxyHealthy == 0
	httpStatus := http.StatusOK
	ready := !blocking
	if blocking {
		httpStatus = http.StatusServiceUnavailable
		if modelStatus == "pending" {
			status = "starting"
		}
	}
	httpx.WriteJSON(w, httpStatus, healthResponse{
		Status:  status,
		Ready:   ready,
		Version: buildinfo.Version,
		Models: healthModels{
			Status:            modelStatus,
			Total:             models.Total,
			Exposed:           models.Exposed,
			LastRefresh:       lastRefresh,
			StaleAfterSeconds: int(staleAfter / time.Second),
			CacheSource:       models.CacheSource,
			Stale:             models.Stale,
		},
		Proxies: healthProxies{
			Total:     proxyTotal,
			Healthy:   proxyHealthy,
			Unhealthy: proxyTotal - proxyHealthy,
		},
		Issues: issues,
	})
}

func (g *Gateway) availableModels() ([]modelcatalog.Route, modelcatalog.CatalogSnapshot) {
	return g.catalog.AvailableModels()
}

var reasoningLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

func (g *Gateway) reasoningEfforts(model string, route modelcatalog.Route) []string {
	if g.cfg.ForcedEffort(model) == "auto" {
		if ceiling := slices.Index(reasoningLevels, g.resolvedEffort(model, route.Protocol)); ceiling >= 0 {
			return slices.Clone(reasoningLevels[:ceiling+1])
		}
	}
	return slices.Clone(reasoningLevels)
}

func (g *Gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	routes, _ := g.availableModels()
	data := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		model := route.ID
		md := g.catalog.MetadataFor(model)
		entry := map[string]any{
			"id": model, "object": "model", "created": now, "owned_by": "opencode",
			"metadata":        md,
			"native_protocol": route.Protocol,
		}
		if route.Protocol == wire.SystemOne {
			entry["supported_endpoints"] = []string{"/v1/systemone"}
		} else {
			entry["supported_endpoints"] = []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"}
		}
		if md.ContextWindow > 0 {
			entry["context_window"] = md.ContextWindow
			entry["context_length"] = md.ContextWindow
		}
		if md.MaxInput > 0 {
			entry["max_input"] = md.MaxInput
		}
		if md.MaxOutput > 0 {
			entry["max_output"] = md.MaxOutput
		}
		if md.Reasoning {
			entry["reasoning"] = true
			entry["supports_reasoning"] = true
			efforts := g.reasoningEfforts(model, route)
			entry["reasoning_efforts"] = efforts
			entry["compat"] = map[string]any{"supportsReasoningEffort": true, "supportedReasoningEfforts": efforts}
			if effort := g.resolvedEffort(model, route.Protocol); effort != "" && effort != "none" {
				entry["default_reasoning_effort"] = effort
			}
		}
		if md.Cost != nil {
			cost := map[string]any{}
			for _, field := range []struct {
				names []string
				value *float64
			}{
				{[]string{"input"}, md.Cost.Input},
				{[]string{"output"}, md.Cost.Output},
				{[]string{"cache_read", "cacheRead"}, md.Cost.CacheRead},
				{[]string{"cache_write", "cacheWrite"}, md.Cost.CacheWrite},
			} {
				if field.value == nil {
					continue
				}
				for _, name := range field.names {
					cost[name] = *field.value
				}
			}
			entry["cost"] = cost
		}
		if md.ToolCall {
			entry["tool_call"] = true
		}
		if md.StructuredOutput {
			entry["structured_output"] = true
		}
		data = append(data, entry)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

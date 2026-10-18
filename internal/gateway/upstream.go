package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

func (g *Gateway) doUpstream(ctx context.Context, route models.Route, body []byte, ids identity.RequestIDs) (*http.Response, error) {
	g.dumpOutboundBody(route, body, ids, 0)
	resp, err, attempts := g.doAnonymousUpstream(ctx, route, body, ids, 0)
	if err != nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		return resp, err
	}
	origBody := resp.Body
	errBody, readErr := io.ReadAll(io.LimitReader(origBody, 1<<20))
	httpx.DrainAndClose(origBody)
	if readErr != nil {
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		return resp, nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(errBody))
	if len(errBody) > 0 {
		snippet := string(errBody)
		if g.redactor != nil {
			snippet = g.redactor.String(snippet)
		}
		if len(snippet) > 240 {
			snippet = snippet[:240]
		}
		g.logger.Info("upstream error returned or retried", "component", "upstream", "event", "upstream_error_body", "request_id", ids.Request, "model", route.ID, "status", resp.StatusCode, "snippet", snippet)
	}
	if !isStaleReasoningReference(errBody) {
		return resp, nil
	}
	stripped, changed := stripStaleReasoningInput(route, body)
	if !changed {
		return resp, nil
	}
	if budget := g.cfg.Retry.AnonymousMaxAttempts; budget > 0 && attempts >= budget {
		return resp, nil
	}
	g.logger.Info("retrying upstream without stale reasoning references", "component", "upstream", "event", "reasoning_reference_retry", "request_id", ids.Request, "model", route.ID, "attempt_offset", attempts)
	g.dumpOutboundBody(route, stripped, ids, attempts)
	retryResp, retryErr, _ := g.doAnonymousUpstream(ctx, route, stripped, ids, attempts)
	if retryErr != nil || retryResp == nil || retryResp.StatusCode/100 != 2 {
		retryStatus := 0
		if retryResp != nil {
			retryStatus = retryResp.StatusCode
			httpx.DrainAndClose(retryResp.Body)
		}
		g.logger.Warn("reasoning reference retry failed; returning original error", "component", "upstream", "event", "reasoning_reference_retry_failed", "request_id", ids.Request, "model", route.ID, "error", retryErr, "retry_status", retryStatus)
		fallback := *resp
		fallback.Body = io.NopCloser(bytes.NewReader(errBody))
		return &fallback, nil
	}
	return retryResp, nil
}

func isStaleReasoningReference(body []byte) bool {
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "reasoning item") && !strings.Contains(text, "reasoning reference") {
		return false
	}
	for _, marker := range []string{"not found", "expir", "does not exist", "no longer"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func stripStaleReasoningInput(route models.Route, body []byte) ([]byte, bool) {
	if len(body) == 0 || route.Protocol != wire.Responses {
		return body, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	changed := false
	if _, ok := payload["previous_response_id"]; ok {
		delete(payload, "previous_response_id")
		changed = true
	}
	if raw, ok := payload["input"].([]any); ok {
		kept := make([]any, 0, len(raw))
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok && jsonutil.StringAt(m, "type") == "reasoning" {
				changed = true
				continue
			}
			kept = append(kept, item)
		}
		if changed {
			payload["input"] = kept
		}
	}
	if !changed {
		return body, false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return encoded, true
}

// doAnonymousUpstream prefers the legacy /zen/v1 lane before /inference.
// Fixed exits are visited once and rotating gateways may be revisited. A
// positive anonymous_max_attempts caps actual HTTP requests across both lanes;
// zero retains the upstream proxy-visit budget. No released stream is replayed.
func (g *Gateway) doAnonymousUpstream(ctx context.Context, route models.Route, rawBody []byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, error, int) {
	if len(rawBody) == 0 {
		return nil, errors.New("no prepared request body"), 0
	}
	var lastResponse *http.Response
	var lastErr error
	cursor := g.anonymous.CursorFor(ids.Session)
	limit := g.anonymous.Len()
	attemptLimit := g.cfg.Retry.AnonymousMaxAttempts
	if attemptLimit > 0 {
		attemptLimit -= attemptOffset
		if attemptLimit <= 0 {
			return nil, errors.New("anonymous request attempt budget exhausted"), 0
		}
		limit = attemptLimit
	} else if g.anonymous.HasPool() && g.cfg.Retry.MaxAttempts > limit {
		limit = g.cfg.Retry.MaxAttempts
	}
	proxies := 0
	attempts := 0
	protocol := route.Protocol
	upstreams := g.anonymousUpstreams()
	preparedBodies := make([][]byte, len(upstreams))
	toolAliases := make([]string, len(upstreams))
	for i, baseURL := range upstreams {
		preparedBodies[i], toolAliases[i] = prepareAnonymousBody(rawBody, protocol, isLegacyZenUpstream(baseURL))
	}
	for proxies < limit && (attemptLimit == 0 || attempts < attemptLimit) {
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		node := cursor.Next()
		if node == nil {
			break
		}
		proxies++
		var nodeResp *http.Response
		var nodeErr error
		for index, baseURL := range upstreams {
			if attemptLimit > 0 && attempts >= attemptLimit {
				break
			}
			if ctx.Err() != nil {
				if lastErr == nil && lastResponse == nil {
					lastErr = ctx.Err()
				}
				break
			}
			attempts++
			if meta := telemetry.MetaFromContext(ctx); meta != nil {
				meta.Attempts = attemptOffset + attempts
				meta.ToolAlias = toolAliases[index]
				if node.proxy != nil {
					meta.Proxy = config.RedactURL(node.proxy.name)
				}
			}
			if lastResponse != nil {
				httpx.DrainAndClose(lastResponse.Body)
				lastResponse = nil
			}
			req, err := newUpstreamRequest(ctx, baseURL, protocol, preparedBodies[index], ids, anonymousZenKey)
			if err != nil {
				return nil, err, attempts
			}
			if node.proxy.freshConnections {
				req.Close = true
			}
			started := time.Now()
			resp, err := doInferenceAttempt(node.proxy.client, req, time.Duration(g.cfg.Performance.FirstEventTimeoutSeconds)*time.Second, g.attemptTimeout)
			duration := time.Since(started)
			if ctx.Err() != nil {
				lastResponse, lastErr = resp, err
				if lastErr == nil && lastResponse == nil {
					lastErr = ctx.Err()
				}
				break
			}
			nodeResp, nodeErr = resp, err
			g.recordUpstreamAttempt(ctx, route, resp, err, duration)
			if err == nil && resp.StatusCode/100 == 2 {
				g.observeAnonymousResult(ctx, node, resp, nil)
				g.logger.Debug("anonymous upstream accepted request", "component", "upstream", "event", "anonymous_attempt_succeeded", "request_id", ids.Request, "attempt", attempts, "upstream", config.RedactURL(baseURL), "proxy", config.RedactURL(node.proxy.name), "status", resp.StatusCode, "duration_ms", duration.Milliseconds())
				return resp, nil, attempts
			}
			if attemptLimit > 0 && isNonRetryableClientResponse(resp, err) {
				// Missing legacy endpoints may still be served by /inference.
				// Other client errors describe the request and must not be replayed.
				fallback := isLegacyZenUpstream(baseURL) && index+1 < len(upstreams) &&
					(resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed)
				if !fallback {
					g.observeAnonymousResult(ctx, node, resp, nil)
					return resp, nil, attempts
				}
			}
			lastResponse = resp
			lastErr = asProxyUnavailable(node.proxy, err)
			if err != nil {
				g.logger.Debug("anonymous transport attempt failed", "component", "upstream", "event", "anonymous_attempt_transport_failed", "request_id", ids.Request, "attempt", attempts, "upstream", config.RedactURL(baseURL), "proxy", config.RedactURL(node.proxy.name), "duration_ms", duration.Milliseconds(), "error", err)
				if isProxyFailure(err) {
					break
				}
			} else {
				g.logger.Debug("anonymous upstream returned an error response; trying the next upstream or proxy", "component", "upstream", "event", "anonymous_attempt_response_failed", "request_id", ids.Request, "attempt", attempts, "upstream", config.RedactURL(baseURL), "proxy", config.RedactURL(node.proxy.name), "status", resp.StatusCode, "duration_ms", duration.Milliseconds())
			}
			if index+1 < len(upstreams) && !node.proxy.healthy.Load() {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
		g.observeAnonymousResult(ctx, node, nodeResp, nodeErr)
	}
	if lastResponse != nil {
		return lastResponse, nil, attempts
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy anonymous proxies available")
		if !g.transports.hasHealthy() {
			lastErr = fmt.Errorf("no healthy anonymous proxies available: %w", errProxyUnavailable)
		}
	}
	return nil, lastErr, attempts
}

// anonymousUpstreams returns the ordered base URLs for the keyless free-tier
// lane: the legacy /zen/v1 origin (Upstream.Zen, defaulting to LegacyZenBase)
// is tried first, and the /inference origin (Upstream.Anonymous) is kept as a
// fallback.
func (g *Gateway) anonymousUpstreams() []string {
	legacy := strings.TrimRight(g.cfg.Upstream.Zen, "/")
	if legacy == "" {
		legacy = strings.TrimRight(models.LegacyZenBase, "/")
	}
	primary := strings.TrimRight(g.cfg.Upstream.Anonymous, "/")
	if primary == "" || strings.EqualFold(legacy, primary) {
		return []string{legacy}
	}
	return []string{legacy, g.cfg.Upstream.Anonymous}
}

func isLegacyZenUpstream(baseURL string) bool {
	trimmed := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if strings.EqualFold(trimmed, strings.TrimRight(models.LegacyZenBase, "/")) {
		return true
	}
	return strings.HasSuffix(trimmed, "/zen")
}

const legacyAnonymousReadTool = "read"

const legacyDeprecatedReadDescription = "DEPRECATED: This tool is deprecated and disabled. Do NOT call or reference this tool under any circumstances; ignore it completely."

var anonymousCoreTools = []string{"shell", "edit", "glob", "grep", "read"}

//go:embed opencode_tools.json
var opencodeToolsJSON []byte

type opencodeToolDefinition struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

var (
	opencodeToolsOnce sync.Once
	opencodeTools     map[string]opencodeToolDefinition
)

func knownOpencodeTool(name string) (opencodeToolDefinition, bool) {
	opencodeToolsOnce.Do(func() {
		definitions := map[string]opencodeToolDefinition{}
		if err := json.Unmarshal(opencodeToolsJSON, &definitions); err != nil {
			definitions = map[string]opencodeToolDefinition{}
		}
		opencodeTools = definitions
	})
	definition, ok := opencodeTools[name]
	return definition, ok
}

func prepareAnonymousBody(body []byte, protocol wire.Protocol, legacy bool) ([]byte, string) {
	if protocol == wire.SystemOne {
		return body, ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, ""
	}
	changed := false
	if streaming, ok := payload["stream"].(bool); !ok || !streaming {
		payload["stream"] = true
		changed = true
	}
	if ensureAnonymousChatUsage(payload, protocol) {
		changed = true
	}
	var alias string
	if legacy {
		var toolChanged bool
		alias, toolChanged = ensureLegacyAnonymousReadTool(payload, protocol)
		if toolChanged {
			changed = true
		}
	} else if ensureAnonymousTools(payload, protocol) {
		changed = true
	}
	if !changed {
		return body, alias
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body, ""
	}
	return encoded, alias
}

func ensureAnonymousChatUsage(payload map[string]any, protocol wire.Protocol) bool {
	if protocol != wire.Chat {
		return false
	}
	options, ok := payload["stream_options"].(map[string]any)
	if !ok {
		payload["stream_options"] = map[string]any{"include_usage": true}
		return true
	}
	includeUsage, ok := options["include_usage"].(bool)
	if ok && includeUsage {
		return false
	}
	options["include_usage"] = true
	return true
}

func ensureLegacyAnonymousReadTool(payload map[string]any, protocol wire.Protocol) (string, bool) {
	raw, exists := payload["tools"]
	if !exists {
		payload["tools"] = []any{legacyDeprecatedReadTool(protocol)}
		return "", true
	}
	items, ok := raw.([]any)
	if !ok {
		return "", false
	}
	candidateIndex := -1
	candidateName := ""
	for index, item := range items {
		name := anonymousToolName(protocol, item)
		if name == "" {
			continue
		}
		if name == legacyAnonymousReadTool {
			return "", false
		}
		if strings.EqualFold(name, legacyAnonymousReadTool) {
			candidateIndex = index
			candidateName = name
			break
		}
		if candidateIndex < 0 && isLegacyReadToolCandidate(name) {
			candidateIndex = index
			candidateName = name
		}
	}
	if candidateIndex >= 0 {
		setAnonymousToolName(protocol, items[candidateIndex], legacyAnonymousReadTool)
		rewriteLegacyRequestToolReferences(payload, protocol, candidateName, legacyAnonymousReadTool)
		payload["tools"] = items
		return candidateName, true
	}
	items = append(items, legacyDeprecatedReadTool(protocol))
	payload["tools"] = items
	return "", true
}

func isLegacyReadToolCandidate(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	if lower == "read" {
		return true
	}
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(lower)
	switch normalized {
	case "readfile", "fileread", "readfiles", "readtext", "readtextfile", "readcontent", "viewfile":
		return true
	}
	return strings.HasPrefix(lower, "read_") || strings.HasPrefix(lower, "read-") || strings.HasSuffix(lower, "_read") || strings.HasSuffix(lower, "-read")
}

func setAnonymousToolName(protocol wire.Protocol, item any, name string) {
	entry, ok := item.(map[string]any)
	if !ok {
		return
	}
	if protocol == wire.Chat {
		if function, ok := entry["function"].(map[string]any); ok {
			function["name"] = name
		}
		return
	}
	entry["name"] = name
}

func rewriteLegacyRequestToolReferences(payload map[string]any, protocol wire.Protocol, fromName, toName string) {
	if fromName == "" || fromName == toName {
		return
	}
	switch protocol {
	case wire.Chat:
		if choice, ok := payload["tool_choice"].(map[string]any); ok {
			if function, ok := choice["function"].(map[string]any); ok && jsonutil.StringAt(function, "name") == fromName {
				function["name"] = toName
			}
		}
		for _, rawMsg := range jsonutil.SliceAt(payload, "messages") {
			msg, _ := rawMsg.(map[string]any)
			for _, rawCall := range jsonutil.SliceAt(msg, "tool_calls") {
				call, _ := rawCall.(map[string]any)
				if function, ok := call["function"].(map[string]any); ok && jsonutil.StringAt(function, "name") == fromName {
					function["name"] = toName
				}
			}
		}
	case wire.Responses:
		if choice, ok := payload["tool_choice"].(map[string]any); ok && jsonutil.StringAt(choice, "name") == fromName {
			choice["name"] = toName
		}
		for _, rawItem := range jsonutil.SliceAt(payload, "input") {
			item, _ := rawItem.(map[string]any)
			if jsonutil.StringAt(item, "type") == "function_call" && jsonutil.StringAt(item, "name") == fromName {
				item["name"] = toName
			}
		}
	case wire.Anthropic:
		if choice, ok := payload["tool_choice"].(map[string]any); ok && jsonutil.StringAt(choice, "name") == fromName {
			choice["name"] = toName
		}
		for _, rawMsg := range jsonutil.SliceAt(payload, "messages") {
			msg, _ := rawMsg.(map[string]any)
			for _, rawBlock := range jsonutil.SliceAt(msg, "content") {
				block, _ := rawBlock.(map[string]any)
				if jsonutil.StringAt(block, "type") == "tool_use" && jsonutil.StringAt(block, "name") == fromName {
					block["name"] = toName
				}
			}
		}
	}
}

func legacyDeprecatedReadTool(protocol wire.Protocol) map[string]any {
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	switch protocol {
	case wire.Chat:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        legacyAnonymousReadTool,
				"description": legacyDeprecatedReadDescription,
				"parameters":  parameters,
			},
		}
	case wire.Anthropic:
		return map[string]any{
			"name":         legacyAnonymousReadTool,
			"description":  legacyDeprecatedReadDescription,
			"input_schema": parameters,
		}
	default:
		return map[string]any{
			"type":        "function",
			"name":        legacyAnonymousReadTool,
			"description": legacyDeprecatedReadDescription,
			"parameters":  parameters,
		}
	}
}

func ensureAnonymousTools(payload map[string]any, protocol wire.Protocol) bool {
	raw, exists := payload["tools"]
	if !exists {
		payload["tools"] = anonymousToolset(protocol, anonymousCoreTools)
		return true
	}
	items, ok := raw.([]any)
	if !ok {
		return false
	}
	changed := false
	present := make(map[string]bool, len(items))
	for index, item := range items {
		name := anonymousToolName(protocol, item)
		if name == "" {
			continue
		}
		present[name] = true
		if _, known := knownOpencodeTool(name); known {
			items[index] = anonymousTool(protocol, name)
			changed = true
		}
	}
	missing := make([]string, 0, len(anonymousCoreTools))
	for _, name := range anonymousCoreTools {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		items = append(items, anonymousToolset(protocol, missing)...)
		changed = true
	}
	if !changed {
		return false
	}
	payload["tools"] = items
	return true
}

func anonymousToolName(protocol wire.Protocol, item any) string {
	entry, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	if protocol == wire.Chat {
		return jsonutil.StringAt(entry, "function", "name")
	}
	return jsonutil.StringAt(entry, "name")
}

func anonymousToolset(protocol wire.Protocol, names []string) []any {
	if names == nil {
		names = anonymousCoreTools
	}
	tools := make([]any, 0, len(names))
	for _, name := range names {
		if tool := anonymousTool(protocol, name); tool != nil {
			tools = append(tools, tool)
		}
	}
	return tools
}

func anonymousTool(protocol wire.Protocol, name string) map[string]any {
	description := "Agent tool " + name
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	if definition, ok := knownOpencodeTool(name); ok {
		description = definition.Description
		if definition.Parameters != nil {
			parameters = definition.Parameters
		}
	}
	switch protocol {
	case wire.Chat:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": description,
				"parameters":  parameters,
			},
		}
	case wire.Anthropic:
		return map[string]any{
			"name":         name,
			"description":  description,
			"input_schema": parameters,
		}
	default:
		return map[string]any{
			"type":        "function",
			"name":        name,
			"description": description,
			"parameters":  parameters,
		}
	}
}

const requestBodyDumpLimit = 64 << 10

func (g *Gateway) dumpOutboundBody(route models.Route, body []byte, ids identity.RequestIDs, attemptOffset int) {
	if !g.cfg.Logging.DumpRequestBodies || len(body) == 0 {
		return
	}
	phase := "initial"
	if attemptOffset > 0 {
		phase = "retry"
	}
	sum := sha256.Sum256(body)
	truncated := len(body) > requestBodyDumpLimit
	payload := body
	if truncated {
		payload = body[:requestBodyDumpLimit]
	}
	g.logger.Debug("outbound upstream request body", "component", "upstream", "event", "upstream_request_body",
		"request_id", ids.Request, "model", route.ID, "protocol", route.Protocol,
		"phase", phase, "bytes", len(body), "sha256", hex.EncodeToString(sum[:8]),
		"truncated", truncated, "body", string(payload))
}

func (g *Gateway) observeAnonymousResult(ctx context.Context, node *anonymousNode, resp *http.Response, err error) {
	status := upstreamStatus(resp)
	g.syncProxyResult(ctx, node.proxy, status, err)
	if err == nil && status/100 == 2 {
		g.anonymous.MarkSuccess(node)
	} else {
		g.anonymous.MarkFailure(node, resp, err)
	}
}

func upstreamStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func (g *Gateway) recordUpstreamAttempt(ctx context.Context, route models.Route, resp *http.Response, err error, duration time.Duration) {
	status := upstreamStatus(resp)
	success := err == nil && status >= 200 && status < 300
	outcome := "retryable_failure"
	if success {
		outcome = "success"
	} else if err != nil {
		outcome = "transport_error"
	} else if isNonRetryableClientResponse(resp, nil) {
		outcome = "rejected"
	}
	if meta := telemetry.MetaFromContext(ctx); meta != nil {
		meta.AttemptOutcome = outcome
		meta.Protocol = route.Protocol
		leg := "anon:"
		if err != nil {
			leg += classifyAttemptError(err)
		} else {
			leg += strconv.Itoa(status)
		}
		leg += ":" + strconv.FormatInt(max(duration.Milliseconds(), 0), 10) + "ms"
		meta.Legs = append(meta.Legs, leg)
	}
	if g.monitor != nil {
		g.monitor.RecordAttempt(success)
	}
}

func newUpstreamRequest(ctx context.Context, baseURL string, protocol wire.Protocol, body []byte, ids identity.RequestIDs, key string) (*http.Request, error) {
	endpoint := strings.TrimRight(baseURL, "/") + wire.Path(protocol)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", ids.Session)
	req.Header.Set("x-session-affinity", ids.Session)
	req.Header.Set("X-Session-Id", ids.Session)
	if isLegacyZenUpstream(baseURL) {
		req.Header.Set("User-Agent", httpx.LegacyUserAgent())
		req.Header.Set("x-opencode-request", ids.Request)
		req.Header.Set("x-opencode-project", legacyProjectID(ids.Project))
	} else {
		req.Header.Set("User-Agent", httpx.UserAgent())
		req.Header.Set("x-opencode-session-id", ids.Session)
		req.Header.Set("x-opencode-project", clientProjectID(ids.Project))
	}
	if ids.ParentSession != "" {
		req.Header.Set("x-parent-session-id", ids.ParentSession)
	}
	if protocol == wire.Anthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

func legacyProjectID(project string) string {
	if strings.HasPrefix(project, "prj_") {
		return project
	}
	return identity.StableID("prj", project)
}

func clientProjectID(project string) string {
	sum := sha256.Sum256([]byte("opencode-project\x00" + project))
	return hex.EncodeToString(sum[:20])
}

func isNonRetryableClientResponse(resp *http.Response, err error) bool {
	return err == nil && resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests
}

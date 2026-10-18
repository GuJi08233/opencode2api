package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"opencode2api/internal/httpx"
	wire "opencode2api/internal/protocol"
)

const (
	CapabilitiesURL = "https://models.opencode.ai/api.json"
	ZenDocsURL      = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/zen.mdx"
	// LegacyZenBase is the legacy /zen console origin (serving /zen/v1/*) that
	// still serves anonymous inference and the models listing alongside the newer
	// /inference backend.
	LegacyZenBase = "https://opencode.ai/zen"
)

var protocolDocEndpointPattern = regexp.MustCompile("\\|[^|]+\\|\\s*`?([^|`\\s]+)`?\\s*\\|\\s*`[^`]+/v1/(chat/completions|responses|messages|systemone)`")

type Capabilities struct {
	Protocols   map[string]wire.Protocol
	Unsupported map[string]bool
	Metadata    map[string]Metadata
}

type capabilityProvider struct {
	ID     string                     `json:"id"`
	API    string                     `json:"api"`
	NPM    string                     `json:"npm"`
	Models map[string]capabilityModel `json:"models"`
}

type capabilityModel struct {
	ID               string                     `json:"id"`
	Provider         *capabilityModelProvider   `json:"provider"`
	Limit            *capabilityModelLimit      `json:"limit"`
	Reasoning        bool                       `json:"reasoning"`
	ToolCall         bool                       `json:"tool_call"`
	StructuredOutput bool                       `json:"structured_output"`
	Modalities       *capabilityModelModalities `json:"modalities"`
}

type capabilityModelModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type capabilityModelLimit struct {
	Context int `json:"context"`
	Input   int `json:"input"`
	Output  int `json:"output"`
}

type Metadata struct {
	ContextWindow    int      `json:"context_window,omitempty"`
	MaxInput         int      `json:"max_input,omitempty"`
	MaxOutput        int      `json:"max_output,omitempty"`
	Reasoning        bool     `json:"reasoning,omitempty"`
	ToolCall         bool     `json:"tool_call,omitempty"`
	StructuredOutput bool     `json:"structured_output,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	Cost             *Cost    `json:"cost,omitempty"`
}

type Cost struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

func (m *capabilityModel) metadata() Metadata {
	md := Metadata{
		Reasoning:        m.Reasoning,
		ToolCall:         m.ToolCall,
		StructuredOutput: m.StructuredOutput,
	}
	if m.Modalities != nil {
		md.InputModalities = m.Modalities.Input
		md.OutputModalities = m.Modalities.Output
	}
	if m.Limit != nil {
		md.ContextWindow = m.Limit.Context
		md.MaxInput = m.Limit.Input
		md.MaxOutput = m.Limit.Output
	}
	return md
}

type capabilityModelProvider struct {
	NPM string `json:"npm"`
}

func FetchCapabilities(ctx context.Context, client *http.Client, endpoint string) (Capabilities, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Capabilities{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httpx.UserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return Capabilities{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Capabilities{}, fmt.Errorf("OpenCode capability endpoint returned HTTP %d", resp.StatusCode)
	}
	var providers map[string]capabilityProvider
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	if err := dec.Decode(&providers); err != nil {
		return Capabilities{}, err
	}
	result := Capabilities{
		Protocols:   map[string]wire.Protocol{},
		Unsupported: map[string]bool{},
		Metadata:    map[string]Metadata{},
	}
	for providerID, provider := range providers {
		if !isZenProvider(providerID, provider.API) {
			continue
		}
		for modelID, model := range provider.Models {
			if model.ID != "" {
				modelID = model.ID
			}
			npm := provider.NPM
			if model.Provider != nil && model.Provider.NPM != "" {
				npm = model.Provider.NPM
			}
			if protocol, ok := protocolForSDK(npm); ok {
				result.Protocols[modelID] = protocol
			} else {
				result.Unsupported[modelID] = true
			}
			result.Metadata[modelID] = model.metadata()
		}
	}
	if protocols, err := FetchProtocolDocs(ctx, client, ZenDocsURL); err == nil {
		for modelID, protocol := range protocols {
			result.Protocols[modelID] = protocol
			delete(result.Unsupported, modelID)
		}
	}
	if len(result.Protocols) == 0 && len(result.Unsupported) == 0 {
		return Capabilities{}, errors.New("OpenCode capability endpoint returned no Zen models")
	}
	return result, nil
}

func FetchProtocolDocs(ctx context.Context, client *http.Client, endpoint string) (map[string]wire.Protocol, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain, text/markdown, */*")
	req.Header.Set("User-Agent", httpx.UserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("OpenCode endpoint documentation returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	result := make(map[string]wire.Protocol)
	for _, line := range strings.Split(string(body), "\n") {
		match := protocolDocEndpointPattern.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		modelID := strings.TrimSpace(match[1])
		if modelID == "" || strings.ContainsAny(modelID, " `|") {
			continue
		}
		var protocol wire.Protocol
		switch match[2] {
		case "chat/completions":
			protocol = wire.Chat
		case "responses":
			protocol = wire.Responses
		case "messages":
			protocol = wire.Anthropic
		case "systemone":
			protocol = wire.SystemOne
		}
		if protocol != "" {
			result[modelID] = protocol
		}
	}
	if len(result) == 0 {
		return nil, errors.New("OpenCode endpoint documentation returned no protocol rows")
	}
	return result, nil
}

func isZenProvider(providerID, api string) bool {
	value := strings.ToLower(strings.TrimSpace(providerID + " " + api))
	if strings.Contains(value, "opencode-go") || strings.Contains(value, "/go/") {
		return false
	}
	return strings.Contains(value, "opencode") || strings.Contains(value, "/zen/")
}

func protocolForSDK(npm string) (wire.Protocol, bool) {
	value := strings.ToLower(strings.TrimSpace(npm))
	switch {
	case strings.Contains(value, "anthropic"):
		return wire.Anthropic, true
	case value == "@ai-sdk/openai" || strings.HasSuffix(value, "/openai"):
		return wire.Responses, true
	case strings.Contains(value, "openai-compatible"):
		return wire.Chat, true
	default:
		return "", false
	}
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func FetchModels(ctx context.Context, client *http.Client, baseURL, key string) ([]string, int, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimmed+"/v1/models", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if strings.EqualFold(trimmed, strings.TrimRight(LegacyZenBase, "/")) || strings.HasSuffix(strings.ToLower(trimmed), "/zen") {
		req.Header.Set("User-Agent", httpx.LegacyUserAgent())
	} else {
		req.Header.Set("User-Agent", httpx.UserAgent())
	}
	req.Header.Set("x-opencode-client", "cli")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("models endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload modelsResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if err := dec.Decode(&payload); err != nil {
		return nil, resp.StatusCode, err
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID != "" {
			models = append(models, item.ID)
		}
	}
	if len(models) == 0 {
		return nil, resp.StatusCode, errors.New("models endpoint returned an empty list")
	}
	return models, resp.StatusCode, nil
}

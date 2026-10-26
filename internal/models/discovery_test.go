package models

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

type discoveryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f discoveryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFetchCapabilitiesRetainsFreeModelsWithUnknownSDK(t *testing.T) {
	const endpoint = "https://catalog.invalid/api.json"
	const currentDocs = "https://raw.githubusercontent.com/anomalyco/opencode/dev/services/www/src/docs/content/console/models.mdx"
	const metadata = `{"opencode":{"api":"https://opencode.ai/inference/v1","npm":"@ai-sdk/openai-compatible","models":{
		"brand-new-free":{"provider":{"npm":"@vendor/new-sdk"}},
		"known-response-free":{"provider":{"npm":"@ai-sdk/openai"}},
		"known-message-free":{"provider":{"npm":"@ai-sdk/anthropic"}},
		"documented-free":{},
		"decision-free":{"provider":{"npm":"@vendor/systemone"}},
		"unknown-paid":{"provider":{"npm":"@vendor/new-sdk"}}
	}}}`
	docs := "| Decision | decision-free | `https://opencode.ai/inference/v1/systemone` | - |\n" +
		"| Documented | documented-free | `https://opencode.ai/inference/v1/responses` | sdk |\n"
	client := &http.Client{Transport: discoveryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		status, body := http.StatusNotFound, "not found"
		switch req.URL.String() {
		case endpoint:
			status, body = http.StatusOK, metadata
		case currentDocs:
			status, body = http.StatusOK, docs
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	capabilities, err := FetchCapabilities(context.Background(), client, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]wire.Protocol{
		"brand-new-free":      wire.Chat,
		"known-response-free": wire.Responses,
		"known-message-free":  wire.Anthropic,
		"documented-free":     wire.Responses,
		"decision-free":       wire.SystemOne,
	}
	for model, protocol := range want {
		if got := capabilities.Protocols[config.TierZen][model]; got != protocol {
			t.Errorf("%s protocol = %s, want %s", model, got, protocol)
		}
		if capabilities.Unsupported[config.TierZen][model] {
			t.Errorf("%s was incorrectly marked unsupported", model)
		}
	}
	if !capabilities.Unsupported[config.TierZen]["unknown-paid"] {
		t.Error("unknown paid SDK was incorrectly accepted")
	}
}

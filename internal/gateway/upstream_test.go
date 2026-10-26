package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

func TestAnonymousUpstreams(t *testing.T) {
	const reverseBase = "http://reverse.invalid/opencode/https/opencode.ai"
	tests := []struct {
		name     string
		upstream config.UpstreamConfig
		want     []string
	}{
		{
			name:     "default origins",
			upstream: config.UpstreamConfig{Anonymous: "https://opencode.ai/inference", Zen: models.V1Base},
			want:     []string{"https://opencode.ai/inference"},
		},
		{
			name:     "unset Zen retains default",
			upstream: config.UpstreamConfig{Anonymous: "https://opencode.ai/inference"},
			want:     []string{"https://opencode.ai/inference"},
		},
		{
			name:     "reverse proxy inference is retained",
			upstream: config.UpstreamConfig{Anonymous: reverseBase + "/inference/", Zen: reverseBase + "/zen/"},
			want:     []string{reverseBase + "/inference"},
		},
		{
			name:     "legacy anonymous Zen is migrated to inference",
			upstream: config.UpstreamConfig{Anonymous: reverseBase + "/zen/", Zen: reverseBase + "/zen"},
			want:     []string{reverseBase + "/inference"},
		},
		{
			name:     "unset primary derives inference behind reverse proxy",
			upstream: config.UpstreamConfig{Zen: reverseBase + "/zen"},
			want:     []string{reverseBase + "/inference"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &Gateway{cfg: config.Config{Upstream: tt.upstream}}
			if got := g.anonymousUpstreams(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("anonymousUpstreams() = %q, want %q", got, tt.want)
			}
		})
	}
}

type anonymousRoundTripFunc func(*http.Request) (*http.Response, error)

func (f anonymousRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAnonymousDoesNotFallbackToRetiredZen(t *testing.T) {
	const base = "http://reverse.invalid/opencode/https/opencode.ai"
	var requested []string
	client := &http.Client{Transport: anonymousRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requested = append(requested, req.URL.String())
		status := http.StatusServiceUnavailable
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    req,
		}, nil
	})}
	proxy := &proxyTransport{name: "direct", client: client, bun: client}
	proxy.healthy.Store(true)
	transports := &proxyTransports{items: []*proxyTransport{proxy}}
	cfg := config.Config{
		Anonymous: true,
		Upstream:  config.UpstreamConfig{Anonymous: base + "/inference", Zen: base + "/zen"},
		Retry:     config.RetryConfig{MaxAttempts: 1},
	}
	g := &Gateway{
		cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		transports: transports, anonymous: anonymousPool(true, transports, 0, anonymousBackoff{}),
		catalog: models.Catalog(config.TierZen, nil),
	}
	route := models.Route{ID: "test-model", Tier: config.TierZen, Protocol: wire.Chat, Anonymous: true}
	response, err, attempts := g.doAnonymousUpstream(context.Background(), route,
		map[config.Tier][]byte{config.TierZen: []byte(`{"model":"test-model","messages":[]}`)}, identity.RequestIDs{}, 0)
	if err != nil {
		t.Fatalf("doAnonymousUpstream() error: %v", err)
	}
	if response == nil {
		t.Fatal("doAnonymousUpstream() returned no response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || attempts != 1 {
		t.Fatalf("response status = %d, attempts = %d; want original 503 after one inference attempt", response.StatusCode, attempts)
	}
	want := []string{base + "/inference/openai/v1/chat/completions"}
	if !reflect.DeepEqual(requested, want) {
		t.Fatalf("upstream requests = %q, want %q", requested, want)
	}
}

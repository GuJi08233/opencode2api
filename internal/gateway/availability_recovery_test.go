package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

func TestAvailabilityRecheckRestoresDisabledFreeModel(t *testing.T) {
	const model = "recovering-free"
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected probe endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	availability, err := models.NewAvailabilityStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := availability.Record(model, 0, false, "model_unavailable", "anonymous", time.Now().Add(-25*time.Hour), models.EffortProbe{}); err != nil {
		t.Fatal(err)
	}
	catalog := models.NewCatalog(nil)
	catalog.SetAvailabilityStore(availability)
	catalog.ReplaceWithCapabilities([]string{model}, map[string]wire.Protocol{model: wire.Chat}, nil, nil)
	if _, err := catalog.Route(model); err == nil {
		t.Fatal("disabled model was available before its scheduled recheck")
	}
	proxy := &proxyTransport{client: upstream.Client()}
	proxy.healthy.Store(true)
	gateway := &Gateway{
		cfg:          config.Config{Upstream: config.UpstreamConfig{Zen: upstream.URL, Anonymous: upstream.URL}},
		catalog:      catalog,
		availability: availability,
		anonymous:    &anonymousPool{nodes: []*anonymousNode{{proxy: proxy}}},
	}
	manager := &RuntimeManager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	manager.current.Store(&gatewayRuntime{gateway: gateway, availability: availability})
	manager.checkFreeModels(context.Background())
	if requests.Load() != 1 {
		t.Fatalf("scheduled recheck made %d requests, want 1", requests.Load())
	}
	if item := availability.Get(model); item.Disabled || item.Reason != "probe_succeeded" || !item.NextCheck.After(time.Now()) {
		t.Fatalf("successful recheck did not restore and reschedule model: %+v", item)
	}
	if _, err := catalog.Route(model); err != nil {
		t.Fatalf("recovered model is still unavailable: %v", err)
	}
}

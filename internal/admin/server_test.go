package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"opencode2api/internal/config"
	"opencode2api/internal/gateway"
	"opencode2api/internal/telemetry"
)

func TestPutConfigPreservesAnonymousUpstream(t *testing.T) {
	const original = "http://127.0.0.1:1/proxy/inference"
	const replacement = "http://127.0.0.1:1/new-proxy/inference"
	for _, test := range []struct {
		name      string
		anonymous *string
		want      string
	}{
		{name: "omitted by WebUI", want: original},
		{name: "empty", anonymous: stringPointer(""), want: original},
		{name: "explicit replacement", anonymous: stringPointer(replacement), want: replacement},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			initial := `{"server_keys":["local-test-key"],"anonymous":true,"upstream":{"zen":"http://127.0.0.1:1/proxy/zen","go":"http://127.0.0.1:1/proxy/zen/go","anonymous":"` + original + `"}}`
			if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			// A canceled root keeps background refreshes from contacting any
			// upstream while the real management handler applies and saves.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			monitor := telemetry.Monitor()
			hub := telemetry.LogHub(cfg.Logging.RingSize)
			manager, err := gateway.RuntimeManager(ctx, path, cfg, logger, monitor, hub, config.SecretRedactor(), new(slog.LevelVar))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(manager.Shutdown)
			server := New(manager, monitor, hub, logger)
			update := ConfigUpdate{
				Listen: cfg.Listen, ServerKeys: []SecretInput{{ID: config.Fingerprint(cfg.ServerKeys[0])}},
				Anonymous: cfg.Anonymous, Warp: cfg.Warp, Prefer: cfg.Prefer,
				Retry: cfg.Retry, Models: cfg.Models, Performance: cfg.Performance, Logging: cfg.Logging,
				WebUI: WebUIView{SessionTTLMinutes: cfg.WebUI.SessionTTLMinutes},
			}
			encoded, err := json.Marshal(update)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(encoded, &payload); err != nil {
				t.Fatal(err)
			}
			zen := "http://127.0.0.1:1/new-proxy/zen"
			goBase := "http://127.0.0.1:1/new-proxy/zen/go"
			upstream := map[string]string{"zen": zen, "go": goBase}
			if test.anonymous != nil {
				upstream["anonymous"] = *test.anonymous
			}
			payload["upstream"] = upstream
			encoded, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.handlePutConfig(response, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(encoded)))
			if response.Code != http.StatusOK {
				t.Fatalf("PUT /api/config returned %d: %s", response.Code, response.Body.String())
			}
			persisted, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			for name, got := range map[string]config.UpstreamConfig{"runtime": manager.Config().Upstream, "persisted": persisted.Upstream} {
				if got.Anonymous != test.want {
					t.Errorf("%s anonymous upstream = %q, want %q", name, got.Anonymous, test.want)
				}
				if got.Zen != zen || got.Go != goBase {
					t.Errorf("%s keyed upstream changes were not applied: %+v", name, got)
				}
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

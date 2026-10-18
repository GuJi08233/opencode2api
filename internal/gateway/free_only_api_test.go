package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"opencode2api/internal/config"
	"opencode2api/internal/jsonutil"
	wire "opencode2api/internal/protocol"
)

func TestFreeOnlyAPI(t *testing.T) {
	const localKey = "test-local-key"
	const completion = "Hello world"
	const upstreamStream = "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"test-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"test-free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"test-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/zen/v1/chat/completions" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer public" || r.Header.Get("x-api-key") != "" {
			t.Errorf("local credentials reached upstream or public credential is missing")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload["model"] != "test-free" || payload["stream"] != true || len(jsonutil.SliceAt(payload, "tools")) == 0 {
			t.Errorf("upstream request lost free model, forced stream, or tools: %v", payload)
		}
		messages := jsonutil.SliceAt(payload, "messages")
		if len(messages) != 1 {
			t.Errorf("upstream request has %d messages, want 1", len(messages))
		} else if message, _ := messages[0].(map[string]any); jsonutil.StringAt(message, "role") != "user" || jsonutil.StringAt(message, "content") != "Say hello" {
			t.Errorf("upstream request did not preserve the user message: %v", messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, upstreamStream)
	}))
	defer upstream.Close()
	cfg, err := config.Load("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerKeys = []string{localKey}
	cfg.Upstream.Zen, cfg.Upstream.Anonymous = upstream.URL+"/zen", upstream.URL+"/inference"
	cfg, err = config.Normalize("config.json", cfg)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.transports.items[0].client.CloseIdleConnections()
	// Advertising the paid model ensures its rejection tests the free-tier
	// requirement rather than only the upstream catalog membership check.
	gateway.catalog.ReplaceWithCapabilities([]string{"test-free", "paid-model"}, map[string]wire.Protocol{"test-free": wire.Chat, "paid-model": wire.Chat}, nil, nil)
	api := httptest.NewServer(gateway.Handler())
	defer api.Close()
	for _, tc := range []struct {
		name, endpoint, body, authHeader, authValue string
		stream                                      bool
	}{
		{name: "Chat collapses SSE", endpoint: "/v1/chat/completions", body: `{"model":"test-free","messages":[{"role":"user","content":"Say hello"}],"stream":false}`, authHeader: "Authorization", authValue: "Bearer " + localKey},
		{name: "Responses converts Chat SSE", endpoint: "/v1/responses", body: `{"model":"test-free","input":"Say hello","stream":false}`, authHeader: "Authorization", authValue: "Bearer " + localKey},
		{name: "Anthropic converts Chat SSE", endpoint: "/v1/messages", body: `{"model":"test-free","messages":[{"role":"user","content":"Say hello"}],"max_tokens":64,"stream":false}`, authHeader: "x-api-key", authValue: localKey},
		{name: "Chat streams SSE", endpoint: "/v1/chat/completions", body: `{"model":"test-free","messages":[{"role":"user","content":"Say hello"}],"stream":true}`, authHeader: "x-api-key", authValue: localKey, stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := upstreamCalls.Load()
			req, err := http.NewRequest(http.MethodPost, api.URL+tc.endpoint, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(tc.authHeader, tc.authValue)
			resp, err := api.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || upstreamCalls.Load()-before != 1 {
				t.Fatalf("status=%d upstream calls=%d, want 200 and 1: %s", resp.StatusCode, upstreamCalls.Load()-before, body)
			}
			if tc.stream {
				if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
					t.Fatalf("stream content type = %q", resp.Header.Get("Content-Type"))
				}
				var text strings.Builder
				done, stopped := false, false
				scanner := bufio.NewScanner(strings.NewReader(string(body)))
				for scanner.Scan() {
					data, ok := strings.CutPrefix(scanner.Text(), "data: ")
					if !ok {
						continue
					}
					if data == "[DONE]" {
						done = true
						continue
					}
					var chunk map[string]any
					if err := json.Unmarshal([]byte(data), &chunk); err != nil {
						t.Fatalf("invalid streamed JSON: %v", err)
					}
					for _, raw := range jsonutil.SliceAt(chunk, "choices") {
						choice, _ := raw.(map[string]any)
						text.WriteString(jsonutil.StringAt(choice, "delta", "content"))
						stopped = stopped || jsonutil.StringAt(choice, "finish_reason") == "stop"
					}
				}
				if scanner.Err() != nil || text.String() != completion || !done || !stopped {
					t.Fatalf("incomplete Chat stream: text=%q done=%t stopped=%t error=%v", text.String(), done, stopped, scanner.Err())
				}
				return
			}
			if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
				t.Fatalf("non-stream content type = %q", resp.Header.Get("Content-Type"))
			}
			var reply map[string]any
			if err := json.Unmarshal(body, &reply); err != nil {
				t.Fatalf("invalid non-stream JSON: %v: %s", err, body)
			}
			var text string
			switch tc.endpoint {
			case "/v1/chat/completions":
				choices := jsonutil.SliceAt(reply, "choices")
				if len(choices) != 1 || reply["object"] != "chat.completion" {
					t.Fatalf("invalid Chat response: %s", body)
				}
				choice, _ := choices[0].(map[string]any)
				text = jsonutil.StringAt(choice, "message", "content")
				if choice["finish_reason"] != "stop" {
					t.Fatalf("Chat response lost stop reason: %s", body)
				}
			case "/v1/responses":
				if reply["object"] != "response" || reply["status"] != "completed" {
					t.Fatalf("invalid Responses response: %s", body)
				}
				for _, raw := range jsonutil.SliceAt(reply, "output") {
					item, _ := raw.(map[string]any)
					for _, rawPart := range jsonutil.SliceAt(item, "content") {
						part, _ := rawPart.(map[string]any)
						if part["type"] == "output_text" {
							text += jsonutil.StringAt(part, "text")
						}
					}
				}
			case "/v1/messages":
				if reply["type"] != "message" || reply["role"] != "assistant" || reply["stop_reason"] != "end_turn" {
					t.Fatalf("invalid Anthropic response: %s", body)
				}
				for _, raw := range jsonutil.SliceAt(reply, "content") {
					part, _ := raw.(map[string]any)
					if part["type"] == "text" {
						text += jsonutil.StringAt(part, "text")
					}
				}
			}
			if text != completion || reply["model"] != "test-free" {
				t.Fatalf("response lost completion or model: %s", body)
			}
		})
	}
	for _, model := range []string{"paid-model", "unknown-free"} {
		t.Run("reject "+model+" locally", func(t *testing.T) {
			before := upstreamCalls.Load()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"Say hello"}]}`))
			req.Header.Set("Authorization", "Bearer "+localKey)
			recorder := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest || upstreamCalls.Load() != before {
				t.Fatalf("status=%d upstream calls=%d, want 400 and 0: %s", recorder.Code, upstreamCalls.Load()-before, recorder.Body.String())
			}
		})
	}
}

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

func TestFreeTierRequestPreservesReverseProxyAndMinimalHeaders(t *testing.T) {
	t.Setenv("OPENCODE_ZEN_FREE_SESSION_AFFINITY", "")
	const base = "http://reverse.invalid:2260/private/route/https/opencode.ai/inference"
	ids := identity.RequestIDs{
		Session: "internal-conversation", ChildSession: "internal-child", ParentSession: "internal-parent",
		Request: "internal-request", FreeTierSession: "ses_123456789abcABCDEFGHIJKLMN",
		FreeTierProject: strings.Repeat("a", 40),
	}
	for _, test := range []struct {
		protocol wire.Protocol
		path     string
	}{
		{wire.Chat, "/openai/v1/chat/completions"},
		{wire.Responses, "/openai/v1/responses"},
		{wire.Anthropic, "/anthropic/v1/messages?beta=true"},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			req, err := upstreamRequest(context.Background(), base, test.protocol, []byte(`{"model":"example-free"}`), ids, anonymousZenKey)
			if err != nil {
				t.Fatal(err)
			}
			if req.URL.String() != base+test.path {
				t.Fatalf("URL = %s, want %s", req.URL, base+test.path)
			}
			if req.Header.Get("x-opencode-session") != ids.FreeTierSession || req.Header.Get("x-opencode-session-id") != ids.FreeTierSession {
				t.Fatalf("wire sessions do not use one request identity: %+v", req.Header)
			}
			if req.Header.Get("x-opencode-project") != ids.FreeTierProject {
				t.Errorf("project = %q", req.Header.Get("x-opencode-project"))
			}
			if req.Header.Get("User-Agent") != "opencode/latest/2.1.4/cli" {
				t.Errorf("User-Agent = %q", req.Header.Get("User-Agent"))
			}
			wantNames := []string{"Accept", "Content-Type", "User-Agent", "X-Opencode-Client", "X-Opencode-Project", "X-Opencode-Session", "X-Opencode-Session-Id"}
			if test.protocol == wire.Anthropic {
				wantNames = append(wantNames, "Anthropic-Version", "X-Api-Key")
				if req.Header.Get("X-Api-Key") != "public" {
					t.Error("Anthropic public credential missing")
				}
			} else {
				wantNames = append(wantNames, "Authorization")
				if req.Header.Get("Authorization") != "Bearer public" {
					t.Error("public credential missing")
				}
			}
			var gotNames []string
			for name := range req.Header {
				gotNames = append(gotNames, name)
			}
			sort.Strings(gotNames)
			sort.Strings(wantNames)
			if !reflect.DeepEqual(gotNames, wantNames) {
				t.Errorf("header names = %q, want minimal set %q", gotNames, wantNames)
			}
		})
	}
	if ids.Session != "internal-conversation" || ids.Project != "" || ids.Request != "internal-request" {
		t.Fatalf("internal routing identity mutated: %+v", ids)
	}
}

func TestFreeXPromptToolsAndBodyOrder(t *testing.T) {
	input := []byte(`{"temperature":0.2,"model":"example-free","messages":[{"role":"system","content":"Keep the caller instruction."},{"role":"user","content":"Reply OK."}],"tools":[{"type":"function","function":{"name":"read_file","description":"Host reader","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"read_file"}}}`)
	body, renames := prepareAnonymousBody(input, wire.Chat, false)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["stream"] != true || payload["max_completion_tokens"] != float64(32000) {
		t.Fatalf("missing streamed client defaults: stream=%v completion=%v", payload["stream"], payload["max_completion_tokens"])
	}
	if got := payload["stream_options"].(map[string]any)["include_usage"]; got != true {
		t.Fatalf("stream usage = %v", got)
	}
	messages := payload["messages"].([]any)
	prompt := messages[0].(map[string]any)["content"].(string)
	for _, want := range []string{opencodePromptMarkerV2, "Model ID: example-free", time.Now().UTC().Format("Mon Jan 02 2006"), "<env>", "Code Mode is not active", "clint_read_file", "Keep the caller instruction."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"{{MODEL", "{{DATE}}", "{{ENV}}", "customize-opencode", "<available_skills>"} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("prompt contains %q", unwanted)
		}
	}
	tools := payload["tools"].([]any)
	wantOfficial := []string{"edit", "glob", "grep", "question", "read", "shell", "skill", "subagent", "webfetch", "websearch", "write", "execute"}
	if len(tools) != len(wantOfficial)+1 {
		t.Fatalf("tools = %d, want host tool plus %d captured tools", len(tools), len(wantOfficial))
	}
	for i, want := range append([]string{"clint_read_file"}, wantOfficial...) {
		function := tools[i].(map[string]any)["function"].(map[string]any)
		if function["name"] != want || function["strict"] != false {
			t.Errorf("tool %d name/strict = %v/%v, want %s/false", i, function["name"], function["strict"], want)
		}
	}
	if renames.Restore("clint_read_file") != "read_file" {
		t.Fatal("host tool response name was not reversible")
	}
	choice := payload["tool_choice"].(map[string]any)["function"].(map[string]any)
	if choice["name"] != "clint_read_file" {
		t.Fatalf("tool_choice was not renamed: %v", choice)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	_, _ = decoder.Token()
	var keys []string
	for decoder.More() {
		key, _ := decoder.Token()
		keys = append(keys, key.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
	wantKeys := append(append([]string{}, v2ChatKeyOrder...), "temperature", "tool_choice")
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("wire key order = %v, want %v", keys, wantKeys)
	}
}

func TestFreeXPromptInjectionIsIdempotentAcrossProtocols(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, marker := range []string{opencodePromptMarker, opencodePromptMarkerV2} {
			t.Run(string(protocol)+"/"+marker[:12], func(t *testing.T) {
				payload := map[string]any{"model": "example-free"}
				switch protocol {
				case wire.Chat:
					payload["messages"] = []any{map[string]any{"role": "system", "content": marker + ": existing"}}
				case wire.Responses:
					payload["instructions"] = marker + ": existing"
				case wire.Anthropic:
					payload["system"] = []any{map[string]any{"type": "text", "text": marker + ": existing"}}
				}
				before, _ := json.Marshal(payload)
				if injectOfficialPrompt(payload, protocol, false) {
					t.Fatal("already captured official prompt was injected a second time")
				}
				after, _ := json.Marshal(payload)
				if !bytes.Equal(before, after) {
					t.Fatal("existing official prompt changed")
				}
			})
		}
	}
}

func TestFreeXPromptPlacementForResponsesAndMessages(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Responses, wire.Anthropic} {
		t.Run(string(protocol), func(t *testing.T) {
			payload := map[string]any{"model": "example-free"}
			if protocol == wire.Responses {
				payload["instructions"] = "Caller instructions"
				payload["input"] = []any{map[string]any{"role": "user", "content": "Reply OK"}}
				payload["tools"] = []any{map[string]any{"type": "function", "name": "host_read", "parameters": map[string]any{"type": "object"}}}
			} else {
				payload["system"] = []any{map[string]any{"type": "text", "text": "Caller instructions"}}
				payload["messages"] = []any{map[string]any{"role": "user", "content": "Reply OK"}}
				payload["tools"] = []any{map[string]any{"name": "host_read", "input_schema": map[string]any{"type": "object"}}}
			}
			input, _ := json.Marshal(payload)
			body, renames := prepareAnonymousBody(input, protocol, false)
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			var text string
			if protocol == wire.Responses {
				text, _ = payload["instructions"].(string)
			} else {
				text = strings.Join(textFragments(payload["system"]), "\n")
			}
			for _, want := range []string{opencodePromptMarkerV2, "Caller instructions", "clint_host_read", "Code Mode is not active"} {
				if !strings.Contains(text, want) {
					t.Errorf("%s prompt missing %q", protocol, want)
				}
			}
			if payload["stream"] != true || renames.Restore("clint_host_read") != "host_read" {
				t.Fatal("native protocol shaping lost streaming or host tool mapping")
			}
			before, _ := json.Marshal(payload)
			if injectOfficialPrompt(payload, protocol, false) {
				t.Fatal("generated prompt was not recognized on a second injection")
			}
			after, _ := json.Marshal(payload)
			if !bytes.Equal(before, after) {
				t.Fatal("second prompt injection changed the body")
			}
		})
	}
}

func TestFreeXPromptDoesNotAdvertiseUnimplementedHostTools(t *testing.T) {
	payload := map[string]any{"model": "example-free", "messages": []any{map[string]any{"role": "user", "content": "Reply OK"}}}
	_, _ = renameAndCompleteTools(payload, wire.Chat, false)
	if !injectOfficialPrompt(payload, wire.Chat, false) {
		t.Fatal("prompt was not injected")
	}
	text := payload["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(text, "This host implements no tools at all; answer without tool calls.") || strings.Contains(text, "customize-opencode") {
		t.Fatal("prompt gave the model tools or skills the host does not provide")
	}
}

func TestFreeXCompletionLimitKeepsCallerChoice(t *testing.T) {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		payload := map[string]any{key: 42}
		if ensureV2CompletionLimit(payload) || payload[key] != 42 || len(payload) != 1 {
			t.Fatalf("explicit %s was changed: %v", key, payload)
		}
	}
}

func TestInferenceAttemptLogShowsDestinationWithoutSensitiveData(t *testing.T) {
	var output bytes.Buffer
	g := &Gateway{logger: slog.New(slog.NewJSONHandler(&output, nil))}
	req, err := http.NewRequest(http.MethodPost, "http://private:secret@reverse.invalid:2260/proxy/inference/openai/v1/chat/completions?api_key=query-secret", strings.NewReader("private-body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer private-credential")
	g.logInferenceAttempt(models.Route{ID: "example-free"}, identity.RequestIDs{Request: "req-test"}, 1, "anonymous", &proxyTransport{name: "http://proxy:proxy-secret@forward.invalid"}, req, &http.Response{StatusCode: 429}, errors.New("error-with-private-credential"), time.Second)
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["event"] != "upstream_attempt" || record["status"] != float64(429) || !strings.Contains(record["upstream"].(string), "reverse.invalid:2260/proxy/inference/") {
		t.Fatalf("missing destination/status: %v", record)
	}
	for _, secret := range []string{"secret", "query-secret", "private-body", "private-credential", "proxy-secret"} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("attempt log leaked %q", secret)
		}
	}
}

func TestInferenceAttemptLogRedactsReverseProxyPrefix(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			var output bytes.Buffer
			g := &Gateway{logger: slog.New(slog.NewJSONHandler(&output, nil))}
			req, err := http.NewRequest(http.MethodPost,
				"http://private-user:private-password@resin:2260/private-resin-token/private-platform/"+scheme+"/opencode.ai/inference/openai/v1/chat/completions?api_key=private-query", nil)
			if err != nil {
				t.Fatal(err)
			}
			original := req.URL.String()
			g.logInferenceAttempt(models.Route{ID: "example-free"}, identity.RequestIDs{Request: "req-test"}, 1, "anonymous", &proxyTransport{name: "direct"}, req, &http.Response{StatusCode: 200}, nil, time.Second)
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			upstream, _ := record["upstream"].(string)
			if !strings.Contains(upstream, "resin:2260/") || !strings.Contains(upstream, "/"+scheme+"/opencode.ai/inference/openai/v1/chat/completions") {
				t.Fatalf("proxy/destination routing evidence was lost: %q", upstream)
			}
			for _, secret := range []string{"private-user", "private-password", "private-resin-token", "private-platform", "private-query"} {
				if strings.Contains(output.String(), secret) {
					t.Errorf("info log exposed reverse-proxy credential %q", secret)
				}
			}
			if req.URL.String() != original {
				t.Fatal("redacting logs changed the actual request destination")
			}
		})
	}
}

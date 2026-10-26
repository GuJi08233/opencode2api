package gateway

import (
	_ "embed"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/jsonutil"
	wire "opencode2api/internal/protocol"
)

// The free tier accepts only agent-shaped requests, and its abuse checks expect
// the system prompt the official CLI sends. These assets are the CLI's own
// wording: the legacy v1 text for keyed /zen and an independent OpenCode
// 2.1.4 capture for /inference and /zen/go. The v2 asset is shared with FreeX;
// it is not a rewrite of the older prompt.
//
//go:embed opencode_prompt_v1.txt
var opencodePromptV1 string

//go:embed opencode_prompt_v2.txt
var opencodePromptV2 string

// opencodePromptMarker identifies a body that already carries the official CLI
// system prompt, so a client that sends it itself is left alone.
const opencodePromptMarker = "You are opencode, an interactive CLI tool"
const opencodePromptMarkerV2 = "You are an AI agent running in OpenCode"

var (
	opencodeEnvOnce sync.Once
	opencodeWorkDir string
)

// opencodeEnvironment renders the <env> block the CLI puts after its base
// prompt. The working directory is resolved once per process; a platform
// without one falls back to the official container path.
func opencodeEnvironment(model string) string {
	opencodeEnvOnce.Do(func() {
		directory := ""
		if current, err := os.Getwd(); err == nil {
			directory = filepath.Clean(strings.TrimSpace(current))
		}
		if directory == "" || directory == "." {
			directory = "/workspace"
		}
		opencodeWorkDir = directory
	})
	apiModel := strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if apiModel == "" {
		apiModel = "unknown"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return strings.Join([]string{
		"You are powered by the model named " + apiModel + ". The exact model ID is opencode/" + apiModel,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + opencodeWorkDir,
		"  Workspace root folder: " + opencodeWorkDir,
		"  Is directory a git repo: yes",
		"  Platform: " + platform,
		"  Today's date: " + time.Now().UTC().Format("Mon Jan 02 2006"),
		"</env>",
	}, "\n")
}

// opencodeEnvironmentV2 renders the environment block captured from 2.1.4.
// The current date is part of the template, above this block.
func opencodeEnvironmentV2(model string) string {
	_ = opencodeEnvironment(model) // Resolve the process directory once.
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return strings.Join([]string{
		"<env>",
		"  Working directory: " + opencodeWorkDir,
		"  Workspace root folder: " + opencodeWorkDir,
		"  Is directory a git repo: yes",
		"  Platform: " + platform,
		"  Prefer " + filepath.Join(os.TempDir(), "opencode") + " over generic system temporary directories such as /tmp; it is pre-created and approved for external access.",
		"</env>",
	}, "\n")
}

// opencodeSystemPrompt assembles the independent capture for each generation.
// A gateway does not advertise OpenCode's built-in skills, which it cannot run.
func opencodeSystemPrompt(model string, v1 bool) string {
	if v1 {
		return opencodePromptV1 + opencodeEnvironment(model)
	}
	model = strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if model == "" {
		model = "unknown"
	}
	parts := strings.Split(model, "-")
	for i, part := range parts {
		if part != "" {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.NewReplacer(
		"{{MODEL_NAME}}", strings.Join(parts, " "),
		"{{MODEL_ID}}", model,
		"{{DATE}}", time.Now().UTC().Format("Mon Jan 02 2006"),
		"{{ENV}}", opencodeEnvironmentV2(model),
	).Replace(opencodePromptV2)
}

// hostToolAvailability names the tools the caller actually implements. The
// captured Code Mode prompt otherwise leads the model to call OpenCode's
// execute tool, which the gateway merely advertises as part of the wire shape.
func hostToolAvailability(payload map[string]any) string {
	var names []string
	seen := map[string]bool{}
	for _, raw := range jsonutil.SliceAt(payload, "tools") {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := jsonutil.FirstString(jsonutil.StringAt(tool, "function", "name"), jsonutil.StringAt(tool, "name"))
		if strings.HasPrefix(name, wire.ClientToolPrefix) && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	callable := "This host implements no tools at all; answer without tool calls."
	if len(names) > 0 {
		callable = "The tools you can call are the host's own, under their " + wire.ClientToolPrefix + " names: " + strings.Join(names, ", ") + "."
	}
	return strings.Join([]string{
		"<system-reminder>",
		"Code Mode is not active in this session: this host is not OpenCode, and the built-in tools described above - `execute` and every entry of the catalog - are not implemented here. A call to one is answered with an unknown-tool error and nothing else.",
		callable + " Every other entry in the tools array is an OpenCode built-in nothing here runs: read that array as the host's own list, not as OpenCode's.",
		"</system-reminder>",
	}, "\n")
}

// injectOfficialPrompt prepends the official CLI system prompt to a shaped
// free-tier payload. It reports whether the payload changed; a body that
// already carries the marker is left untouched, so repeated shaping cannot
// duplicate the prompt. System One payloads are not chat bodies and are never
// touched.
func injectOfficialPrompt(payload map[string]any, protocol wire.Protocol, v1 bool) bool {
	if payload == nil || protocol == wire.SystemOne {
		return false
	}
	model, _ := payload["model"].(string)
	prompt := opencodeSystemPrompt(model, v1) + "\n\n" + hostToolAvailability(payload)
	switch protocol {
	case wire.Chat:
		return injectChatPrompt(payload, prompt)
	case wire.Anthropic:
		return injectAnthropicPrompt(payload, prompt)
	case wire.Responses:
		return injectResponsesPrompt(payload, prompt)
	default:
		return false
	}
}

// injectChatPrompt folds every system/developer message into the single system
// message the 1.x and 2.x CLIs send, then prepends it. Client instructions keep
// their order and are separated from the official prompt by a blank line.
func injectChatPrompt(payload map[string]any, prompt string) bool {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(messages)+1)
	var instructions []string
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" {
			kept = append(kept, raw)
			continue
		}
		content := message["content"]
		if carriesOpenCodePrompt(content) {
			return false
		}
		for _, part := range textFragments(content) {
			if part = strings.TrimSpace(part); part != "" {
				instructions = append(instructions, part)
			}
		}
	}
	system := prompt
	if joined := strings.Join(instructions, "\n"); joined != "" {
		system += "\n\n" + joined
	}
	payload["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, kept...)
	return true
}

// injectAnthropicPrompt prepends the prompt to the top-level system value,
// keeping a caller's string or block list intact after it.
func injectAnthropicPrompt(payload map[string]any, prompt string) bool {
	system, exists := payload["system"]
	if exists && carriesOpenCodePrompt(system) {
		return false
	}
	if !exists {
		payload["system"] = prompt
		return true
	}
	switch value := system.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			payload["system"] = prompt
		} else {
			payload["system"] = prompt + "\n\n" + value
		}
	case []any:
		payload["system"] = append([]any{map[string]any{"type": "text", "text": prompt}}, value...)
	default:
		payload["system"] = prompt
	}
	return true
}

// injectResponsesPrompt prepends the prompt to the Responses instructions.
func injectResponsesPrompt(payload map[string]any, prompt string) bool {
	instructions, exists := payload["instructions"]
	if exists && carriesOpenCodePrompt(instructions) {
		return false
	}
	if text, ok := instructions.(string); exists && ok {
		if strings.TrimSpace(text) == "" {
			payload["instructions"] = prompt
		} else {
			payload["instructions"] = prompt + "\n\n" + text
		}
		return true
	}
	payload["instructions"] = prompt
	return true
}

// textFragments flattens a system or instruction value into its text: a plain
// string, a list of content blocks, or a block object carrying text or content.
func textFragments(value any) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var parts []string
		for _, item := range typed {
			parts = append(parts, textFragments(item)...)
		}
		return parts
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return textFragments(text)
		}
		if content, ok := typed["content"]; ok {
			return textFragments(content)
		}
	}
	return nil
}

// carriesOpenCodePrompt reports whether any text inside value already contains
// the marker.
func carriesOpenCodePrompt(value any) bool {
	for _, part := range textFragments(value) {
		if strings.Contains(part, opencodePromptMarker) || strings.Contains(part, opencodePromptMarkerV2) {
			return true
		}
	}
	return false
}

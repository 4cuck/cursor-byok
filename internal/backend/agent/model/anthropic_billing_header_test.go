package modeladapter

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"cursor/internal/modelchannel"
)

// capturedCLIPrompt is the prompt of a request claude-cli 2.1.329 sent to a
// local server on 2026-11-27. Its billing header block began
// "x-anthropic-billing-header: cc_version=2.1.329.909; cc_entrypoint=sdk-cli;".
const capturedCLIPrompt = "Reply with the single word ok."

// TestClaudeCodeFingerprint checks the fingerprint against the one the CLI
// sent for a known prompt, and against values its JavaScript rule gives for
// other texts (computed with that rule under Node).
func TestClaudeCodeFingerprint(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		version string
		want    string
	}{
		{"the prompt the CLI was seen to send", capturedCLIPrompt, "2.1.329", "909"},
		{"the same prompt under another version", capturedCLIPrompt, "2.1.179", "4bb"},
		{"no text", "", "2.1.329", "3c4"},
		{"a text shorter than every position", "hi", "2.1.329", "3c4"},
		{"a text that reaches the first position only", "12345", "2.1.329", "9cd"},
		{"a text Cursor could start a chat with", "<user_info>\nOS Version: win32 10.0.26200\n</user_info>", "2.1.329", "150"},
		{"characters of the Basic Multilingual Plane", "请帮我看看这个仓库里的构建脚本为什么在 Windows 上失败", "2.1.329", "b65"},
		// 😀 takes two UTF-16 units. Position 4 is its first unit in one text
		// and its second in the other; either unit alone is hashed as U+FFFD.
		{"the first unit of a two-unit character", "abcd😀fgh and some more text after it", "2.1.329", "772"},
		{"the second unit of a two-unit character", "abc😀efgh and some more text after it", "2.1.329", "772"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeCodeFingerprint(tt.text, tt.version); got != tt.want {
				t.Fatalf("claudeCodeFingerprint(%q, %q) = %q, want %q", tt.text, tt.version, got, tt.want)
			}
		})
	}
}

// TestAnthropicBillingHeaderNamesTheVersionOfTheUserAgent pins the billing
// header's text, and ties its version to the user agent's. Anthropic refuses a
// model that needs a newer Claude Code than cc_version names ("Claude Code
// 2.1.179 does not support this model; version 2.1.280 or newer is required"),
// which is what happened while the block still named an older version than
// the user agent did.
func TestAnthropicBillingHeaderNamesTheVersionOfTheUserAgent(t *testing.T) {
	if got, want := anthropicBillingHeader(capturedCLIPrompt), "x-anthropic-billing-header: cc_version=2.1.329.909; cc_entrypoint=cli; cch=37703;"; got != want {
		t.Fatalf("anthropicBillingHeader() = %q, want %q", got, want)
	}
	for _, agent := range []string{ClaudeCodeUserAgent, AnthropicClaudeCodeUserAgent} {
		if want := "claude-cli/" + ClaudeCodeVersion + " (external, " + claudeCodeEntrypoint + ")"; agent != want {
			t.Fatalf("user agent = %q, want %q", agent, want)
		}
	}
	header := anthropicBillingHeader("")
	if !strings.HasPrefix(header, anthropicBillingHeaderPrefix+" cc_version="+ClaudeCodeVersion+".") {
		t.Fatalf("billing header %q does not name version %s", header, ClaudeCodeVersion)
	}
	if !strings.Contains(header, "; cc_entrypoint="+claudeCodeEntrypoint+";") {
		t.Fatalf("billing header %q does not name the entrypoint of the user agent", header)
	}
}

func TestAnthropicBillingFingerprintText(t *testing.T) {
	messages := []anthropicMessage{
		{Role: "assistant", Content: []map[string]any{{"type": "text", "text": "an assistant message that comes first"}}},
		{Role: "user", Content: []map[string]any{
			{"type": "image", "source": map[string]any{"type": "base64"}},
			{"type": "text", "text": "the first text of the first user message"},
			{"type": "text", "text": "a second text"},
		}},
		{Role: "user", Content: []map[string]any{{"type": "text", "text": "a later user message"}}},
	}
	if got := anthropicFirstUserText(messages); got != "the first text of the first user message" {
		t.Fatalf("anthropicFirstUserText() = %q", got)
	}
	toolResultFirst := []anthropicMessage{
		{Role: "user", Content: []map[string]any{{"type": "tool_result", "tool_use_id": "call-1"}}},
		{Role: "user", Content: []map[string]any{{"type": "text", "text": "a later user message"}}},
	}
	if got := anthropicFirstUserText(toolResultFirst); got != "" {
		t.Fatalf("a first user message without text: anthropicFirstUserText() = %q, want none", got)
	}
	if got := anthropicFirstUserText(nil); got != "" {
		t.Fatalf("no messages: anthropicFirstUserText() = %q, want none", got)
	}

	// Only Anthropic's own API gets a fingerprint that follows the conversation.
	for _, official := range []string{"https://api.anthropic.com/v1/messages", "https://API.Anthropic.com/v1/messages"} {
		if got := anthropicBillingFingerprintText(official, messages); got != "the first text of the first user message" {
			t.Fatalf("anthropicBillingFingerprintText(%q) = %q, want the first user text", official, got)
		}
	}
	for _, other := range []string{"https://relay.example.com/v1/messages", "https://api.anthropic.com.example.com/v1/messages", "http://127.0.0.1:8080/v1/messages", ""} {
		if got := anthropicBillingFingerprintText(other, messages); got != "" {
			t.Fatalf("anthropicBillingFingerprintText(%q) = %q, want none", other, got)
		}
	}
}

// TestAnthropicStreamBillingHeader checks the first system block of the
// request the adapter sends: on Anthropic's own API it carries the fingerprint
// of the conversation's first user message, in every auth mode and on every
// turn; on another host it is the same for every conversation.
func TestAnthropicStreamBillingHeader(t *testing.T) {
	firstSystemText := func(t *testing.T, baseURL string, apiKey string, mode string, messages []Message) string {
		t.Helper()
		var body map[string]any
		adapter := &AnthropicAdapter{client: &http.Client{Transport: anthropicRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(raw, &body)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(canonicalAnthropicSSE()))}, nil
		})}}
		events, err := runAnthropicTestStream(t, adapter, StreamRequest{
			BaseURL: baseURL, APIKey: apiKey, ProviderModelID: "claude-opus-5-5", ModelCallID: "call-1",
			AnthropicAuthMode: mode, Messages: messages,
		}, "")
		if err != nil || countAnthropicTurnFinished(events) != 1 {
			t.Fatalf("Stream() error = %v, events = %+v", err, events)
		}
		system, _ := body["system"].([]any)
		if len(system) == 0 {
			t.Fatalf("the request has no system blocks: %+v", body["system"])
		}
		block, _ := system[0].(map[string]any)
		text, _ := block["text"].(string)
		return text
	}

	firstTurn := []Message{{Role: "system", Content: "REAL SYSTEM PROMPT"}, {Role: "user", Content: capturedCLIPrompt}}
	laterTurn := append(append([]Message(nil), firstTurn...), Message{Role: "assistant", Content: "ok"}, Message{Role: "user", Content: "and now something else entirely"})
	const official = "x-anthropic-billing-header: cc_version=2.1.329.909; cc_entrypoint=cli; cch=37703;"
	const elsewhere = "x-anthropic-billing-header: cc_version=2.1.329.3c4; cc_entrypoint=cli; cch=37703;"

	tests := []struct {
		name     string
		baseURL  string
		apiKey   string
		mode     string
		messages []Message
		want     string
	}{
		{"a subscription token on the official API", "https://api.anthropic.com", "sk-ant-oat01-test", modelchannel.AnthropicAuthModeOAuth, firstTurn, official},
		{"a later turn of the same conversation", "https://api.anthropic.com", "sk-ant-oat01-test", modelchannel.AnthropicAuthModeOAuth, laterTurn, official},
		{"an API key on the official API", "https://api.anthropic.com", "sk-ant-api03-test", modelchannel.AnthropicAuthModeAuto, firstTurn, official},
		{"another host", "https://relay.example.com", "sk-relay", modelchannel.AnthropicAuthModeAuto, firstTurn, elsewhere},
		{"another host, a later turn", "https://relay.example.com", "sk-relay", modelchannel.AnthropicAuthModeAuto, laterTurn, elsewhere},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstSystemText(t, tt.baseURL, tt.apiKey, tt.mode, tt.messages); got != tt.want {
				t.Fatalf("first system block = %q, want %q", got, tt.want)
			}
		})
	}
}

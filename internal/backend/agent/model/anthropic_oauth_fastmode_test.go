package modeladapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor/internal/modelchannel"
)

// TestAnthropicProviderSystemBlocksOAuth verifies the OAuth system prompt: the
// billing header, then the Claude Code identity as a block of its own, then the
// real system prompt, as the Claude CLI sends them.
func TestAnthropicProviderSystemBlocksOAuth(t *testing.T) {
	blocks := anthropicProviderSystemBlocks([]string{"REAL SYSTEM PROMPT"}, true, "BILLING HEADER")
	want := []string{"BILLING HEADER", anthropicClaudeCodeIdentitySystemText, "REAL SYSTEM PROMPT"}
	if len(blocks) != len(want) {
		t.Fatalf("expected %d system blocks, got %d: %+v", len(want), len(blocks), blocks)
	}
	for index, text := range want {
		if got := blocks[index]["text"]; got != text {
			t.Fatalf("system block %d = %q, want %q", index, got, text)
		}
	}

	withoutPrompt := anthropicProviderSystemBlocks(nil, true, "BILLING HEADER")
	if len(withoutPrompt) != 2 || withoutPrompt[0]["text"] != "BILLING HEADER" || withoutPrompt[1]["text"] != anthropicClaudeCodeIdentitySystemText {
		t.Fatalf("OAuth blocks without a system prompt = %+v, want the billing header and the identity", withoutPrompt)
	}
}

// TestAnthropicProviderSystemBlocksNonOAuth keeps API-key channels as they
// were: the billing header and the system prompt, without the identity block.
func TestAnthropicProviderSystemBlocksNonOAuth(t *testing.T) {
	blocks := anthropicProviderSystemBlocks([]string{"SYS"}, false, "BILLING HEADER")
	if len(blocks) != 2 || blocks[0]["text"] != "BILLING HEADER" || blocks[1]["text"] != "SYS" {
		t.Fatalf("non-OAuth blocks = %+v, want the billing header and the system prompt", blocks)
	}
}

func TestApplyAnthropicFastMode(t *testing.T) {
	on := map[string]any{}
	applyAnthropicFastMode(on, StreamRequest{FastMode: true})
	if on["speed"] != "fast" {
		t.Fatalf("FastMode on: speed = %v, want \"fast\"", on["speed"])
	}
	if !anthropicFastModeRequested(on, StreamRequest{FastMode: true}) {
		t.Fatal("a body with speed fast and the toggle on must count as a Fast mode request")
	}

	off := map[string]any{}
	applyAnthropicFastMode(off, StreamRequest{FastMode: false})
	if _, exists := off["speed"]; exists {
		t.Fatalf("FastMode off: speed must be absent, got %v", off["speed"])
	}

	// A user-provided speed (extra params) must not be overwritten, and it is
	// not a Fast mode request of the toggle.
	custom := map[string]any{"speed": "standard"}
	applyAnthropicFastMode(custom, StreamRequest{FastMode: true})
	if custom["speed"] != "standard" {
		t.Fatalf("existing speed overwritten: got %v, want \"standard\"", custom["speed"])
	}
	if anthropicFastModeRequested(custom, StreamRequest{FastMode: true}) {
		t.Fatal("a body that overrides speed must not count as a Fast mode request")
	}
	// speed fast without the toggle is the user's own extra param.
	if anthropicFastModeRequested(map[string]any{"speed": "fast"}, StreamRequest{}) {
		t.Fatal("speed fast without the toggle must not count as a Fast mode request")
	}
}

func TestAnthropicStandardSpeedPayloadDropsOnlySpeed(t *testing.T) {
	body := map[string]any{"model": "claude-test", "speed": "fast", "max_tokens": float64(16)}
	payload, err := anthropicStandardSpeedPayload(body)
	if err != nil {
		t.Fatalf("anthropicStandardSpeedPayload() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if _, present := decoded["speed"]; present {
		t.Fatalf("standard payload still has speed: %s", payload)
	}
	if decoded["model"] != "claude-test" || decoded["max_tokens"] != float64(16) {
		t.Fatalf("standard payload lost fields: %s", payload)
	}
	if body["speed"] != "fast" {
		t.Fatal("the original body must be left as it was")
	}
}

func anthropicBetaSet(header http.Header) map[string]struct{} {
	set := make(map[string]struct{})
	for _, feature := range strings.Split(header.Get("anthropic-beta"), ",") {
		if trimmed := strings.TrimSpace(feature); trimmed != "" {
			set[trimmed] = struct{}{}
		}
	}
	return set
}

func hasAnthropicBeta(header http.Header, feature string) bool {
	_, ok := anthropicBetaSet(header)[feature]
	return ok
}

func TestFinalizeAnthropicDynamicHeaders(t *testing.T) {
	tests := []struct {
		name      string
		oauth     bool
		fast      bool
		wantBetas []string
		wantXApp  string
	}{
		{name: "oauth and fast", oauth: true, fast: true, wantXApp: "cli", wantBetas: []string{
			modelchannel.AnthropicOAuthBetaFeature, modelchannel.AnthropicClaudeCodeBetaFeature, modelchannel.AnthropicFastModeBetaFeature,
		}},
		{name: "oauth only", oauth: true, wantXApp: "cli", wantBetas: []string{
			modelchannel.AnthropicOAuthBetaFeature, modelchannel.AnthropicClaudeCodeBetaFeature,
		}},
		{name: "fast only", fast: true, wantBetas: []string{modelchannel.AnthropicFastModeBetaFeature}},
		{name: "neither"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
			finalizeAnthropicDynamicHeaders(req, tt.oauth, tt.fast)
			betas := anthropicBetaSet(req.Header)
			if len(betas) != len(tt.wantBetas) {
				t.Fatalf("anthropic-beta = %q, want exactly %v", req.Header.Get("anthropic-beta"), tt.wantBetas)
			}
			for _, want := range tt.wantBetas {
				if _, ok := betas[want]; !ok {
					t.Fatalf("anthropic-beta %q missing; got %q", want, req.Header.Get("anthropic-beta"))
				}
			}
			if got := req.Header.Get("x-app"); got != tt.wantXApp {
				t.Fatalf("x-app = %q, want %q", got, tt.wantXApp)
			}
		})
	}
}

// TestMergeAnthropicBetaHeaderPreservesExisting ensures a user-supplied
// anthropic-beta survives and the required features are appended without dupes.
func TestMergeAnthropicBetaHeaderPreservesExisting(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14, oauth-2025-04-20")
	finalizeAnthropicDynamicHeaders(req, true, false)

	value := req.Header.Get("anthropic-beta")
	if strings.Count(value, modelchannel.AnthropicOAuthBetaFeature) != 1 {
		t.Fatalf("oauth beta duplicated or missing: %q", value)
	}
	if !strings.HasPrefix(value, "interleaved-thinking-2025-05-14") {
		t.Fatalf("existing beta dropped or reordered: %q", value)
	}
	if !hasAnthropicBeta(req.Header, modelchannel.AnthropicClaudeCodeBetaFeature) {
		t.Fatalf("claude-code beta missing: %q", value)
	}
}

func TestAnthropicFastModeRefusal(t *testing.T) {
	overage := http.Header{}
	overage.Set(anthropicOverageDisabledReasonHeader, "out_of_credits")
	tests := []struct {
		name       string
		err        error
		wantRefuse bool
		wantText   string
	}{
		{name: "account has no fast mode", err: &HTTPStatusError{StatusCode: 400, Message: "anthropic adapter status=400 body=Fast mode is not enabled for this organization"}, wantRefuse: true},
		{name: "model does not support speed", err: &HTTPStatusError{StatusCode: 400, Body: "{\"error\":{\"message\":\"'claude-haiku-4-5' does not support the `speed` parameter\"}}"}, wantRefuse: true},
		{name: "gateway does not know the field", err: &HTTPStatusError{StatusCode: 400, Message: "speed: Extra inputs are not permitted"}, wantRefuse: true},
		{name: "usage credits refused", err: &HTTPStatusError{StatusCode: 429, Headers: overage}, wantRefuse: true, wantText: "out_of_credits"},
		{name: "rate limited", err: &HTTPStatusError{StatusCode: 429}, wantRefuse: true, wantText: "rate limited"},
		{name: "overloaded", err: &HTTPStatusError{StatusCode: 529}, wantRefuse: true, wantText: "overloaded"},
		{name: "wrapped refusal", err: fmt.Errorf("provider: %w", &HTTPStatusError{StatusCode: 529}), wantRefuse: true},
		{name: "unrelated bad request", err: &HTTPStatusError{StatusCode: 400, Message: "max_tokens: must be greater than 0"}},
		{name: "unauthorized", err: &HTTPStatusError{StatusCode: 401, Message: "invalid bearer token"}},
		{name: "server error", err: &HTTPStatusError{StatusCode: 500, Message: "internal"}},
		{name: "not a status error", err: errors.New("fast mode connection reset")},
		{name: "nil", err: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := anthropicFastModeRefusal(tt.err)
			if (reason != "") != tt.wantRefuse {
				t.Fatalf("anthropicFastModeRefusal() = %q, want refusal=%v", reason, tt.wantRefuse)
			}
			if tt.wantText != "" && !strings.Contains(reason, tt.wantText) {
				t.Fatalf("anthropicFastModeRefusal() = %q, want it to mention %q", reason, tt.wantText)
			}
		})
	}
}

func TestAnthropicFastModeCooldown(t *testing.T) {
	resetAnthropicFastModeCooldowns()
	t.Cleanup(resetAnthropicFastModeCooldowns)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	key := anthropicFastModeKey("https://api.anthropic.com", "token-a", "claude-test")

	if anthropicFastModeCoolingDown(key, now) {
		t.Fatal("a key with no refusal must not be cooling down")
	}
	if got := startAnthropicFastModeCooldown(key, now, &HTTPStatusError{StatusCode: 400}); got != anthropicFastModeCooldownDefault {
		t.Fatalf("cooldown without Retry-After = %s, want %s", got, anthropicFastModeCooldownDefault)
	}
	if !anthropicFastModeCoolingDown(key, now.Add(anthropicFastModeCooldownDefault-time.Second)) {
		t.Fatal("the key must cool down until the cooldown has passed")
	}
	for _, other := range []string{
		anthropicFastModeKey("https://api.anthropic.com", "token-b", "claude-test"),
		anthropicFastModeKey("https://api.anthropic.com", "token-a", "claude-other"),
		anthropicFastModeKey("https://gateway.example", "token-a", "claude-test"),
	} {
		if anthropicFastModeCoolingDown(other, now) {
			t.Fatalf("a refusal must not cool down another credential, model or endpoint (%q)", other)
		}
	}
	if anthropicFastModeCoolingDown(key, now.Add(anthropicFastModeCooldownDefault)) {
		t.Fatal("the cooldown must end once its time has passed")
	}

	short := http.Header{}
	short.Set("Retry-After", "60")
	if got := startAnthropicFastModeCooldown(key, now, &HTTPStatusError{StatusCode: 429, Headers: short}); got != anthropicFastModeCooldownMinimum {
		t.Fatalf("cooldown for a short Retry-After = %s, want the minimum %s", got, anthropicFastModeCooldownMinimum)
	}
	long := http.Header{}
	long.Set("Retry-After", "7200")
	if got := startAnthropicFastModeCooldown(key, now, &HTTPStatusError{StatusCode: 429, Headers: long}); got != 2*time.Hour {
		t.Fatalf("cooldown for a long Retry-After = %s, want 2h", got)
	}
	if strings.Contains(key, "token-a") {
		t.Fatalf("the cooldown key must not contain the credential: %q", key)
	}
}

// anthropicCapturedRequest is one request an anthropic capture server received.
type anthropicCapturedRequest struct {
	header http.Header
	body   map[string]any
}

// newAnthropicCaptureServer starts a server that records every request and
// answers it with respond. The returned function lists the requests so far.
func newAnthropicCaptureServer(t *testing.T, respond func(request anthropicCapturedRequest, w http.ResponseWriter)) (*httptest.Server, func() []anthropicCapturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []anthropicCapturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		request := anthropicCapturedRequest{header: r.Header.Clone()}
		_ = json.Unmarshal(raw, &request.body)
		mu.Lock()
		captured = append(captured, request)
		mu.Unlock()
		respond(request, w)
	}))
	t.Cleanup(server.Close)
	return server, func() []anthropicCapturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]anthropicCapturedRequest(nil), captured...)
	}
}

func writeAnthropicTestSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(canonicalAnthropicSSE()))
}

func writeAnthropicTestError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "invalid_request_error", "message": message},
	})
}

func anthropicFastModeTestRequest(server *httptest.Server) StreamRequest {
	return StreamRequest{
		BaseURL: server.URL + "/v1", APIKey: "token", ProviderModelID: "claude-test", ModelCallID: "call-1",
		FastMode:     true,
		RequestKnobs: map[string]any{"stream": true},
		Messages:     []Message{{Role: "user", Content: "hi"}},
	}
}

// TestAnthropicStreamOAuthRequestShape checks the request a Claude subscription
// token is sent with: bearer auth only, the Claude Code beta features, x-app,
// the claude-cli user agent, and the identity as a system block of its own
// between the billing header and the real system prompt.
func TestAnthropicStreamOAuthRequestShape(t *testing.T) {
	server, requests := newAnthropicCaptureServer(t, func(_ anthropicCapturedRequest, w http.ResponseWriter) {
		writeAnthropicTestSSE(w)
	})
	adapter := &AnthropicAdapter{client: server.Client()}
	events, err := runAnthropicTestStream(t, adapter, StreamRequest{
		BaseURL: server.URL + "/v1", APIKey: "sk-ant-oat01-test", ProviderModelID: "claude-test", ModelCallID: "call-1",
		AnthropicAuthMode: modelchannel.AnthropicAuthModeOAuth,
		Messages:          []Message{{Role: "system", Content: "REAL SYSTEM PROMPT"}, {Role: "user", Content: "hi"}},
	}, "")
	if err != nil || countAnthropicTurnFinished(events) != 1 {
		t.Fatalf("Stream() error = %v, events = %+v", err, events)
	}
	got := requests()
	if len(got) != 1 {
		t.Fatalf("server received %d requests, want 1", len(got))
	}
	header := got[0].header
	if auth := header.Get("Authorization"); auth != "Bearer sk-ant-oat01-test" {
		t.Fatalf("Authorization = %q, want the bearer token", auth)
	}
	if key := header.Get("x-api-key"); key != "" {
		t.Fatalf("x-api-key = %q, want none for OAuth", key)
	}
	for _, beta := range []string{modelchannel.AnthropicOAuthBetaFeature, modelchannel.AnthropicClaudeCodeBetaFeature} {
		if !hasAnthropicBeta(header, beta) {
			t.Fatalf("anthropic-beta %q missing; got %q", beta, header.Get("anthropic-beta"))
		}
	}
	if hasAnthropicBeta(header, modelchannel.AnthropicFastModeBetaFeature) {
		t.Fatalf("fast-mode beta sent without Fast mode: %q", header.Get("anthropic-beta"))
	}
	if app := header.Get("x-app"); app != "cli" {
		t.Fatalf("x-app = %q, want cli", app)
	}
	if agent := header.Get("User-Agent"); !strings.HasPrefix(agent, "claude-cli/") {
		t.Fatalf("User-Agent = %q, want a claude-cli agent", agent)
	}
	system, _ := got[0].body["system"].([]any)
	// The test server is not Anthropic's own API, so the fingerprint in the
	// billing header does not follow the first user message.
	wantSystem := []string{anthropicBillingHeader(""), anthropicClaudeCodeIdentitySystemText, "REAL SYSTEM PROMPT"}
	if len(system) != len(wantSystem) {
		t.Fatalf("system has %d blocks, want the billing header, the identity and the real prompt: %+v", len(system), got[0].body["system"])
	}
	for index, text := range wantSystem {
		if block, _ := system[index].(map[string]any); block["text"] != text {
			t.Fatalf("system block %d = %+v, want exactly %q", index, system[index], text)
		}
	}
	if _, present := got[0].body["speed"]; present {
		t.Fatalf("speed sent without Fast mode: %+v", got[0].body["speed"])
	}
}

// TestAnthropicStreamAutoModeRecognizesSubscriptionToken covers the default
// auto mode on the official API: a subscription token is sent as an OAuth
// request, and an API key is sent as before.
func TestAnthropicStreamAutoModeRecognizesSubscriptionToken(t *testing.T) {
	send := func(t *testing.T, apiKey string) anthropicCapturedRequest {
		t.Helper()
		var captured anthropicCapturedRequest
		adapter := &AnthropicAdapter{client: &http.Client{Transport: anthropicRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(request.Body)
			captured = anthropicCapturedRequest{header: request.Header.Clone()}
			_ = json.Unmarshal(raw, &captured.body)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(canonicalAnthropicSSE()))}, nil
		})}}
		events, err := runAnthropicTestStream(t, adapter, StreamRequest{
			BaseURL: "https://api.anthropic.com", APIKey: apiKey, ProviderModelID: "claude-test", ModelCallID: "call-1",
			AnthropicAuthMode: modelchannel.AnthropicAuthModeAuto,
			Messages:          []Message{{Role: "system", Content: "REAL SYSTEM PROMPT"}, {Role: "user", Content: "hi"}},
		}, "")
		if err != nil || countAnthropicTurnFinished(events) != 1 {
			t.Fatalf("Stream() error = %v, events = %+v", err, events)
		}
		return captured
	}
	systemTexts := func(request anthropicCapturedRequest) []string {
		blocks, _ := request.body["system"].([]any)
		texts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			entry, _ := block.(map[string]any)
			text, _ := entry["text"].(string)
			texts = append(texts, text)
		}
		return texts
	}

	subscription := send(t, "sk-ant-oat01-test")
	if subscription.header.Get("Authorization") != "Bearer sk-ant-oat01-test" || subscription.header.Get("x-api-key") != "" {
		t.Fatalf("subscription token headers: Authorization = %q, x-api-key = %q, want bearer only", subscription.header.Get("Authorization"), subscription.header.Get("x-api-key"))
	}
	if !hasAnthropicBeta(subscription.header, modelchannel.AnthropicOAuthBetaFeature) || subscription.header.Get("x-app") != "cli" {
		t.Fatalf("subscription token request lacks the OAuth headers: anthropic-beta = %q, x-app = %q", subscription.header.Get("anthropic-beta"), subscription.header.Get("x-app"))
	}
	if texts := systemTexts(subscription); len(texts) != 3 || texts[1] != anthropicClaudeCodeIdentitySystemText {
		t.Fatalf("subscription token system blocks = %q, want the identity as the second of three", texts)
	}

	apiKey := send(t, "sk-ant-api03-test")
	if apiKey.header.Get("x-api-key") != "sk-ant-api03-test" || apiKey.header.Get("Authorization") != "" {
		t.Fatalf("API key headers: x-api-key = %q, Authorization = %q, want x-api-key only", apiKey.header.Get("x-api-key"), apiKey.header.Get("Authorization"))
	}
	if apiKey.header.Get("anthropic-beta") != "" || apiKey.header.Get("x-app") != "" {
		t.Fatalf("API key request carries OAuth headers: anthropic-beta = %q, x-app = %q", apiKey.header.Get("anthropic-beta"), apiKey.header.Get("x-app"))
	}
	if texts := systemTexts(apiKey); len(texts) != 2 || texts[0] != anthropicBillingHeader("hi") || texts[1] != "REAL SYSTEM PROMPT" {
		t.Fatalf("API key system blocks = %q, want the billing header and the prompt", texts)
	}
}

func TestAnthropicStreamFastModeSendsSpeedAndBeta(t *testing.T) {
	resetAnthropicFastModeCooldowns()
	t.Cleanup(resetAnthropicFastModeCooldowns)
	server, requests := newAnthropicCaptureServer(t, func(_ anthropicCapturedRequest, w http.ResponseWriter) {
		writeAnthropicTestSSE(w)
	})
	adapter := &AnthropicAdapter{client: server.Client()}
	req := anthropicFastModeTestRequest(server)
	events, err := runAnthropicTestStream(t, adapter, req, "")
	if err != nil || countAnthropicTurnFinished(events) != 1 {
		t.Fatalf("Stream() error = %v, events = %+v", err, events)
	}
	got := requests()
	if len(got) != 1 {
		t.Fatalf("server received %d requests, want 1", len(got))
	}
	if got[0].body["speed"] != "fast" || !hasAnthropicBeta(got[0].header, modelchannel.AnthropicFastModeBetaFeature) {
		t.Fatalf("Fast mode request: speed = %v, anthropic-beta = %q", got[0].body["speed"], got[0].header.Get("anthropic-beta"))
	}
	if hasAnthropicBeta(got[0].header, modelchannel.AnthropicOAuthBetaFeature) || got[0].header.Get("x-app") != "" {
		t.Fatalf("OAuth headers sent on an API-key channel: %+v", got[0].header)
	}
	if knob := req.RequestKnobs["anthropic_fast_mode"]; knob != "fast" {
		t.Fatalf("anthropic_fast_mode knob = %v, want fast", knob)
	}
}

// TestAnthropicStreamFastModeFallsBackToStandardSpeed covers a provider that
// refuses Fast mode: the same request is sent again without it and succeeds,
// and later requests leave Fast mode off without asking.
func TestAnthropicStreamFastModeFallsBackToStandardSpeed(t *testing.T) {
	refusals := []struct {
		name    string
		respond func(w http.ResponseWriter)
	}{
		{name: "model does not support speed", respond: func(w http.ResponseWriter) {
			writeAnthropicTestError(w, http.StatusBadRequest, "'claude-test' does not support the `speed` parameter")
		}},
		{name: "account has no fast mode", respond: func(w http.ResponseWriter) {
			writeAnthropicTestError(w, http.StatusBadRequest, "Fast mode is not enabled for your organization")
		}},
		{name: "usage credits refused", respond: func(w http.ResponseWriter) {
			w.Header().Set(anthropicOverageDisabledReasonHeader, "out_of_credits")
			writeAnthropicTestError(w, http.StatusTooManyRequests, "usage credits are required")
		}},
		{name: "overloaded", respond: func(w http.ResponseWriter) {
			writeAnthropicTestError(w, anthropicOverloadedStatus, "Overloaded")
		}},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			resetAnthropicFastModeCooldowns()
			t.Cleanup(resetAnthropicFastModeCooldowns)
			server, requests := newAnthropicCaptureServer(t, func(request anthropicCapturedRequest, w http.ResponseWriter) {
				if request.body["speed"] == "fast" {
					refusal.respond(w)
					return
				}
				writeAnthropicTestSSE(w)
			})
			adapter := &AnthropicAdapter{client: server.Client()}

			first := anthropicFastModeTestRequest(server)
			events, err := runAnthropicTestStream(t, adapter, first, "")
			if err != nil || countAnthropicTurnFinished(events) != 1 {
				t.Fatalf("Stream() error = %v, events = %+v", err, events)
			}
			got := requests()
			if len(got) != 2 {
				t.Fatalf("server received %d requests, want the Fast mode attempt and one at standard speed", len(got))
			}
			if got[0].body["speed"] != "fast" || !hasAnthropicBeta(got[0].header, modelchannel.AnthropicFastModeBetaFeature) {
				t.Fatalf("first attempt was not a Fast mode request: speed = %v, anthropic-beta = %q", got[0].body["speed"], got[0].header.Get("anthropic-beta"))
			}
			if _, present := got[1].body["speed"]; present || hasAnthropicBeta(got[1].header, modelchannel.AnthropicFastModeBetaFeature) {
				t.Fatalf("second attempt still asks for Fast mode: speed = %v, anthropic-beta = %q", got[1].body["speed"], got[1].header.Get("anthropic-beta"))
			}
			delete(got[0].body, "speed")
			fastBody, _ := json.Marshal(got[0].body)
			standardBody, _ := json.Marshal(got[1].body)
			if string(fastBody) != string(standardBody) {
				t.Fatalf("the standard speed attempt must be the same request without speed:\n fast:     %s\n standard: %s", fastBody, standardBody)
			}
			if knob, _ := first.RequestKnobs["anthropic_fast_mode"].(string); !strings.Contains(knob, "fast mode refused") {
				t.Fatalf("anthropic_fast_mode knob = %q, want it to report the refusal", knob)
			}

			second := anthropicFastModeTestRequest(server)
			if _, err := runAnthropicTestStream(t, adapter, second, ""); err != nil {
				t.Fatalf("Stream() during the cooldown error = %v", err)
			}
			got = requests()
			if len(got) != 3 {
				t.Fatalf("server received %d requests, want one more during the cooldown", len(got))
			}
			if _, present := got[2].body["speed"]; present || hasAnthropicBeta(got[2].header, modelchannel.AnthropicFastModeBetaFeature) {
				t.Fatalf("a request during the cooldown asked for Fast mode: speed = %v, anthropic-beta = %q", got[2].body["speed"], got[2].header.Get("anthropic-beta"))
			}
			if knob, _ := second.RequestKnobs["anthropic_fast_mode"].(string); !strings.Contains(knob, "cooling down") {
				t.Fatalf("anthropic_fast_mode knob = %q, want it to report the cooldown", knob)
			}
		})
	}
}

// TestAnthropicStreamFastModeKeepsUnrelatedErrors covers an error that is not
// about Fast mode: it is returned as it is, nothing is sent again, and Fast
// mode is not cooled down.
func TestAnthropicStreamFastModeKeepsUnrelatedErrors(t *testing.T) {
	resetAnthropicFastModeCooldowns()
	t.Cleanup(resetAnthropicFastModeCooldowns)
	server, requests := newAnthropicCaptureServer(t, func(_ anthropicCapturedRequest, w http.ResponseWriter) {
		writeAnthropicTestError(w, http.StatusUnauthorized, "invalid bearer token")
	})
	adapter := &AnthropicAdapter{client: server.Client()}
	req := anthropicFastModeTestRequest(server)
	_, err := runAnthropicTestStream(t, adapter, req, "")
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Stream() error = %v, want the provider's 401", err)
	}
	if got := requests(); len(got) != 1 {
		t.Fatalf("server received %d requests, want 1", len(got))
	}
	if anthropicFastModeCoolingDown(anthropicFastModeKey(server.URL+"/v1", "token", "claude-test"), time.Now()) {
		t.Fatal("an unrelated error must not cool down Fast mode")
	}
}

// TestAnthropicStreamFastModeReportsStandardSpeedFailure covers a request that
// fails at standard speed too: the error of the standard speed attempt is the
// one reported, since that is the request that counts.
func TestAnthropicStreamFastModeReportsStandardSpeedFailure(t *testing.T) {
	resetAnthropicFastModeCooldowns()
	t.Cleanup(resetAnthropicFastModeCooldowns)
	server, requests := newAnthropicCaptureServer(t, func(request anthropicCapturedRequest, w http.ResponseWriter) {
		if request.body["speed"] == "fast" {
			writeAnthropicTestError(w, http.StatusTooManyRequests, "fast mode rate limit")
			return
		}
		writeAnthropicTestError(w, http.StatusForbidden, "subscription limit reached")
	})
	adapter := &AnthropicAdapter{client: server.Client()}
	_, err := runAnthropicTestStream(t, adapter, anthropicFastModeTestRequest(server), "")
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusForbidden {
		t.Fatalf("Stream() error = %v, want the 403 of the standard speed attempt", err)
	}
	if got := requests(); len(got) != 2 {
		t.Fatalf("server received %d requests, want 2", len(got))
	}
}

// TestAnthropicStreamFastModeLeavesUserSpeedAlone covers extra params that set
// speed themselves: the toggle does not replace the value and does not send the
// fast-mode beta for it.
func TestAnthropicStreamFastModeLeavesUserSpeedAlone(t *testing.T) {
	resetAnthropicFastModeCooldowns()
	t.Cleanup(resetAnthropicFastModeCooldowns)
	server, requests := newAnthropicCaptureServer(t, func(_ anthropicCapturedRequest, w http.ResponseWriter) {
		writeAnthropicTestSSE(w)
	})
	adapter := &AnthropicAdapter{client: server.Client()}
	req := anthropicFastModeTestRequest(server)
	req.AnthropicExtraParamsEnabled = true
	req.AnthropicExtraParamsJSON = `{"speed":"standard"}`
	if _, err := runAnthropicTestStream(t, adapter, req, ""); err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := requests()
	if len(got) != 1 || got[0].body["speed"] != "standard" {
		t.Fatalf("requests = %d, speed = %v, want the user's speed in one request", len(got), got[0].body["speed"])
	}
	if hasAnthropicBeta(got[0].header, modelchannel.AnthropicFastModeBetaFeature) {
		t.Fatalf("fast-mode beta sent for a request that does not ask for Fast mode: %q", got[0].header.Get("anthropic-beta"))
	}
}

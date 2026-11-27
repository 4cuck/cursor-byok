package modelchannel

import "testing"

func TestNormalizeAnthropicAuthModeOAuth(t *testing.T) {
	for _, value := range []string{"oauth", "OAuth", " OAUTH "} {
		if got := NormalizeAnthropicAuthMode(value); got != AnthropicAuthModeOAuth {
			t.Fatalf("NormalizeAnthropicAuthMode(%q) = %q, want %q", value, got, AnthropicAuthModeOAuth)
		}
	}
}

func TestAnthropicGeneratedAuthHeadersOAuth(t *testing.T) {
	token := "sk-ant-oat01-abc123"
	headers := AnthropicGeneratedAuthHeaders("https://api.anthropic.com/v1/messages", AnthropicAuthModeOAuth, token)
	if got := headers.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("Authorization = %q, want bearer token", got)
	}
	if got := headers.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key must be empty for OAuth, got %q", got)
	}
}

func TestIsAnthropicOAuthToken(t *testing.T) {
	tests := map[string]bool{
		"sk-ant-oat01-abc":        true,
		"Bearer sk-ant-oat01-abc": true,
		"  sk-ant-oat02-abc  ":    true,
		"sk-ant-api03-abc":        false,
		"sk-ant-ort01-abc":        false,
		"token":                   false,
		"":                        false,
	}
	for value, want := range tests {
		if got := IsAnthropicOAuthToken(value); got != want {
			t.Fatalf("IsAnthropicOAuthToken(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestResolveAnthropicAuthMode(t *testing.T) {
	const (
		official   = "https://api.anthropic.com/v1/messages"
		gateway    = "https://gateway.example/v1/messages"
		deceptive  = "https://api.anthropic.com.gateway.example/v1/messages"
		oauthToken = "sk-ant-oat01-token"
		apiKey     = "sk-ant-api03-key"
	)
	tests := []struct {
		name string
		url  string
		mode string
		key  string
		want string
	}{
		{name: "auto, official API, API key", url: official, mode: AnthropicAuthModeAuto, key: apiKey, want: AnthropicAuthModeAPIKey},
		{name: "auto, official API, subscription token", url: official, mode: AnthropicAuthModeAuto, key: oauthToken, want: AnthropicAuthModeOAuth},
		{name: "auto, gateway, subscription token", url: gateway, mode: AnthropicAuthModeAuto, key: oauthToken, want: AnthropicAuthModeLegacyDual},
		{name: "auto, gateway, API key", url: gateway, mode: AnthropicAuthModeAuto, key: apiKey, want: AnthropicAuthModeLegacyDual},
		{name: "auto, host that only looks official", url: deceptive, mode: AnthropicAuthModeAuto, key: oauthToken, want: AnthropicAuthModeLegacyDual},
		{name: "explicit x_api_key is kept for a subscription token", url: official, mode: AnthropicAuthModeAPIKey, key: oauthToken, want: AnthropicAuthModeAPIKey},
		{name: "explicit bearer is kept for a subscription token", url: official, mode: AnthropicAuthModeBearer, key: oauthToken, want: AnthropicAuthModeBearer},
		{name: "explicit oauth on a gateway", url: gateway, mode: AnthropicAuthModeOAuth, key: apiKey, want: AnthropicAuthModeOAuth},
		{name: "missing mode stays legacy dual", url: official, mode: "", key: oauthToken, want: AnthropicAuthModeLegacyDual},
		{name: "invalid mode", url: official, mode: "nope", key: apiKey, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveAnthropicAuthMode(tt.url, tt.mode, tt.key); got != tt.want {
				t.Fatalf("ResolveAnthropicAuthMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAnthropicGeneratedAuthHeadersAutoSubscriptionToken covers auto mode on
// the official API: a subscription token goes out as a bearer token, because
// the API refuses it in x-api-key, and an API key still goes out in x-api-key.
func TestAnthropicGeneratedAuthHeadersAutoSubscriptionToken(t *testing.T) {
	const official = "https://api.anthropic.com/v1/messages"
	token := "sk-ant-oat01-abc123"
	headers := AnthropicGeneratedAuthHeaders(official, AnthropicAuthModeAuto, token)
	if got := headers.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("Authorization = %q, want the bearer token", got)
	}
	if got := headers.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key = %q, want none for a subscription token", got)
	}

	headers = AnthropicGeneratedAuthHeaders(official, AnthropicAuthModeAuto, "sk-ant-api03-key")
	if headers.Get("x-api-key") != "sk-ant-api03-key" || headers.Get("Authorization") != "" {
		t.Fatalf("API key headers = %+v, want x-api-key only", headers)
	}
}

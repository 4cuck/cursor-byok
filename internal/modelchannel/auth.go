package modelchannel

import (
	"net/http"
	"strings"
)

const (
	AnthropicAuthModeLegacyDual = "legacy_dual"
	AnthropicAuthModeAuto       = "auto"
	AnthropicAuthModeAPIKey     = "x_api_key"
	AnthropicAuthModeBearer     = "bearer"
	// AnthropicAuthModeOAuth authenticates with a Claude Pro/Max subscription
	// OAuth access token (sk-ant-oat01-…), the kind the Claude CLI stores in
	// ~/.claude/credentials.json. The token is sent as a bearer token only, and
	// the adapter adds what the Claude CLI sends with such a token: its OAuth
	// beta features, x-app, and the Claude Code identity system block (see
	// anthropic.go).
	AnthropicAuthModeOAuth = "oauth"
)

// The anthropic-beta features below are the ones the Claude CLI (2.1.329)
// sends. The API also answered a subscription token that was sent as a plain
// bearer token without the two OAuth features, so they are sent to match the
// CLI, not because a request is known to fail without them.
const (
	// AnthropicOAuthBetaFeature is the CLI's beta feature for OAuth tokens.
	AnthropicOAuthBetaFeature = "oauth-2025-04-20"
	// AnthropicClaudeCodeBetaFeature is sent by the CLI next to the OAuth one.
	AnthropicClaudeCodeBetaFeature = "claude-code-20250219"
	// AnthropicFastModeBetaFeature is the CLI's beta feature for the Messages
	// "speed":"fast" field of Claude Fast mode.
	AnthropicFastModeBetaFeature = "fast-mode-2026-02-01"
)

// NormalizeAnthropicAuthMode keeps legacy configurations compatible: an omitted
// value means the historic dual-header behavior. Invalid non-empty values return
// an empty string so configuration validation can reject them explicitly.
func NormalizeAnthropicAuthMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return AnthropicAuthModeLegacyDual
	case AnthropicAuthModeLegacyDual, AnthropicAuthModeAuto, AnthropicAuthModeAPIKey, AnthropicAuthModeBearer, AnthropicAuthModeOAuth:
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func AnthropicAuthToken(value string) string {
	token := strings.TrimSpace(value)
	if len(token) >= len("Bearer ") && strings.EqualFold(token[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(token[len("Bearer "):])
	}
	return token
}

// anthropicOAuthTokenPrefix starts every Claude subscription OAuth access token.
const anthropicOAuthTokenPrefix = "sk-ant-oat"

// IsAnthropicOAuthToken reports whether value is a Claude subscription OAuth
// access token rather than an API key. The official API answers such a token
// in x-api-key with 401 "invalid x-api-key"; it is accepted as a bearer token.
func IsAnthropicOAuthToken(value string) bool {
	return strings.HasPrefix(AnthropicAuthToken(value), anthropicOAuthTokenPrefix)
}

// ResolveAnthropicAuthMode returns the mode a request to finalURL with apiKey
// is authenticated in. It is mode itself, except for auto: on the official API
// that is oauth for a subscription OAuth token and x_api_key for any other
// key, and on every other host it is legacy_dual. An invalid mode gives "".
func ResolveAnthropicAuthMode(finalURL, mode, apiKey string) string {
	resolvedMode := NormalizeAnthropicAuthMode(mode)
	if resolvedMode != AnthropicAuthModeAuto {
		return resolvedMode
	}
	if URLHostForProtocol(finalURL) != "api.anthropic.com" {
		return AnthropicAuthModeLegacyDual
	}
	if IsAnthropicOAuthToken(apiKey) {
		return AnthropicAuthModeOAuth
	}
	return AnthropicAuthModeAPIKey
}

// AnthropicGeneratedAuthHeaders returns only generated credentials. Explicit
// user-provided auth headers must be applied separately and take final precedence.
func AnthropicGeneratedAuthHeaders(finalURL, mode, apiKey string) http.Header {
	token := AnthropicAuthToken(apiKey)
	headers := make(http.Header)
	if token == "" {
		return headers
	}
	switch ResolveAnthropicAuthMode(finalURL, mode, apiKey) {
	case AnthropicAuthModeAPIKey:
		headers.Set("x-api-key", token)
	case AnthropicAuthModeBearer, AnthropicAuthModeOAuth:
		// A subscription OAuth token is sent as a bearer token only; the
		// adapter attaches the Claude CLI's OAuth headers.
		headers.Set("Authorization", "Bearer "+token)
	case AnthropicAuthModeLegacyDual:
		headers.Set("x-api-key", token)
		headers.Set("Authorization", "Bearer "+token)
	}
	return headers
}

func HasExplicitAnthropicAuthHeader(headers http.Header) bool {
	for name := range headers {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "authorization", "x-api-key":
			return true
		}
	}
	return false
}

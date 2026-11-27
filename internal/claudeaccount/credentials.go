// Package claudeaccount keeps Claude subscription accounts: the OAuth access
// token the Claude CLI stores for a Pro or Max subscription, and what
// Anthropic reports about the account behind it (who it is, the plan, how much
// of the usage limits is spent).
//
// Credentials come in one way only: the user submits them. Nothing here looks
// for the Claude CLI's files on its own.
//
// Account, which is what the UI is shown of a saved account, has no token
// field. Manager.Tokens hands the tokens to Go callers, which put them into
// model rows as API keys; there they are part of the user's configuration like
// any other API key.
package claudeaccount

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Error codes. Errors cross the Wails boundary as text only, so the text of
// each error starts with its code and the UI turns the code into a sentence.
const (
	CodeImportEmpty    = "claude_subscription_import_empty"
	CodeImportTooLarge = "claude_subscription_import_too_large"
	CodeImportInvalid  = "claude_subscription_import_invalid"
	CodeImportNoToken  = "claude_subscription_import_no_token"
	CodeNotOAuthToken  = "claude_subscription_not_oauth_token"
	CodeTokenExpired   = "claude_subscription_token_expired"
	CodeTokenRejected  = "claude_subscription_token_rejected"
	CodeUnreachable    = "claude_subscription_unreachable"
	// CodeDetailsUnavailable is not a failure of an operation: it marks an
	// account whose token works but may not read the profile or the usage.
	CodeDetailsUnavailable = "claude_subscription_details_unavailable"
	CodeNotFound           = "claude_subscription_not_found"
	CodeNoUsableAccount    = "claude_subscription_no_usable_account"
	CodeStoreFailed        = "claude_subscription_store_failed"
	CodeModelsFailed       = "claude_subscription_models_failed"
)

// codedError returns an error whose text is "code: detail".
func codedError(code string, detail string) error {
	return fmt.Errorf("%s: %s", code, detail)
}

// wrapCoded returns an error whose text is "code: detail: cause" and that
// unwraps to cause.
func wrapCoded(code string, detail string, cause error) error {
	return fmt.Errorf("%s: %s: %w", code, detail, cause)
}

// maxCredentialsBytes bounds pasted input. The Claude CLI's credentials file
// is a few hundred bytes.
const maxCredentialsBytes = 64 * 1024

// oauthTokenPrefix starts every Claude subscription OAuth access token.
const oauthTokenPrefix = "sk-ant-oat"

// bareTokenPattern matches an access token pasted on its own.
var bareTokenPattern = regexp.MustCompile(`^sk-ant-oat\d*-[A-Za-z0-9_-]+$`)

// secondsCeiling separates an expiry in seconds from one in milliseconds: the
// Claude CLI writes milliseconds, and a value this small can only be seconds.
const secondsCeiling = 100_000_000_000

// Credentials is a Claude subscription credential as the Claude CLI stores it.
type Credentials struct {
	AccessToken string
	// ExpiresAtUnixMS is when the token expires, or 0 when that is not known.
	ExpiresAtUnixMS  int64
	Scopes           []string
	SubscriptionType string
	RateLimitTier    string
}

// ParseCredentials reads a credential out of what a user pasted: the contents
// of the Claude CLI's credentials file, the claudeAiOauth object inside it, or
// the access token alone.
func ParseCredentials(input string) (Credentials, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return Credentials{}, codedError(CodeImportEmpty, "nothing was provided")
	}
	if len(raw) > maxCredentialsBytes {
		return Credentials{}, codedError(CodeImportTooLarge, "the pasted text is too large to be a credentials file")
	}
	if bareTokenPattern.MatchString(raw) {
		return Credentials{AccessToken: raw}, nil
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &document); err != nil || document == nil {
		if strings.HasPrefix(raw, "sk-ant-") {
			return Credentials{}, codedError(CodeNotOAuthToken, "this is not a Claude subscription token (sk-ant-oat...)")
		}
		return Credentials{}, codedError(CodeImportInvalid, "the pasted text is neither a credentials file nor an access token")
	}
	if nested, ok := document["claudeAiOauth"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(nested, &inner); err != nil || inner == nil {
			return Credentials{}, codedError(CodeImportNoToken, "claudeAiOauth holds no access token")
		}
		document = inner
	}

	var token string
	if err := json.Unmarshal(document["accessToken"], &token); err != nil || strings.TrimSpace(token) == "" {
		return Credentials{}, codedError(CodeImportNoToken, "no claudeAiOauth.accessToken was found")
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, oauthTokenPrefix) {
		return Credentials{}, codedError(CodeNotOAuthToken, "the access token is not a Claude subscription token (sk-ant-oat...)")
	}

	credentials := Credentials{AccessToken: token}
	var expiresAt float64
	if json.Unmarshal(document["expiresAt"], &expiresAt) == nil && expiresAt > 0 {
		if expiresAt < secondsCeiling {
			expiresAt *= 1000
		}
		credentials.ExpiresAtUnixMS = int64(expiresAt)
	}
	var scopes []string
	if json.Unmarshal(document["scopes"], &scopes) == nil {
		for _, scope := range scopes {
			if scope = strings.TrimSpace(scope); scope != "" {
				credentials.Scopes = append(credentials.Scopes, scope)
			}
		}
	}
	_ = json.Unmarshal(document["subscriptionType"], &credentials.SubscriptionType)
	_ = json.Unmarshal(document["rateLimitTier"], &credentials.RateLimitTier)
	credentials.SubscriptionType = strings.TrimSpace(credentials.SubscriptionType)
	credentials.RateLimitTier = strings.TrimSpace(credentials.RateLimitTier)
	return credentials, nil
}

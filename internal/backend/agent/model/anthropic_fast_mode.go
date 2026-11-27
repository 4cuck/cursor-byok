// anthropic_fast_mode.go implements Claude Fast mode for the Anthropic adapter:
// the request field, the refusals that fall back to standard speed, and the
// cooldown that follows a refusal. The behavior follows the Claude CLI
// (claude-cli 2.1.329), which sends "speed":"fast" with the fast-mode beta and,
// when the provider refuses Fast mode, sends the request again without it.
package modeladapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// anthropicFastModeCooldownDefault is how long Fast mode stays off after
	// the provider refused it, as in the Claude CLI.
	anthropicFastModeCooldownDefault = 30 * time.Minute
	// anthropicFastModeCooldownMinimum is the shortest cooldown when the
	// provider names its own wait in Retry-After, as in the Claude CLI.
	anthropicFastModeCooldownMinimum = 10 * time.Minute
	// anthropicOverageDisabledReasonHeader explains a 429 that refuses usage
	// credits, which Fast mode on a Claude subscription is billed to.
	anthropicOverageDisabledReasonHeader = "anthropic-ratelimit-unified-overage-disabled-reason"
	// anthropicOverloadedStatus is Anthropic's "overloaded" status code.
	anthropicOverloadedStatus = 529
)

// anthropicFastModeRefusalText matches the 400 messages that refuse Fast mode
// itself: "Fast mode is not enabled" for the account, "'<model>' does not
// support the `speed` parameter", and a gateway's "speed: Extra inputs are not
// permitted". The Claude CLI falls back on the same wordings.
var anthropicFastModeRefusalText = regexp.MustCompile("(?i)fast mode|speed: extra inputs|`speed` parameter")

// applyAnthropicFastMode asks for Claude Fast mode by setting the top-level
// `speed` field. The matching beta header is attached by
// finalizeAnthropicDynamicHeaders. A value already in the body is kept.
func applyAnthropicFastMode(body map[string]any, req StreamRequest) {
	if body == nil || !req.FastMode {
		return
	}
	if _, exists := body["speed"]; exists {
		return
	}
	body["speed"] = "fast"
}

// anthropicFastModeRequested reports whether body asks for Fast mode on behalf
// of the channel's Fast toggle. Extra params and the provider compatibility
// rewrites run after applyAnthropicFastMode and may have changed the field.
func anthropicFastModeRequested(body map[string]any, req StreamRequest) bool {
	if !req.FastMode {
		return false
	}
	speed, _ := body["speed"].(string)
	return speed == "fast"
}

// anthropicStandardSpeedPayload encodes body as the same request without Fast
// mode.
func anthropicStandardSpeedPayload(body map[string]any) ([]byte, error) {
	standard := make(map[string]any, len(body))
	for key, value := range body {
		if key != "speed" {
			standard[key] = value
		}
	}
	return json.Marshal(standard)
}

// anthropicFastModeRefusal reports why err refuses Fast mode itself rather
// than the request, or "" when it does not. A request that was sent with Fast
// mode and got one of these is sent again at standard speed.
//
//   - 400 naming Fast mode or the `speed` parameter: the account has no Fast
//     mode, the model does not support it, or a gateway does not know the field
//   - 429 and 529: Fast mode has rate limits of its own and, on a Claude
//     subscription, is billed to usage credits the account may not have
func anthropicFastModeRefusal(err error) string {
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr == nil {
		return ""
	}
	switch statusErr.StatusCode {
	case http.StatusBadRequest:
		if anthropicFastModeRefusalText.MatchString(statusErr.Message) || anthropicFastModeRefusalText.MatchString(statusErr.Body) {
			return "fast mode is not available for this account or model"
		}
	case http.StatusTooManyRequests:
		if reason := strings.TrimSpace(statusErr.Headers.Get(anthropicOverageDisabledReasonHeader)); reason != "" {
			return "usage credits are not available (" + reason + ")"
		}
		return "rate limited"
	case anthropicOverloadedStatus:
		return "overloaded"
	}
	return ""
}

// anthropicFastModeCooldowns holds, per credential and model, the time until
// which Fast mode is left off after the provider refused it. Without it every
// request of an account that has no Fast mode would be sent twice.
var anthropicFastModeCooldowns = struct {
	sync.Mutex
	until map[string]time.Time
}{until: make(map[string]time.Time)}

// anthropicFastModeKey identifies one credential on one endpoint using one
// model. The key itself is not stored, only a digest of it.
func anthropicFastModeKey(baseURL string, apiKey string, modelID string) string {
	digest := sha256.Sum256([]byte(apiKey))
	return strings.ToLower(strings.TrimSpace(baseURL)) + "\x00" + hex.EncodeToString(digest[:8]) + "\x00" + strings.ToLower(strings.TrimSpace(modelID))
}

// anthropicFastModeCoolingDown reports whether Fast mode is still to be left
// off for key at now.
func anthropicFastModeCoolingDown(key string, now time.Time) bool {
	anthropicFastModeCooldowns.Lock()
	defer anthropicFastModeCooldowns.Unlock()
	until, ok := anthropicFastModeCooldowns.until[key]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(anthropicFastModeCooldowns.until, key)
	return false
}

// startAnthropicFastModeCooldown leaves Fast mode off for key after the
// refusal err and returns for how long: the provider's Retry-After when it
// names one, but no less than the minimum, and the default otherwise.
func startAnthropicFastModeCooldown(key string, now time.Time, err error) time.Duration {
	cooldown := anthropicFastModeCooldownDefault
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) && statusErr != nil {
		if retryAfter := statusErr.RetryAfter(); retryAfter > 0 {
			cooldown = max(retryAfter, anthropicFastModeCooldownMinimum)
		}
	}
	anthropicFastModeCooldowns.Lock()
	anthropicFastModeCooldowns.until[key] = now.Add(cooldown)
	anthropicFastModeCooldowns.Unlock()
	return cooldown
}

// resetAnthropicFastModeCooldowns forgets every cooldown.
func resetAnthropicFastModeCooldowns() {
	anthropicFastModeCooldowns.Lock()
	anthropicFastModeCooldowns.until = make(map[string]time.Time)
	anthropicFastModeCooldowns.Unlock()
}

package claudeaccount

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"cursor/internal/modelchannel"
)

// DefaultBaseURL is the Anthropic API a Claude subscription token is for.
const DefaultBaseURL = "https://api.anthropic.com"

// maxResponseBytes bounds how much of an answer is read.
const maxResponseBytes = 2 << 20

// maxModelPages bounds how many pages of the model list are followed.
const maxModelPages = 10

// API reads account details from Anthropic with a subscription token, through
// the endpoints the Claude CLI uses for the same purpose: the OAuth profile,
// the usage limits, and the model list.
type API struct {
	client    *http.Client
	baseURL   string
	userAgent string
}

// NewAPI returns an API that sends requests with client and identifies itself
// with userAgent.
func NewAPI(client *http.Client, userAgent string) *API {
	return NewAPIAt(DefaultBaseURL, client, userAgent)
}

// NewAPIAt is NewAPI for an Anthropic API at another address. Tests use it to
// talk to a local server.
func NewAPIAt(baseURL string, client *http.Client, userAgent string) *API {
	if client == nil {
		client = http.DefaultClient
	}
	return &API{client: client, baseURL: strings.TrimSpace(baseURL), userAgent: strings.TrimSpace(userAgent)}
}

// StatusError is an answer with a status other than 2xx.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// Profile is who a token belongs to and what plan the account is on.
type Profile struct {
	AccountUUID      string `json:"accountUuid,omitempty"`
	Email            string `json:"email,omitempty"`
	DisplayName      string `json:"displayName,omitempty"`
	OrganizationUUID string `json:"organizationUuid,omitempty"`
	OrganizationName string `json:"organizationName,omitempty"`
	// OrganizationType is Anthropic's name for the plan, such as claude_pro
	// or claude_max.
	OrganizationType string `json:"organizationType,omitempty"`
	BillingType      string `json:"billingType,omitempty"`
	RateLimitTier    string `json:"rateLimitTier,omitempty"`
	// UsageCreditsEnabled is whether the account can pay for use beyond the
	// plan's limits. It is nil when Anthropic did not say.
	UsageCreditsEnabled *bool `json:"usageCreditsEnabled,omitempty"`
}

// Usage window ids. A window is one usage limit of the plan.
const (
	WindowSession      = "5h"
	WindowWeekly       = "7d"
	WindowWeeklyOpus   = "7d-opus"
	WindowWeeklySonnet = "7d-sonnet"
	// WindowWeeklyModelPrefix starts the id of a weekly limit for one model
	// family that Anthropic names in its answer. The rest of the id is that
	// name in lower case; UsageWindow.Label is the name as Anthropic wrote it.
	WindowWeeklyModelPrefix = "7d:"
)

// UsageWindow is how much of one usage limit is spent.
type UsageWindow struct {
	ID string `json:"id"`
	// Label is Anthropic's name for the model family of a window whose id
	// starts with WindowWeeklyModelPrefix, and "" for every other window.
	Label string `json:"label,omitempty"`
	// UsedPercent is between 0 and 100.
	UsedPercent float64 `json:"usedPercent"`
	// ResetsAt is when the window starts over, in RFC 3339, or "".
	ResetsAt string `json:"resetsAt,omitempty"`
}

// UsageCredits is the state of paying for use beyond the plan's limits.
type UsageCredits struct {
	Enabled bool `json:"enabled"`
	// UsedPercent is how much of the monthly credit limit is spent, between 0
	// and 100, or nil when there is no limit or Anthropic did not say.
	UsedPercent    *float64 `json:"usedPercent,omitempty"`
	DisabledReason string   `json:"disabledReason,omitempty"`
}

// Usage is how much of the plan's limits is spent.
type Usage struct {
	Windows      []UsageWindow `json:"windows,omitempty"`
	UsageCredits *UsageCredits `json:"usageCredits,omitempty"`
}

// Model is one model the token can list.
type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
}

// Profile reads the profile of the account token belongs to.
func (api *API) Profile(ctx context.Context, token string) (Profile, error) {
	var body struct {
		Account struct {
			UUID        string `json:"uuid"`
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
			FullName    string `json:"full_name"`
		} `json:"account"`
		Organization struct {
			UUID                 string `json:"uuid"`
			Name                 string `json:"name"`
			OrganizationType     string `json:"organization_type"`
			BillingType          string `json:"billing_type"`
			RateLimitTier        string `json:"rate_limit_tier"`
			HasExtraUsageEnabled *bool  `json:"has_extra_usage_enabled"`
		} `json:"organization"`
	}
	if err := api.get(ctx, token, "/api/oauth/profile", nil, &body); err != nil {
		return Profile{}, err
	}
	displayName := strings.TrimSpace(body.Account.DisplayName)
	if displayName == "" {
		displayName = strings.TrimSpace(body.Account.FullName)
	}
	return Profile{
		AccountUUID:         strings.TrimSpace(body.Account.UUID),
		Email:               strings.TrimSpace(body.Account.Email),
		DisplayName:         displayName,
		OrganizationUUID:    strings.TrimSpace(body.Organization.UUID),
		OrganizationName:    strings.TrimSpace(body.Organization.Name),
		OrganizationType:    strings.TrimSpace(body.Organization.OrganizationType),
		BillingType:         strings.TrimSpace(body.Organization.BillingType),
		RateLimitTier:       strings.TrimSpace(body.Organization.RateLimitTier),
		UsageCreditsEnabled: body.Organization.HasExtraUsageEnabled,
	}, nil
}

// Usage reads how much of the plan's limits the account has spent.
func (api *API) Usage(ctx context.Context, token string) (Usage, error) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	}
	var body struct {
		FiveHour       *window `json:"five_hour"`
		SevenDay       *window `json:"seven_day"`
		SevenDayOpus   *window `json:"seven_day_opus"`
		SevenDaySonnet *window `json:"seven_day_sonnet"`
		ExtraUsage     *struct {
			IsEnabled      bool     `json:"is_enabled"`
			Utilization    *float64 `json:"utilization"`
			DisabledReason string   `json:"disabled_reason"`
		} `json:"extra_usage"`
		// Limits is decoded on its own, in modelScopedWindows.
		Limits json.RawMessage `json:"limits"`
	}
	if err := api.get(ctx, token, "/api/oauth/usage", nil, &body); err != nil {
		return Usage{}, err
	}
	var usage Usage
	named := make(map[string]struct{})
	for _, entry := range []struct {
		id     string
		window *window
		// family is the model family the window is for, in lower case, or "".
		family string
	}{
		{WindowSession, body.FiveHour, ""},
		{WindowWeekly, body.SevenDay, ""},
		{WindowWeeklyOpus, body.SevenDayOpus, "opus"},
		{WindowWeeklySonnet, body.SevenDaySonnet, "sonnet"},
	} {
		if entry.window == nil || entry.window.Utilization == nil {
			continue
		}
		usage.Windows = append(usage.Windows, UsageWindow{
			ID:          entry.id,
			UsedPercent: clampPercent(*entry.window.Utilization),
			ResetsAt:    strings.TrimSpace(entry.window.ResetsAt),
		})
		if entry.family != "" {
			named[entry.family] = struct{}{}
		}
	}
	usage.Windows = append(usage.Windows, modelScopedWindows(body.Limits, named)...)
	if body.ExtraUsage != nil {
		credits := &UsageCredits{
			Enabled:        body.ExtraUsage.IsEnabled,
			DisabledReason: strings.TrimSpace(body.ExtraUsage.DisabledReason),
		}
		if body.ExtraUsage.Utilization != nil {
			used := clampPercent(*body.ExtraUsage.Utilization)
			credits.UsedPercent = &used
		}
		usage.UsageCredits = credits
	}
	return usage, nil
}

// modelScopedWindows reads the weekly limits for single model families out of
// the "limits" list of the usage answer. They are the entries of kind
// "weekly_scoped" that name a model, with the spent share in "percent"; the
// Claude CLI finds them the same way. A family in skip has a window already
// and is left out.
//
// The list is newer than the rest of the answer and nothing else depends on
// it, so a list in an unexpected form gives no windows, not an error.
func modelScopedWindows(raw json.RawMessage, skip map[string]struct{}) []UsageWindow {
	var limits []struct {
		Kind     string   `json:"kind"`
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &limits) != nil {
		return nil
	}
	var windows []UsageWindow
	seen := make(map[string]struct{})
	for _, limit := range limits {
		if limit.Kind != "weekly_scoped" || limit.Percent == nil || limit.Scope == nil || limit.Scope.Model == nil {
			continue
		}
		name := strings.TrimSpace(limit.Scope.Model.DisplayName)
		if name == "" {
			continue
		}
		family := strings.ToLower(name)
		if _, has := skip[family]; has {
			continue
		}
		if _, repeated := seen[family]; repeated {
			continue
		}
		seen[family] = struct{}{}
		windows = append(windows, UsageWindow{
			ID:          WindowWeeklyModelPrefix + family,
			Label:       name,
			UsedPercent: clampPercent(*limit.Percent),
			ResetsAt:    strings.TrimSpace(limit.ResetsAt),
		})
	}
	return windows
}

// Models lists the models token can see, in the order Anthropic returns them.
func (api *API) Models(ctx context.Context, token string) ([]Model, error) {
	var models []Model
	seen := make(map[string]struct{})
	afterID := ""
	for page := 0; page < maxModelPages; page++ {
		query := url.Values{"limit": {"1000"}}
		if afterID != "" {
			query.Set("after_id", afterID)
		}
		var body struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := api.get(ctx, token, "/v1/models", query, &body); err != nil {
			return nil, err
		}
		for _, item := range body.Data {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			models = append(models, Model{ID: id, DisplayName: strings.TrimSpace(item.DisplayName)})
		}
		afterID = strings.TrimSpace(body.LastID)
		if !body.HasMore || afterID == "" || len(body.Data) == 0 {
			break
		}
	}
	return models, nil
}

// get sends an authenticated GET and decodes the JSON answer into out.
func (api *API) get(ctx context.Context, token string, path string, query url.Values, out any) error {
	target := strings.TrimRight(api.baseURL, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("anthropic-beta", modelchannel.AnthropicOAuthBetaFeature)
	if api.userAgent != "" {
		request.Header.Set("User-Agent", api.userAgent)
	}
	response, err := api.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &StatusError{Status: response.StatusCode, Message: errorMessage(data)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", path, err)
	}
	return nil
}

// errorMessage returns the message of an Anthropic error body, or a short
// excerpt of a body in another form.
func errorMessage(data []byte) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && strings.TrimSpace(body.Error.Message) != "" {
		return strings.TrimSpace(body.Error.Message)
	}
	text := strings.TrimSpace(string(data))
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

func clampPercent(value float64) float64 {
	return min(100, max(0, value))
}

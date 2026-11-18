package modeladapter

import (
	"context"
	"net/url"
	"strings"
	"time"

	legacyruntime "cursor/internal/runtime"
)

const (
	ProviderChannelHealthReady    = "ready"
	ProviderChannelHealthCooldown = "cooldown"

	ProviderDiagnosticsStateReady       = "ready"
	ProviderDiagnosticsStateUnavailable = "unavailable"
	ProviderDiagnosticsStateError       = "error"

	// cursorAccountDiagnosticHost is what the diagnostics panel shows for Cursor
	// account channels. They have no configured base URL because the account
	// gateway always talks to Cursor's own control plane.
	cursorAccountDiagnosticHost = "api2.cursor.sh"
)

// ProviderDiagnosticsSnapshot is a read-only, credential-safe view of the live router.
type ProviderDiagnosticsSnapshot struct {
	GeneratedAtUnixMS int64                       `json:"generatedAtUnixMs"`
	State             string                      `json:"state"`
	ErrorCode         string                      `json:"errorCode,omitempty"`
	RouterAvailable   bool                        `json:"routerAvailable"`
	Channels          []ProviderChannelDiagnostic `json:"channels"`
}

// ProviderChannelDiagnostic intentionally contains no credential values, full URLs,
// request headers, provider bodies, cache keys, or request-parameter JSON.
type ProviderChannelDiagnostic struct {
	ChannelID string `json:"channelId"`
	// Source tells the UI whether the channel is a third-party adapter or a
	// Cursor account model. Account models deliberately carry no API key and no
	// base URL, which otherwise looks like a misconfigured channel.
	Source                  string `json:"source,omitempty"`
	DisplayName             string `json:"displayName"`
	GroupName               string `json:"groupName,omitempty"`
	Provider                string `json:"provider"`
	ProtocolMode            string `json:"protocolMode"`
	ProtocolGroup           string `json:"protocolGroup"`
	ModelID                 string `json:"modelId"`
	EndpointScheme          string `json:"endpointScheme,omitempty"`
	EndpointHost            string `json:"endpointHost,omitempty"`
	ContextWindowTokens     int    `json:"contextWindowTokens,omitempty"`
	MaxCompletionTokens     int    `json:"maxCompletionTokens,omitempty"`
	CredentialConfigured    bool   `json:"credentialConfigured"`
	CustomHeadersConfigured bool   `json:"customHeadersConfigured"`
	HealthState             string `json:"healthState"`
	CooldownUntilUnixMS     int64  `json:"cooldownUntilUnixMs,omitempty"`
}

type diagnosticsChannelResolver interface {
	ResolvedChannelsForDiagnostics(context.Context) ([]*legacyruntime.ResolvedChannel, error)
}

// ProviderDiagnostics takes a side-effect-free snapshot. It never selects a channel,
// probes a provider, clears a cooldown, or advances round-robin state.
func (router *Router) ProviderDiagnostics(ctx context.Context) ProviderDiagnosticsSnapshot {
	now := time.Now()
	snapshot := ProviderDiagnosticsSnapshot{
		GeneratedAtUnixMS: now.UnixMilli(),
		State:             ProviderDiagnosticsStateUnavailable,
	}
	if router == nil || router.resolver == nil {
		snapshot.ErrorCode = "router_unavailable"
		return snapshot
	}
	resolver, ok := router.resolver.(diagnosticsChannelResolver)
	if !ok {
		snapshot.State = ProviderDiagnosticsStateError
		snapshot.ErrorCode = "diagnostics_resolver_unavailable"
		return snapshot
	}
	channels, err := resolver.ResolvedChannelsForDiagnostics(ctx)
	if err != nil {
		snapshot.State = ProviderDiagnosticsStateError
		snapshot.ErrorCode = "channel_resolution_failed"
		return snapshot
	}

	cooldowns := make(map[string]channelHealth)
	router.healthMu.Lock()
	for channelID, health := range router.healthByChannel {
		if health.cooldownUntil.After(now) {
			cooldowns[channelID] = health
		}
	}
	router.healthMu.Unlock()

	snapshot.State = ProviderDiagnosticsStateReady
	snapshot.RouterAvailable = true
	snapshot.Channels = make([]ProviderChannelDiagnostic, 0, len(channels))
	for _, channel := range channels {
		if channel == nil {
			continue
		}
		diagnostic := projectProviderChannelDiagnostic(channel)
		if health, cooling := cooldowns[channelHealthMapKey(channel)]; cooling {
			diagnostic.HealthState = ProviderChannelHealthCooldown
			diagnostic.CooldownUntilUnixMS = health.cooldownUntil.UnixMilli()
		}
		snapshot.Channels = append(snapshot.Channels, diagnostic)
	}
	return snapshot
}

func projectProviderChannelDiagnostic(channel *legacyruntime.ResolvedChannel) ProviderChannelDiagnostic {
	source := legacyruntime.NormalizeModelSource(channel.Source)
	accountModel := source == legacyruntime.ModelSourceCursorAccount
	diagnostic := ProviderChannelDiagnostic{
		ChannelID:               strings.TrimSpace(channel.ID),
		Source:                  source,
		DisplayName:             strings.TrimSpace(channel.Name),
		GroupName:               strings.TrimSpace(channel.GroupName),
		Provider:                strings.TrimSpace(channel.Provider),
		ProtocolMode:            strings.TrimSpace(channel.ProtocolMode),
		ProtocolGroup:           strings.TrimSpace(channel.ProtocolGroup),
		ModelID:                 strings.TrimSpace(channel.Model),
		ContextWindowTokens:     channel.ContextWindowTokens,
		MaxCompletionTokens:     channel.MaxTokens,
		CredentialConfigured:    strings.TrimSpace(channel.APIKey) != "",
		CustomHeadersConfigured: channel.CustomHeadersEnabled && strings.TrimSpace(channel.CustomHeadersJSON) != "",
		HealthState:             ProviderChannelHealthReady,
	}
	if accountModel {
		// Cursor account models never store an API key or a base URL: the account
		// gateway injects the signed-in credential at request time.
		diagnostic.CredentialConfigured = true
		diagnostic.EndpointScheme = "https"
		diagnostic.EndpointHost = cursorAccountDiagnosticHost
		if until, cooling := CursorAccountModelCooldownUntil(channel.CursorAccountID, channel.Model); cooling {
			diagnostic.HealthState = ProviderChannelHealthCooldown
			diagnostic.CooldownUntilUnixMS = until.UnixMilli()
		}
	}
	if parsed, err := url.Parse(strings.TrimSpace(channel.BaseURL)); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		diagnostic.EndpointScheme = strings.ToLower(parsed.Scheme)
		diagnostic.EndpointHost = parsed.Host
	}
	return diagnostic
}

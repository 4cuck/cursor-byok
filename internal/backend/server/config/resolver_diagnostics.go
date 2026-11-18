package config

import (
	"context"

	legacyruntime "cursor/internal/runtime"
)

// ResolvedChannelsForDiagnostics resolves every configured channel in stable
// configuration order without advancing the runtime round-robin cursor.
//
// A single unusable adapter used to fail the whole scan, which blanked the
// diagnostics panel with "channel_resolution_failed" even though every other
// channel was fine. Unresolvable adapters are now skipped; the error is only
// reported when nothing at all could be resolved.
func (manager *Manager) ResolvedChannelsForDiagnostics(_ context.Context) ([]*legacyruntime.ResolvedChannel, error) {
	if manager == nil {
		return nil, legacyruntime.ErrChannelNotAvailable
	}
	adapters, err := NormalizeModelAdapterConfigs(manager.currentConfig().ModelAdapters)
	if err != nil {
		return nil, err
	}
	channels := make([]*legacyruntime.ResolvedChannel, 0, len(adapters))
	var firstErr error
	for _, adapter := range adapters {
		channel, resolveErr := resolveModelAdapterChannel([]ModelAdapterConfig{adapter}, adapter.ID)
		if resolveErr != nil {
			if firstErr == nil {
				firstErr = resolveErr
			}
			continue
		}
		channels = append(channels, channel)
	}
	if len(channels) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return channels, nil
}

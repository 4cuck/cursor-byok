package config

import (
	"context"
	"path/filepath"
	"testing"

	legacyruntime "cursor/internal/runtime"
)

const (
	testCursorFableSpec = `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":300000,"contextTokenLimitForMaxMode":1000000,
"parameterDefinitions":[{"id":"context","parameterType":{"enumParameter":{"values":[{"value":"300k"},{"value":"1m"}]}}}],
"variants":[{"parameterValues":[{"id":"context","value":"300k"}],"isDefaultNonMaxConfig":true},
{"parameterValues":[{"id":"context","value":"1m"}],"isMaxMode":true,"isDefaultMaxConfig":true}]`
	testCursorFableOlderSpec  = testCursorFableSpec + `}`
	testCursorFableSyncedSpec = testCursorFableSpec + `,"contextWindows":[{"tokens":300000,"value":"300k","default":true},{"tokens":1000000,"value":"1m","maxMode":true}]}`
	testCursorHydrogenSpec    = `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":1000000,"contextTokenLimitForMaxMode":1000000}`
)

func testCursorAccountAdapter(modelID string, spec string, contextWindowTokens int) ModelAdapterConfig {
	return ModelAdapterConfig{
		Source:              legacyruntime.ModelSourceCursorAccount,
		DisplayName:         modelID,
		GroupName:           "cursor_account1",
		SupplierID:          "cursor_account1",
		CursorAccountID:     "account-1",
		ModelID:             modelID,
		ContextWindowTokens: contextWindowTokens,
		CursorModelSpec:     spec,
	}
}

func TestNormalizeModelAdapterConfigsUsesCursorContextWindows(t *testing.T) {
	cases := []struct {
		name    string
		adapter ModelAdapterConfig
		want    int
	}{
		{
			name:    "stale local guess becomes Cursor's window",
			adapter: testCursorAccountAdapter("gemini-4-hydrogen", testCursorHydrogenSpec, 128_000),
			want:    1_000_000,
		},
		{
			name:    "synced choice of the max window stays",
			adapter: testCursorAccountAdapter("claude-fable-5-2", testCursorFableSyncedSpec, 1_000_000),
			want:    1_000_000,
		},
		{
			name:    "window Cursor does not offer falls back to the default",
			adapter: testCursorAccountAdapter("claude-fable-5-2", testCursorFableSyncedSpec, 500_000),
			want:    300_000,
		},
		{
			name:    "older guess does not turn on max mode",
			adapter: testCursorAccountAdapter("claude-fable-5-2", testCursorFableOlderSpec, 1_000_000),
			want:    300_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeModelAdapterConfigs([]ModelAdapterConfig{tc.adapter})
			if err != nil {
				t.Fatalf("NormalizeModelAdapterConfigs() error = %v", err)
			}
			if len(got) != 1 || got[0].ContextWindowTokens != tc.want {
				t.Fatalf("adapters = %#v, want contextWindowTokens %d", got, tc.want)
			}
		})
	}
}

func TestPersistChannelContextWindowKeepsCursorWindow(t *testing.T) {
	manager, err := NewManager(context.Background(), NewStore(filepath.Join(t.TempDir(), "config.yaml"), ""))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	cfg := manager.Current()
	cfg.ModelAdapters = []ModelAdapterConfig{testCursorAccountAdapter("claude-fable-5-2", testCursorFableSyncedSpec, 1_000_000)}
	if _, err := manager.Save(context.Background(), cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	channelID := manager.Current().ModelAdapters[0].ID
	if err := manager.PersistChannelContextWindow(context.Background(), channelID, 500_000); err != nil {
		t.Fatalf("PersistChannelContextWindow() error = %v", err)
	}
	if got := manager.Current().ModelAdapters[0].ContextWindowTokens; got != 1_000_000 {
		t.Fatalf("contextWindowTokens = %d, want the selected Cursor window 1000000", got)
	}
}

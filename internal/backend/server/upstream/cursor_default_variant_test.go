package upstream

import (
	"sort"
	"strings"
	"testing"

	legacyruntime "cursor/internal/runtime"
)

// Cursor marks one variant as the default for max mode. For the models below
// it is the same variant as the normal default, with the smaller window:
// switching max mode on does not by itself select the larger window, the user
// has to pick it. A chat on the larger window re-reads that much more context
// on every tool step, so the default decides what a long chat costs.

// Gemini 4 Neon as Cursor lists it: 1m exists only as a max mode variant.
const testNeonDefaultSpec = `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":300000,"contextTokenLimitForMaxMode":1000000,
"parameterDefinitions":[
{"id":"context","parameterType":{"enumParameter":{"values":[{"value":"300k"},{"value":"1m"}]}}},
{"id":"reasoning_effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"medium"},{"value":"high"}]}}}],
"variants":[
{"parameterValues":[{"id":"context","value":"300k"},{"id":"reasoning_effort","value":"low"}]},
{"parameterValues":[{"id":"context","value":"300k"},{"id":"reasoning_effort","value":"medium"}]},
{"parameterValues":[{"id":"context","value":"300k"},{"id":"reasoning_effort","value":"high"}],"isDefaultNonMaxConfig":true,"isDefaultMaxConfig":true},
{"parameterValues":[{"id":"context","value":"1m"},{"id":"reasoning_effort","value":"low"}],"isMaxMode":true},
{"parameterValues":[{"id":"context","value":"1m"},{"id":"reasoning_effort","value":"medium"}],"isMaxMode":true},
{"parameterValues":[{"id":"context","value":"1m"},{"id":"reasoning_effort","value":"high"}],"isMaxMode":true}],
"contextWindows":[{"tokens":300000,"value":"300k","default":true},{"tokens":1000000,"value":"1m","maxMode":true}]}`

// Claude Fable 5.2 as Cursor lists it: fast is a max mode variant of either window.
const testFableDefaultSpec = `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":300000,"contextTokenLimitForMaxMode":1000000,
"parameterDefinitions":[
{"id":"context","parameterType":{"enumParameter":{"values":[{"value":"300k"},{"value":"1m"}]}}},
{"id":"effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"high"}]}}},
{"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}],
"variants":[
{"parameterValues":[{"id":"context","value":"300k"},{"id":"effort","value":"low"},{"id":"fast","value":"true"}],"isMaxMode":true},
{"parameterValues":[{"id":"context","value":"300k"},{"id":"effort","value":"low"},{"id":"fast","value":"false"}]},
{"parameterValues":[{"id":"context","value":"300k"},{"id":"effort","value":"high"},{"id":"fast","value":"true"}],"isMaxMode":true},
{"parameterValues":[{"id":"context","value":"300k"},{"id":"effort","value":"high"},{"id":"fast","value":"false"}],"isDefaultNonMaxConfig":true,"isDefaultMaxConfig":true},
{"parameterValues":[{"id":"context","value":"1m"},{"id":"effort","value":"low"},{"id":"fast","value":"false"}],"isMaxMode":true},
{"parameterValues":[{"id":"context","value":"1m"},{"id":"effort","value":"high"},{"id":"fast","value":"false"}],"isMaxMode":true}],
"contextWindows":[{"tokens":300000,"value":"300k","default":true},{"tokens":1000000,"value":"1m","maxMode":true}]}`

// servedDefaultVariants lists the variants the IDE is told to start from.
func servedDefaultVariants(t *testing.T, modelID string, spec string, flag string) []string {
	t.Helper()
	entry := cursorWindowModelEntry(t, legacyruntime.ModelAdapterConfig{
		ID:                  "cursor_account:" + modelID,
		Source:              legacyruntime.ModelSourceCursorAccount,
		DisplayName:         modelID,
		ModelID:             modelID,
		Type:                legacyruntime.ModelSourceCursorAccount,
		ContextWindowTokens: 300_000,
		CursorModelSpec:     spec,
	})
	var defaults []string
	for _, variant := range entry["variants"].([]map[string]any) {
		if variant[flag] != true {
			continue
		}
		values := cursorVariantValues(variant)
		parts := make([]string, 0, len(values))
		for id, value := range values {
			parts = append(parts, id+"="+value)
		}
		sort.Strings(parts)
		defaults = append(defaults, strings.Join(parts, " "))
	}
	return defaults
}

func TestAvailableModelsKeepsCursorsDefaultVariantForMaxMode(t *testing.T) {
	cases := []struct {
		name, modelID, spec, want string
	}{
		{"larger window stays opt-in", "gemini-4-neon", testNeonDefaultSpec, "context=300k reasoning_effort=high"},
		{"fast serving stays opt-in", "claude-fable-5-2", testFableDefaultSpec, "context=300k effort=high fast=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := servedDefaultVariants(t, tc.modelID, tc.spec, "isDefaultMaxConfig")
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("default variant for max mode = %q, want [%q]: Cursor's own default must reach the IDE unchanged", got, tc.want)
			}
			if normal := servedDefaultVariants(t, tc.modelID, tc.spec, "isDefaultNonMaxConfig"); len(normal) != 1 || normal[0] != tc.want {
				t.Fatalf("default variant without max mode = %q, want [%q]", normal, tc.want)
			}
		})
	}
}

// A default for normal mode has to be a variant normal mode can run.
func TestAvailableModelsMovesNormalDefaultOffMaxOnlyVariant(t *testing.T) {
	spec := strings.Replace(testNeonDefaultSpec,
		`{"parameterValues":[{"id":"context","value":"300k"},{"id":"reasoning_effort","value":"high"}],"isDefaultNonMaxConfig":true,"isDefaultMaxConfig":true}`,
		`{"parameterValues":[{"id":"context","value":"300k"},{"id":"reasoning_effort","value":"high"}]}`, 1)
	spec = strings.Replace(spec,
		`{"parameterValues":[{"id":"context","value":"1m"},{"id":"reasoning_effort","value":"medium"}],"isMaxMode":true}`,
		`{"parameterValues":[{"id":"context","value":"1m"},{"id":"reasoning_effort","value":"medium"}],"isMaxMode":true,"isDefaultNonMaxConfig":true,"isDefaultMaxConfig":true}`, 1)

	normal := servedDefaultVariants(t, "gemini-4-neon", spec, "isDefaultNonMaxConfig")
	if len(normal) != 1 || normal[0] != "context=300k reasoning_effort=medium" {
		t.Fatalf("default variant without max mode = %q, want the 300k variant that keeps the flagged effort", normal)
	}
	if max := servedDefaultVariants(t, "gemini-4-neon", spec, "isDefaultMaxConfig"); len(max) != 1 || max[0] != "context=1m reasoning_effort=medium" {
		t.Fatalf("default variant for max mode = %q, want Cursor's flagged 1m variant", max)
	}
}

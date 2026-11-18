package modelcontext

import (
	"reflect"
	"testing"
)

const (
	hydrogenCursorSpec = `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":1000000,"contextTokenLimitForMaxMode":1000000,
"parameterDefinitions":[{"id":"reasoning_effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"high"}]}}}],
"variants":[{"parameterValues":[{"id":"reasoning_effort","value":"low"}]},
{"parameterValues":[{"id":"reasoning_effort","value":"high"}],"isDefaultMaxConfig":true,"isDefaultNonMaxConfig":true}]}`

	fableCursorSpec = `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":300000,"contextTokenLimitForMaxMode":1000000,
"parameterDefinitions":[
{"id":"context","parameterType":{"enumParameter":{"values":[{"value":"300k"},{"value":"1m"}]}}},
{"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}],
"variants":[
{"parameterValues":[{"id":"context","value":"300k"},{"id":"fast","value":"false"}],"isDefaultNonMaxConfig":true},
{"parameterValues":[{"id":"context","value":"300k"},{"id":"fast","value":"true"}],"isMaxMode":true},
{"parameterValues":[{"id":"context","value":"1m"},{"id":"fast","value":"false"}],"isMaxMode":true,"isDefaultMaxConfig":true},
{"parameterValues":[{"id":"context","value":"1m"},{"id":"fast","value":"true"}],"isMaxMode":true}]}`

	sonnet46CursorSpec = `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":1000000,"contextTokenLimitForMaxMode":1000000,
"parameterDefinitions":[{"id":"context","parameterType":{"enumParameter":{"values":[{"value":"200k"},{"value":"1m"}]}}}],
"variants":[
{"parameterValues":[{"id":"context","value":"200k"}],"isDefaultNonMaxConfig":true},
{"parameterValues":[{"id":"context","value":"1m"}],"isMaxMode":true,"isDefaultMaxConfig":true}]}`

	syncedFableCursorSpec = `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":300000,"contextTokenLimitForMaxMode":1000000,
"parameterDefinitions":[{"id":"context","parameterType":{"enumParameter":{"values":[{"value":"300k"},{"value":"1m"}]}}}],
"variants":[
{"parameterValues":[{"id":"context","value":"300k"}],"isDefaultNonMaxConfig":true},
{"parameterValues":[{"id":"context","value":"1m"}],"isMaxMode":true,"isDefaultMaxConfig":true}],
"contextWindows":[{"tokens":300000,"value":"300k","default":true},{"tokens":1000000,"value":"1m","maxMode":true}]}`
)

func TestParseCursorContextValueMatchesIDE(t *testing.T) {
	cases := map[string]int{
		"300k":       300_000,
		"272K":       272_000,
		"1m":         1_000_000,
		"1.05M":      1_050_000,
		"1048576":    1_048_576,
		" 500k ":     500_000,
		"2b":         2_000_000_000,
		"3b":         0,
		"0":          0,
		"":           0,
		"1m tokens":  0,
		"unlimited":  0,
		"-200k":      0,
		"200k-extra": 0,
	}
	for value, want := range cases {
		if got := ParseCursorContextValue(value); got != want {
			t.Errorf("ParseCursorContextValue(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestCursorContextWindowsFollowCursorCatalog(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want []CursorContextWindow
	}{
		{
			name: "single window without context parameter",
			spec: hydrogenCursorSpec,
			want: []CursorContextWindow{{Tokens: 1_000_000, Default: true}},
		},
		{
			name: "normal window and max-only window",
			spec: fableCursorSpec,
			want: []CursorContextWindow{
				{Tokens: 300_000, Value: "300k", Default: true},
				{Tokens: 1_000_000, Value: "1m", MaxMode: true},
			},
		},
		{
			name: "default comes from the default variant below contextTokenLimit",
			spec: sonnet46CursorSpec,
			want: []CursorContextWindow{
				{Tokens: 200_000, Value: "200k", Default: true},
				{Tokens: 1_000_000, Value: "1m", MaxMode: true},
			},
		},
		{
			name: "limits without context parameter",
			spec: `{"contextTokenLimit":200000,"contextTokenLimitForMaxMode":1000000}`,
			want: []CursorContextWindow{
				{Tokens: 200_000, Default: true},
				{Tokens: 1_000_000, MaxMode: true},
			},
		},
		{
			name: "max mode only",
			spec: `{"supportsNonMaxMode":false,"contextTokenLimit":200000,"contextTokenLimitForMaxMode":1000000}`,
			want: []CursorContextWindow{{Tokens: 1_000_000, MaxMode: true, Default: true}},
		},
		{
			name: "max mode disabled",
			spec: `{"supportsMaxMode":false,"contextTokenLimit":272000,"contextTokenLimitForMaxMode":1000000}`,
			want: []CursorContextWindow{{Tokens: 272_000, Default: true}},
		},
		{
			name: "context values without variants use the normal limit",
			spec: `{"contextTokenLimit":256000,"contextTokenLimitForMaxMode":500000,
"parameterDefinitions":[{"id":"context","parameterType":{"enumParameter":{"values":[{"value":"500k"},{"value":"256k"}]}}}]}`,
			want: []CursorContextWindow{
				{Tokens: 256_000, Value: "256k", Default: true},
				{Tokens: 500_000, Value: "500k", MaxMode: true},
			},
		},
		{name: "no limits", spec: `{"contextTokenLimit":0}`},
		{name: "invalid json", spec: `{`},
		{name: "empty", spec: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CursorContextWindows(tc.spec); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("windows = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveCursorContextWindowHonorsSyncedChoices(t *testing.T) {
	cases := []struct {
		name       string
		spec       string
		configured int
		want       CursorContextWindow
		wantOK     bool
	}{
		{
			name:       "synced spec keeps an offered max window",
			spec:       syncedFableCursorSpec,
			configured: 1_000_000,
			want:       CursorContextWindow{Tokens: 1_000_000, Value: "1m", MaxMode: true},
			wantOK:     true,
		},
		{
			name:       "synced spec replaces a window Cursor does not offer",
			spec:       syncedFableCursorSpec,
			configured: 128_000,
			want:       CursorContextWindow{Tokens: 300_000, Value: "300k", Default: true},
			wantOK:     true,
		},
		{
			name:       "older spec ignores a guessed window that would turn on max mode",
			spec:       fableCursorSpec,
			configured: 1_000_000,
			want:       CursorContextWindow{Tokens: 300_000, Value: "300k", Default: true},
			wantOK:     true,
		},
		{
			name:       "older spec replaces a stale guess",
			spec:       hydrogenCursorSpec,
			configured: 128_000,
			want:       CursorContextWindow{Tokens: 1_000_000, Default: true},
			wantOK:     true,
		},
		{name: "spec without windows", spec: `{"contextTokenLimit":0}`, configured: 200_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveCursorContextWindow(tc.spec, tc.configured)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("ResolveCursorContextWindow = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestResolveCursorAccountTokensFallsBackWithoutSpec(t *testing.T) {
	if got := ResolveCursorAccountTokens("gemini-4-hydrogen", hydrogenCursorSpec, 128_000); got != 1_000_000 {
		t.Fatalf("hydrogen tokens = %d, want 1000000", got)
	}
	if got := ResolveCursorAccountTokens("custom-model", "", 123_456); got != 123_456 {
		t.Fatalf("configured tokens without spec = %d, want 123456", got)
	}
	if got, want := ResolveCursorAccountTokens("default", `{"contextTokenLimit":0}`, 0), Resolve("default", 0); got != want {
		t.Fatalf("tokens without Cursor windows = %d, want catalog value %d", got, want)
	}
}

func TestResolveRuntimeCursorContextWindowHonorsIDESelection(t *testing.T) {
	cases := []struct {
		name       string
		spec       string
		configured int
		clientCtx  string
		maxMode    bool
		want       CursorContextWindow
	}{
		{
			name:       "IDE 1m overrides 300k adapter default",
			spec:       syncedFableCursorSpec,
			configured: 300_000,
			clientCtx:  "1m",
			maxMode:    true,
			want:       CursorContextWindow{Tokens: 1_000_000, Value: "1m", MaxMode: true},
		},
		{
			name:       "IDE 300k overrides 1m adapter default",
			spec:       syncedFableCursorSpec,
			configured: 1_000_000,
			clientCtx:  "300k",
			maxMode:    false,
			want:       CursorContextWindow{Tokens: 300_000, Value: "300k", Default: true},
		},
		{
			name:       "IDE 300k with maxMode from Fast keeps 300k window",
			spec:       fableCursorSpec,
			configured: 300_000,
			clientCtx:  "300k",
			maxMode:    true,
			want:       CursorContextWindow{Tokens: 300_000, Value: "300k", Default: true},
		},
		{
			name:       "numeric token value matches Cursor window",
			spec:       syncedFableCursorSpec,
			configured: 300_000,
			clientCtx:  "1000000",
			want:       CursorContextWindow{Tokens: 1_000_000, Value: "1m", MaxMode: true},
		},
		{
			name:       "maxMode without context parameter selects max window",
			spec:       syncedFableCursorSpec,
			configured: 300_000,
			maxMode:    true,
			want:       CursorContextWindow{Tokens: 1_000_000, Value: "1m", MaxMode: true},
		},
		{
			name:       "missing client context falls back to configured synced window",
			spec:       syncedFableCursorSpec,
			configured: 1_000_000,
			want:       CursorContextWindow{Tokens: 1_000_000, Value: "1m", MaxMode: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveRuntimeCursorContextWindow(tc.spec, tc.configured, tc.clientCtx, tc.maxMode)
			if !ok || got != tc.want {
				t.Fatalf("ResolveRuntimeCursorContextWindow = %+v, %v; want %+v, true", got, ok, tc.want)
			}
		})
	}
}
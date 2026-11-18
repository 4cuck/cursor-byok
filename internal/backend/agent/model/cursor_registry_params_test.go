package modeladapter

import (
	"strings"
	"testing"
)

func TestCursorRegistryParametersCoverCatalogShapes(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		client  []ModelParameter
		effort  string
		fast    bool
		maxMode bool
		want    string
		wantMax bool
	}{
		{
			name:   "grok remaps effort and keeps context and fast",
			spec:   grok47Spec,
			client: []ModelParameter{{ID: "effort", Value: "low"}},
			want:   "context=256k,reasoning_effort=low,fast=true",
		},
		{
			name:    "grok max mode uses the max context",
			spec:    grok47Spec,
			maxMode: true,
			want:    "context=500k,reasoning_effort=high,fast=true",
			wantMax: true,
		},
		{
			name:    "grok long context selected in the IDE sets max mode",
			spec:    grok47Spec,
			client:  []ModelParameter{{ID: "context", Value: "500k"}, {ID: "reasoning_effort", Value: "high"}, {ID: "fast", Value: "true"}},
			want:    "context=500k,reasoning_effort=high,fast=true",
			wantMax: true,
		},
		{
			name:   "grok client fast false overrides the default",
			spec:   grok47Spec,
			client: []ModelParameter{{ID: "reasoning_effort", Value: "low"}, {ID: "fast", Value: "false"}},
			want:   "context=256k,reasoning_effort=low,fast=false",
		},
		{
			name:    "grok extra-high alias maps to xhigh and 500k sets max mode",
			spec:    grok47FullSpec,
			client:  []ModelParameter{{ID: "context", Value: "500k"}, {ID: "effort", Value: "extra-high"}, {ID: "fast", Value: "true"}},
			want:    "context=500k,reasoning_effort=xhigh,fast=true",
			wantMax: true,
		},
		{
			name:    "grok 500k xhigh fast from IDE sets max mode even if req.MaxMode is false",
			spec:    grok47FullSpec,
			effort:  "xhigh",
			fast:    true,
			maxMode: false,
			client:  []ModelParameter{{ID: "context", Value: "500k"}, {ID: "reasoning_effort", Value: "xhigh"}, {ID: "fast", Value: "true"}},
			want:    "context=500k,reasoning_effort=xhigh,fast=true",
			wantMax: true,
		},
		{
			name:    "grok effort alias without context fills 256k non-max default",
			spec:    grok47FullSpec,
			effort:  "xhigh",
			fast:    true,
			maxMode: false,
			client:  []ModelParameter{{ID: "effort", Value: "xhigh"}},
			want:    "context=256k,reasoning_effort=xhigh,fast=true",
			wantMax: false,
		},
		{
			name:   "gpt 5.4 maps normalized xhigh effort onto extra-high",
			spec:   gpt54Spec,
			effort: "xhigh",
			want:   "context=272k,reasoning=extra-high,fast=false",
		},
		{
			name:   "gpt 5.4 preserves none and minimal alias values",
			spec:   gpt54Spec,
			client: []ModelParameter{{ID: "effort", Value: "none"}},
			want:   "context=272k,reasoning=none,fast=false",
		},
		{
			name:   "hydrogen uses reasoning_effort only",
			spec:   hydrogenSpec,
			effort: "low",
			want:   "reasoning_effort=low",
		},
		{
			name: "gpt uses reasoning plus context and fast",
			spec: gptSpec,
			client: []ModelParameter{
				{ID: "effort", Value: "high"},
			},
			want: "context=272k,reasoning=high,fast=false",
		},
		{
			name:    "gpt max mode keeps the long context",
			spec:    gptSpec,
			maxMode: true,
			want:    "context=1m,reasoning=medium,fast=false",
			wantMax: true,
		},
		{
			name:   "glm uses reasoning",
			spec:   glmSpec,
			effort: "high",
			want:   "reasoning=high",
		},
		{
			name:   "composer has no effort parameter",
			spec:   composerSpec,
			effort: "high",
			want:   "fast=true",
		},
		{
			name:   "claude keeps thinking and context while changing effort",
			spec:   claudeSpec,
			effort: "low",
			want:   "thinking=true,context=300k,effort=low",
		},
		{
			name:   "haiku thinking ignores an effort override",
			spec:   haikuSpec,
			effort: "max",
			fast:   true,
			want:   "thinking=true",
		},
		{
			name:   "model with no registered parameters sends none",
			spec:   `{"parameterDefinitions":[],"variants":[{"isDefaultNonMaxConfig":true}]}`,
			effort: "high",
			fast:   true,
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requested := cursorAccountRequestedModel("model", tc.effort, tc.fast, tc.maxMode, tc.client, tc.spec, 0)
			if got := strings.Join(parameterPairs(requested), ","); got != tc.want {
				t.Fatalf("parameters = %q, want %q", got, tc.want)
			}
			if requested.GetMaxMode() != tc.wantMax {
				t.Fatalf("max mode = %v, want %v", requested.GetMaxMode(), tc.wantMax)
			}
		})
	}
}

func TestCursorRegistryParametersFollowIDESelectionWithConfiguredFallback(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		window  int
		client  []ModelParameter
		effort  string
		fast    bool
		maxMode bool
		want    string
		wantMax bool
	}{
		{
			name:    "IDE 1m selection overrides a 300k adapter fallback and enables max mode",
			spec:    fable52WindowSpec,
			window:  300_000,
			client:  []ModelParameter{{ID: "context", Value: "1m"}, {ID: "effort", Value: "medium"}, {ID: "fast", Value: "false"}},
			want:    "context=1m,effort=medium,fast=false",
			wantMax: true,
		},
		{
			name:    "IDE 300k selection overrides a 1m adapter fallback and runs non-max",
			spec:    fable52WindowSpec,
			window:  1_000_000,
			client:  []ModelParameter{{ID: "context", Value: "300k"}, {ID: "effort", Value: "medium"}, {ID: "fast", Value: "false"}},
			want:    "context=300k,effort=medium,fast=false",
			wantMax: false,
		},
		{
			name:    "max fallback window without client context uses Cursor's max default",
			spec:    fable52WindowSpec,
			window:  1_000_000,
			want:    "context=1m,effort=high,fast=false",
			wantMax: true,
		},
		{
			name:   "unoffered context value left from another model heals to the fallback window",
			spec:   fable52WindowSpec,
			window: 300_000,
			client: []ModelParameter{{ID: "context", Value: "500k"}, {ID: "effort", Value: "high"}, {ID: "fast", Value: "false"}},
			want:   "context=300k,effort=high,fast=false",
		},
		{
			name:    "fast on the normal window still needs max mode",
			spec:    fable52WindowSpec,
			window:  300_000,
			client:  []ModelParameter{{ID: "context", Value: "300k"}, {ID: "effort", Value: "high"}, {ID: "fast", Value: "true"}},
			want:    "context=300k,effort=high,fast=true",
			wantMax: true,
		},
		{
			name:    "unlisted fast variant on the IDE 1m window falls back to the closest 1m variant",
			spec:    gpt55WindowSpec,
			window:  272_000,
			client:  []ModelParameter{{ID: "context", Value: "1m"}, {ID: "reasoning", Value: "high"}, {ID: "fast", Value: "true"}},
			want:    "context=1m,reasoning=high,fast=false",
			wantMax: true,
		},
		{
			name:    "IDE max mode without context parameter selects the 1m variant",
			spec:    gpt55WindowSpec,
			window:  272_000,
			client:  []ModelParameter{{ID: "reasoning", Value: "high"}, {ID: "fast", Value: "false"}},
			maxMode: true,
			want:    "context=1m,reasoning=high,fast=false",
			wantMax: true,
		},
		{
			name:    "synthesized context parameter on dual-limit spec sets max mode without sending unknown parameter",
			spec:    `{"contextTokenLimit":200000,"contextTokenLimitForMaxMode":1000000,"parameterDefinitions":[{"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}],"variants":[{"parameterValues":[{"id":"fast","value":"false"}],"isDefaultNonMaxConfig":true},{"parameterValues":[{"id":"fast","value":"false"}],"isMaxMode":true,"isDefaultMaxConfig":true}],"contextWindows":[{"tokens":200000,"default":true},{"tokens":1000000,"maxMode":true}]}`,
			window:  200_000,
			client:  []ModelParameter{{ID: "context", Value: "1m"}, {ID: "fast", Value: "false"}},
			want:    "fast=false",
			wantMax: true,
		},
		{
			name:   "single window model is unchanged",
			spec:   hydrogenSpec,
			window: 1_000_000,
			effort: "low",
			want:   "reasoning_effort=low",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requested := cursorAccountRequestedModel("model", tc.effort, tc.fast, tc.maxMode, tc.client, tc.spec, tc.window)
			if got := strings.Join(parameterPairs(requested), ","); got != tc.want {
				t.Fatalf("parameters = %q, want %q", got, tc.want)
			}
			if requested.GetMaxMode() != tc.wantMax {
				t.Fatalf("max mode = %v, want %v", requested.GetMaxMode(), tc.wantMax)
			}
		})
	}
}

func TestCursorRegistryParametersWithoutSpecKeepLegacyEffort(t *testing.T) {
	requested := cursorAccountRequestedModel("claude-fable-5-2", "low", true, false, nil, "", 0)
	if got := strings.Join(parameterPairs(requested), ","); got != "effort=low,fast=true" {
		t.Fatalf("parameters = %q", got)
	}
	if requested.GetModelId() != "claude-fable-5-2" {
		t.Fatalf("model = %q", requested.GetModelId())
	}
}

// fable52WindowSpec matches Cursor's catalog: fast on 300k and every 1m variant need max mode.
const fable52WindowSpec = `{
  "supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":300000,"contextTokenLimitForMaxMode":1000000,
  "parameterDefinitions":[
    {"id":"context","parameterType":{"enumParameter":{"values":[{"value":"300k"},{"value":"1m"}]}}},
    {"id":"effort","parameterType":{"enumParameter":{"values":[{"value":"medium"},{"value":"high"}]}}},
    {"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"context","value":"300k"},{"id":"effort","value":"medium"},{"id":"fast","value":"false"}]},
    {"parameterValues":[{"id":"context","value":"300k"},{"id":"effort","value":"medium"},{"id":"fast","value":"true"}],"isMaxMode":true},
    {"parameterValues":[{"id":"context","value":"300k"},{"id":"effort","value":"high"},{"id":"fast","value":"false"}],"isDefaultNonMaxConfig":true},
    {"parameterValues":[{"id":"context","value":"300k"},{"id":"effort","value":"high"},{"id":"fast","value":"true"}],"isMaxMode":true},
    {"parameterValues":[{"id":"context","value":"1m"},{"id":"effort","value":"medium"},{"id":"fast","value":"false"}],"isMaxMode":true},
    {"parameterValues":[{"id":"context","value":"1m"},{"id":"effort","value":"medium"},{"id":"fast","value":"true"}],"isMaxMode":true},
    {"parameterValues":[{"id":"context","value":"1m"},{"id":"effort","value":"high"},{"id":"fast","value":"false"}],"isMaxMode":true,"isDefaultMaxConfig":true},
    {"parameterValues":[{"id":"context","value":"1m"},{"id":"effort","value":"high"},{"id":"fast","value":"true"}],"isMaxMode":true}
  ],
  "contextWindows":[{"tokens":300000,"value":"300k","default":true},{"tokens":1000000,"value":"1m","maxMode":true}]
}`

// gpt55WindowSpec matches Cursor's catalog: the 1m window has no fast variants.
const gpt55WindowSpec = `{
  "supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":272000,"contextTokenLimitForMaxMode":1000000,
  "parameterDefinitions":[
    {"id":"context","parameterType":{"enumParameter":{"values":[{"value":"272k"},{"value":"1m"}]}}},
    {"id":"reasoning","parameterType":{"enumParameter":{"values":[{"value":"medium"},{"value":"high"}]}}},
    {"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"context","value":"272k"},{"id":"reasoning","value":"medium"},{"id":"fast","value":"false"}],"isDefaultNonMaxConfig":true},
    {"parameterValues":[{"id":"context","value":"272k"},{"id":"reasoning","value":"medium"},{"id":"fast","value":"true"}]},
    {"parameterValues":[{"id":"context","value":"272k"},{"id":"reasoning","value":"high"},{"id":"fast","value":"false"}]},
    {"parameterValues":[{"id":"context","value":"272k"},{"id":"reasoning","value":"high"},{"id":"fast","value":"true"}]},
    {"parameterValues":[{"id":"context","value":"1m"},{"id":"reasoning","value":"medium"},{"id":"fast","value":"false"}],"isMaxMode":true,"isDefaultMaxConfig":true},
    {"parameterValues":[{"id":"context","value":"1m"},{"id":"reasoning","value":"high"},{"id":"fast","value":"false"}],"isMaxMode":true}
  ],
  "contextWindows":[{"tokens":272000,"value":"272k","default":true},{"tokens":1000000,"value":"1m","maxMode":true}]
}`

const grok47Spec = `{
  "parameterDefinitions":[
    {"id":"context","parameterType":{"enumParameter":{"values":[{"value":"256k"},{"value":"500k"}]}}},
    {"id":"reasoning_effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"high"}]}}},
    {"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"context","value":"256k"},{"id":"reasoning_effort","value":"high"},{"id":"fast","value":"true"}],"isDefaultNonMaxConfig":true},
    {"parameterValues":[{"id":"context","value":"256k"},{"id":"reasoning_effort","value":"low"},{"id":"fast","value":"true"}]},
    {"parameterValues":[{"id":"context","value":"500k"},{"id":"reasoning_effort","value":"high"},{"id":"fast","value":"true"}],"isMaxMode":true,"isDefaultMaxConfig":true}
  ]
}`

const hydrogenSpec = `{
  "parameterDefinitions":[
    {"id":"reasoning_effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"high"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"reasoning_effort","value":"high"}],"isDefaultNonMaxConfig":true},
    {"parameterValues":[{"id":"reasoning_effort","value":"low"}]}
  ]
}`

const gptSpec = `{
  "parameterDefinitions":[
    {"id":"context","parameterType":{"enumParameter":{"values":[{"value":"272k"},{"value":"1m"}]}}},
    {"id":"reasoning","parameterType":{"enumParameter":{"values":[{"value":"medium"},{"value":"high"}]}}},
    {"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"context","value":"272k"},{"id":"reasoning","value":"medium"},{"id":"fast","value":"false"}],"isDefaultNonMaxConfig":true},
    {"parameterValues":[{"id":"context","value":"1m"},{"id":"reasoning","value":"medium"},{"id":"fast","value":"false"}],"isMaxMode":true,"isDefaultMaxConfig":true}
  ]
}`

const glmSpec = `{
  "parameterDefinitions":[
    {"id":"reasoning","parameterType":{"enumParameter":{"values":[{"value":"high"},{"value":"max"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"reasoning","value":"high"}],"isDefaultNonMaxConfig":true}
  ]
}`

const composerSpec = `{
  "parameterDefinitions":[
    {"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"fast","value":"true"}],"isDefaultNonMaxConfig":true}
  ]
}`

const claudeSpec = `{
  "parameterDefinitions":[
    {"id":"thinking","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}},
    {"id":"context","parameterType":{"enumParameter":{"values":[{"value":"300k"},{"value":"1m"}]}}},
    {"id":"effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"high"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"thinking","value":"true"},{"id":"context","value":"300k"},{"id":"effort","value":"high"}],"isDefaultNonMaxConfig":true}
  ]
}`

const haikuSpec = `{
  "parameterDefinitions":[
    {"id":"thinking","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"thinking","value":"true"}],"isDefaultNonMaxConfig":true}
  ]
}`

const grok47FullSpec = `{
  "parameterDefinitions":[
    {"id":"context","parameterType":{"enumParameter":{"values":[{"value":"256k"},{"value":"500k"}]}}},
    {"id":"reasoning_effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"medium"},{"value":"high"},{"value":"xhigh"}]}}},
    {"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"context","value":"256k"},{"id":"reasoning_effort","value":"high"},{"id":"fast","value":"true"}],"isDefaultNonMaxConfig":true},
    {"parameterValues":[{"id":"context","value":"500k"},{"id":"reasoning_effort","value":"xhigh"},{"id":"fast","value":"true"}],"isMaxMode":true}
  ]
}`

const gpt54Spec = `{
  "parameterDefinitions":[
    {"id":"context","parameterType":{"enumParameter":{"values":[{"value":"272k"},{"value":"1m"}]}}},
    {"id":"reasoning","parameterType":{"enumParameter":{"values":[{"value":"none"},{"value":"low"},{"value":"medium"},{"value":"high"},{"value":"extra-high"}]}}},
    {"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"context","value":"272k"},{"id":"reasoning","value":"medium"},{"value":"fast","value":"false"}],"isDefaultNonMaxConfig":true},
    {"parameterValues":[{"id":"context","value":"1m"},{"id":"reasoning","value":"medium"},{"id":"fast","value":"false"}],"isMaxMode":true,"isDefaultMaxConfig":true}
  ]
}`

const speedTierSpec = `{
  "parameterDefinitions":[
    {"id":"effort","parameterType":{"enumParameter":{"values":[{"value":"medium"},{"value":"high"}]}}},
    {"id":"speed","parameterType":{"enumParameter":{"values":[{"value":"default"},{"value":"fast"}]}}}
  ],
  "variants":[
    {"parameterValues":[{"id":"effort","value":"medium"},{"id":"speed","value":"default"}],"isDefaultNonMaxConfig":true},
    {"parameterValues":[{"id":"effort","value":"high"},{"id":"speed","value":"fast"}]}
  ]
}`

func TestCompleteCursorRegistryParametersTranslatesFastAndSpeedTiers(t *testing.T) {
	// Model uses "speed" ("default"/"fast"), caller passes fast=true or {id:"fast", value:"true"}.
	requested := cursorAccountRequestedModel("model-with-speed", "high", true, false, nil, speedTierSpec, 0)
	if got := strings.Join(parameterPairs(requested), ","); got != "effort=high,speed=fast" {
		t.Fatalf("fast bool -> speed parameter = %q, want effort=high,speed=fast", got)
	}
	requestedExplicit := cursorAccountRequestedModel("model-with-speed", "medium", false, false, []ModelParameter{
		{ID: "fast", Value: "true"},
	}, speedTierSpec, 0)
	if got := strings.Join(parameterPairs(requestedExplicit), ","); got != "effort=medium,speed=fast" {
		t.Fatalf("explicit fast=true -> speed=fast = %q, want effort=medium,speed=fast", got)
	}

	// Model uses "fast" ("false"/"true"), caller passes {id:"speed", value:"fast"}.
	requestedReverse := cursorAccountRequestedModel("composer-2.5", "", false, false, []ModelParameter{
		{ID: "speed", Value: "fast"},
	}, composerSpec, 0)
	if got := strings.Join(parameterPairs(requestedReverse), ","); got != "fast=true" {
		t.Fatalf("explicit speed=fast -> fast=true = %q, want fast=true", got)
	}
}
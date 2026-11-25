package modeladapter

import (
	"sort"
	"strings"
	"testing"

	"cursor/gen/agentv1"
)

// A request can name max mode without sending every parameter. What it leaves
// out is filled from Cursor's default for max mode. For Claude Fable 5.2 that
// default is the normal one: 300k, high effort, not fast. Fast serving and the
// 1m window exist only as max mode variants the user picks.

const testFableRegistrySpec = `{"supportsMaxMode":true,"supportsNonMaxMode":true,"contextTokenLimit":300000,"contextTokenLimitForMaxMode":1000000,
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

func requestedParameters(model *agentv1.RequestedModel) string {
	parts := make([]string, 0, len(model.GetParameters()))
	for _, parameter := range model.GetParameters() {
		parts = append(parts, parameter.GetId()+"="+parameter.GetValue())
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func TestCursorAccountMaxModeWithoutParametersUsesCursorsDefault(t *testing.T) {
	requested := cursorAccountRequestedModel("claude-fable-5-2", "", false, true, nil, testFableRegistrySpec, 300_000)

	if got, want := requestedParameters(requested), "context=300k effort=high fast=false"; got != want {
		t.Fatalf("parameters sent upstream = %q, want %q: nothing the request left out may switch on fast serving or lower the effort", got, want)
	}
}

func TestCursorAccountMaxModeKeepsWhatTheRequestChose(t *testing.T) {
	chosen := []ModelParameter{{ID: "context", Value: "1m"}, {ID: "effort", Value: "low"}}

	requested := cursorAccountRequestedModel("claude-fable-5-2", "", false, true, chosen, testFableRegistrySpec, 300_000)

	if got, want := requestedParameters(requested), "context=1m effort=low fast=false"; got != want {
		t.Fatalf("parameters sent upstream = %q, want %q", got, want)
	}
	if !requested.GetMaxMode() {
		t.Fatal("the 1m window is a max mode variant; the request must carry max mode")
	}
}

func TestCursorAccountMaxModeFastStaysOnWhenAsked(t *testing.T) {
	requested := cursorAccountRequestedModel("claude-fable-5-2", "", true, true, nil, testFableRegistrySpec, 300_000)

	if got, want := requestedParameters(requested), "context=300k effort=high fast=true"; got != want {
		t.Fatalf("parameters sent upstream = %q, want %q", got, want)
	}
}

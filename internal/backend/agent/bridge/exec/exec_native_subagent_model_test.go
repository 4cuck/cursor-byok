package execbridge

import (
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
)

func TestRewriteNativeSubagentModelInheritsParentInsteadOfAuto(t *testing.T) {
	exec := &agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{
				ToolCallId:   "call-explore",
				SubagentType: "explore",
				ModelId:      "composer-2.5",
				Prompt:       "inspect the routes",
				ModelParameters: []*agentv1.RequestedModel_ModelParameterValue{
					{Id: "effort", Value: "medium"},
				},
			},
		},
	}
	parent := NativeSubagentParent{
		ModelID:        "claude-fable-5-2",
		ThinkingEffort: "low",
		Fast:           true,
		Overrides: map[string]runtimecore.SubagentModelOverrideSelection{
			"explore": {SubagentType: "explore", Selection: "inherit"},
		},
	}
	if err := RewriteNativeSubagentModel(exec, parent); err != nil {
		t.Fatal(err)
	}
	args := exec.GetSubagentArgs()
	if args.GetModelId() != "claude-fable-5-2" {
		t.Fatalf("model = %q, want the parent fable model", args.GetModelId())
	}
	got := map[string]string{}
	for _, parameter := range args.GetModelParameters() {
		got[parameter.GetId()] = parameter.GetValue()
	}
	if got["effort"] != "low" || got["fast"] != "true" {
		t.Fatalf("parameters = %#v, want fable 5.2 low fast", got)
	}
	if strings.Contains(args.GetModelId(), "composer") || strings.EqualFold(args.GetModelId(), "auto") {
		t.Fatalf("subagent stayed on auto: %#v", args)
	}
}

func TestRewriteNativeSubagentModelInheritsCatalogParameters(t *testing.T) {
	exec := &agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{
				SubagentType: "explore",
				ModelId:      "composer-2.5",
				ModelParameters: []*agentv1.RequestedModel_ModelParameterValue{
					{Id: "effort", Value: "medium"},
				},
			},
		},
	}
	parent := NativeSubagentParent{
		ModelID: "grok-4.7",
		Parameters: []NativeModelParameter{
			{ID: "context", Value: "256k"},
			{ID: "reasoning_effort", Value: "low"},
			{ID: "fast", Value: "true"},
		},
		Overrides: map[string]runtimecore.SubagentModelOverrideSelection{
			"explore": {Selection: "inherit"},
		},
	}
	if err := RewriteNativeSubagentModel(exec, parent); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, parameter := range exec.GetSubagentArgs().GetModelParameters() {
		got[parameter.GetId()] = parameter.GetValue()
	}
	if got["reasoning_effort"] != "low" || got["context"] != "256k" || got["fast"] != "true" || got["effort"] != "" {
		t.Fatalf("parameters = %#v", got)
	}
}

func TestRewriteNativeSubagentModelKeepsExplicitAutoWithoutInherit(t *testing.T) {
	exec := &agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{
				SubagentType: "explore",
				ModelId:      "composer-2.5",
			},
		},
	}
	err := RewriteNativeSubagentModel(exec, NativeSubagentParent{ModelID: "claude-fable-5-2", ThinkingEffort: "low", Fast: true})
	if err != nil {
		t.Fatal(err)
	}
	if exec.GetSubagentArgs().GetModelId() != "composer-2.5" {
		t.Fatalf("model = %q, want the cost-aware default left in place", exec.GetSubagentArgs().GetModelId())
	}
}

func TestRewriteNativeSubagentModelReplacesAutoAlias(t *testing.T) {
	exec := &agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{SubagentType: "generalPurpose", ModelId: "auto"},
		},
	}
	parent := NativeSubagentParent{ModelID: "claude-fable-5-2", ThinkingEffort: "low", Fast: true}
	if err := RewriteNativeSubagentModel(exec, parent); err != nil {
		t.Fatal(err)
	}
	if exec.GetSubagentArgs().GetModelId() != "claude-fable-5-2" {
		t.Fatalf("model = %q", exec.GetSubagentArgs().GetModelId())
	}
}

func TestRewriteNativeSubagentModelInheritCoversGeneralPurposeAlias(t *testing.T) {
	exec := &agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{SubagentType: "generalPurpose", ModelId: "composer-2.5"},
		},
	}
	parent := NativeSubagentParent{
		ModelID: "claude-fable-5-2",
		Overrides: map[string]runtimecore.SubagentModelOverrideSelection{
			"explore": {Selection: "inherit"},
		},
	}
	if err := RewriteNativeSubagentModel(exec, parent); err != nil {
		t.Fatal(err)
	}
	if exec.GetSubagentArgs().GetModelId() != "claude-fable-5-2" {
		t.Fatalf("model = %q, want inherit to cover generalPurpose", exec.GetSubagentArgs().GetModelId())
	}
}

func TestRewriteNativeSubagentModelLeavesOtherTypesOnAuto(t *testing.T) {
	exec := &agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{SubagentType: "shell", ModelId: "composer-2.5"},
		},
	}
	parent := NativeSubagentParent{
		ModelID: "claude-fable-5-2",
		Overrides: map[string]runtimecore.SubagentModelOverrideSelection{
			"explore": {Selection: "inherit"},
		},
	}
	if err := RewriteNativeSubagentModel(exec, parent); err != nil {
		t.Fatal(err)
	}
	if exec.GetSubagentArgs().GetModelId() != "composer-2.5" {
		t.Fatalf("model = %q, want shell left on the cost-aware default", exec.GetSubagentArgs().GetModelId())
	}
}

func TestRewriteNativeSubagentModelRejectsDisabledType(t *testing.T) {
	exec := &agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{SubagentType: "explore", ModelId: "composer-2.5"},
		},
	}
	err := RewriteNativeSubagentModel(exec, NativeSubagentParent{
		ModelID: "claude-fable-5-2",
		Overrides: map[string]runtimecore.SubagentModelOverrideSelection{
			"explore": {Selection: "disabled"},
		},
	})
	if err == nil {
		t.Fatal("expected disabled explore subagents to be rejected")
	}
	if exec.GetSubagentArgs().GetModelId() != "composer-2.5" {
		t.Fatalf("model changed on error: %q", exec.GetSubagentArgs().GetModelId())
	}
}

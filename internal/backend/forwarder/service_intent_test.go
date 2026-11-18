package forwarder

import (
	"testing"

	"cursor/gen/agentv1"
)

func TestResolveRequestedModelNameUsesShortDisplayName(t *testing.T) {
	message := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: &agentv1.AgentRunRequest{
			ModelDetails: &agentv1.ModelDetails{ModelId: "gpt-5.6", DisplayNameShort: "GPT-5.6", DisplayModelId: "gpt-5.6"},
		}},
	}
	service := &Service{}
	if got := service.resolveRequestedModelName(message, "gpt-5.6"); got != "GPT-5.6" {
		t.Fatalf("model name = %q, want GPT-5.6", got)
	}
}

func TestExtractRuntimeThinkingEffortAcceptsEffortParameter(t *testing.T) {
	if got := extractRuntimeThinkingEffort(messageWithModelParameter("effort", "high")); got != "high" {
		t.Fatalf("thinking effort: got %q, want high", got)
	}
}

func TestExtractRuntimeThinkingEffortAcceptsCustomEffortParameter(t *testing.T) {
	if got := extractRuntimeThinkingEffort(messageWithModelParameter("effort", "Adaptive")); got != "adaptive" {
		t.Fatalf("custom thinking effort: got %q, want adaptive", got)
	}
	// 无字母的自定义值（如纯数字）不可信：参数路径同样拒绝，避免误透传。
	if got := extractRuntimeThinkingEffort(messageWithModelParameter("effort", "70")); got != "" {
		t.Fatalf("numeric-only effort must be rejected: got %q", got)
	}
}

func TestSplitRuntimeThinkingEffortVariantStringAcceptsCustomEffort(t *testing.T) {
	modelID, effort := splitRuntimeThinkingEffortVariantString("channel-gpt:adaptive")
	if modelID != "channel-gpt" || effort != "adaptive" {
		t.Fatalf("variant split: got (%q, %q), want (channel-gpt, adaptive)", modelID, effort)
	}
	// 纯数字后缀保持按 modelID 处理，避免 "gpt:5" 类模型名与 ":0" 索引被误拆。
	if modelID, effort := splitRuntimeThinkingEffortVariantString("gpt:5"); modelID != "" || effort != "" {
		t.Fatalf("numeric suffix must not split: got (%q, %q)", modelID, effort)
	}
	if modelID, effort := splitRuntimeThinkingEffortVariantString("cursor_account1:grok-4.7:0"); modelID != "" || effort != "" {
		t.Fatalf("numeric variant index 0 must not split: got (%q, %q)", modelID, effort)
	}
}

func TestExtractRuntimeModelParametersPreservesRegistryIDs(t *testing.T) {
	message := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{
			RunRequest: &agentv1.AgentRunRequest{
				RequestedModel: &agentv1.RequestedModel{
					ModelId: "cursor_account1:grok-4.7",
					Parameters: []*agentv1.RequestedModel_ModelParameterValue{
						{Id: "context", Value: "256k"},
						{Id: "reasoning_effort", Value: "low"},
						{Id: "fast", Value: "true"},
					},
				},
			},
		},
	}
	if got := extractRuntimeThinkingEffort(message); got != "low" {
		t.Fatalf("thinking effort = %q", got)
	}
	parameters := extractRuntimeModelParameters(message)
	if len(parameters) != 3 || parameters[1].ID != "reasoning_effort" || parameters[1].Value != "low" {
		t.Fatalf("parameters = %#v", parameters)
	}
}

func TestExtractRuntimeThinkingEffortIgnoresFastParameter(t *testing.T) {
	if got := extractRuntimeThinkingEffort(messageWithModelParameter("fast", "true")); got != "" {
		t.Fatalf("fast parameter must not become thinking effort: got %q", got)
	}
}

func TestExtractBracketVariantRepresentationPreservesModelAndParameters(t *testing.T) {
	message := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{
			RunRequest: &agentv1.AgentRunRequest{
				RequestedModel: &agentv1.RequestedModel{
					ModelId:                       "cursor_account1:claude-fable-5-2[context=1m,effort=high,fast=true]",
					IsVariantStringRepresentation: true,
				},
			},
		},
	}
	if got := extractRequestedModelID(message); got != "cursor_account1:claude-fable-5-2" {
		t.Fatalf("model id = %q, want cursor_account1:claude-fable-5-2", got)
	}
	if got := extractRuntimeThinkingEffort(message); got != "high" {
		t.Fatalf("thinking effort = %q, want high", got)
	}
	if !extractRuntimeFastMode(message) {
		t.Fatal("fast mode = false, want true")
	}
	params := extractRuntimeModelParameters(message)
	if len(params) != 3 || params[0].ID != "context" || params[0].Value != "1m" {
		t.Fatalf("parameters = %#v", params)
	}
}

func messageWithModelParameter(id string, value string) *agentv1.AgentClientMessage {
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{
			RunRequest: &agentv1.AgentRunRequest{
				RequestedModel: &agentv1.RequestedModel{
					ModelId: "channel-gpt",
					Parameters: []*agentv1.RequestedModel_ModelParameterValue{{
						Id:    id,
						Value: value,
					}},
				},
			},
		},
	}
}
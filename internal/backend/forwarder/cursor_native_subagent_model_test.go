package forwarder

import (
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/proto"
)

func TestDispatchNativeCursorExecInheritsParentModel(t *testing.T) {
	service := NewService(t.TempDir(), nilResolver{})
	defer service.multitaskDelegation.Close()
	const requestID = "req-native-inherit"
	stream, err := service.broker.OpenStream(requestID, "conv-native-inherit", 1, "claude-fable-5-2", "Fable 5.2",
		agentv1.AgentMode_AGENT_MODE_AGENT, "use subagents")
	if err != nil {
		t.Fatal(err)
	}
	stream.ThinkingEffort = "low"
	stream.FastMode = true
	stream.SubagentModelOverrides = map[string]runtimecore.SubagentModelOverrideSelection{
		"explore": {SubagentType: "explore", Selection: "inherit"},
	}
	raw, err := proto.Marshal(&agentv1.ExecServerMessage{
		Id:     4,
		ExecId: "upstream-subagent",
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{
				ToolCallId:   "call-explore",
				SubagentType: "explore",
				ModelId:      "composer-2.5",
				Prompt:       "inspect routes",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = service.dispatchNativeCursorExec(stream, runtimecore.ToolInvocation{
		CallID:      "call-explore",
		ToolName:    "Task",
		ModelCallID: requestID,
		ArgsJSON:    []byte(`{"description":"inspect routes","prompt":"inspect routes","subagent_type":"explore","model":"auto"}`),
		NativeExec:  raw,
	})
	stream.mu.Lock()
	for _, timer := range stream.StreamTimers {
		if timer != nil {
			timer.Stop()
		}
	}
	stream.mu.Unlock()
	events, readErr := service.broker.ReadFromCursor(requestID, 0)
	if err != nil && !strings.Contains(err.Error(), "checkpoint conversation is not initialized") {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	var sawExec, sawCard bool
	for _, event := range events {
		message := event.Message
		if message == nil {
			continue
		}
		if args := message.GetExecServerMessage().GetSubagentArgs(); args != nil {
			sawExec = true
			if args.GetModelId() != "claude-fable-5-2" {
				t.Fatalf("launched model = %q, want claude-fable-5-2", args.GetModelId())
			}
			got := map[string]string{}
			for _, parameter := range args.GetModelParameters() {
				got[parameter.GetId()] = parameter.GetValue()
			}
			if got["effort"] != "low" || got["fast"] != "true" {
				t.Fatalf("parameters = %#v, want fable 5.2 low fast", got)
			}
		}
		if task := message.GetInteractionUpdate().GetToolCallStarted().GetToolCall().GetTaskToolCall(); task != nil {
			sawCard = true
			if task.GetArgs().GetModel() != "Fable 5.2" {
				t.Fatalf("task card model = %q, want Fable 5.2", task.GetArgs().GetModel())
			}
		}
	}
	if !sawExec || !sawCard {
		t.Fatalf("exec=%t card=%t events=%d", sawExec, sawCard, len(events))
	}
}

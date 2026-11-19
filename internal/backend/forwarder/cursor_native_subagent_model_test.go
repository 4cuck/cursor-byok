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

func TestDispatchNativeCursorExecRewritesParentConversationAndStoppedSubagentPreservesResumeID(t *testing.T) {
	service := NewService(t.TempDir(), nilResolver{})
	defer service.multitaskDelegation.Close()
	const (
		parentRequestID      = "req-parent-native-subagent"
		parentConversationID = "conv-parent-native-subagent"
		childRequestID       = "req-child-native-subagent"
		childConversationID  = "conv-child-native-subagent"
		toolCallID           = "call-native-subagent"
	)

	parentConv := testConversation([]HistoryEntry{
		testUserMessageEntry(t, 1, parentRequestID, "investigate subagents"),
	})
	parentConv.ConversationID = parentConversationID
	parentConv.RootConversationID = parentConversationID
	persistedParent, err := service.store.SaveConversationWithEntries(parentConversationID, parentConv, parentConv.Entries)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := service.broker.OpenStream(parentRequestID, parentConversationID, 1, "gemini-4-argon", "Gemini 4 Argon",
		agentv1.AgentMode_AGENT_MODE_AGENT, "investigate subagents")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.replaceCheckpointConversation(stream, persistedParent); err != nil {
		t.Fatal(err)
	}

	ephemeralUpstreamConvID := "ephemeral-upstream-uuid-that-does-not-exist-in-ide"
	raw, err := proto.Marshal(&agentv1.ExecServerMessage{
		Id:     11,
		ExecId: "upstream-subagent-11",
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{
				ToolCallId:               toolCallID,
				SubagentType:             "explore",
				ModelId:                  "auto",
				Prompt:                   "inspect codebase",
				ParentConversationId:     &ephemeralUpstreamConvID,
				RootParentConversationId: &ephemeralUpstreamConvID,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := service.dispatchNativeCursorExec(stream, runtimecore.ToolInvocation{
		CallID:      toolCallID,
		ToolName:    "Task",
		ModelCallID: parentRequestID,
		ArgsJSON:    []byte(`{"description":"inspect codebase","prompt":"inspect codebase","subagent_type":"explore"}`),
		NativeExec:  raw,
	}); err != nil {
		t.Fatalf("dispatchNativeCursorExec() error = %v", err)
	}

	events, err := service.broker.ReadFromCursor(parentRequestID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var forwardedArgs *agentv1.SubagentArgs
	for _, ev := range events {
		if args := ev.Message.GetExecServerMessage().GetSubagentArgs(); args != nil {
			forwardedArgs = args
		}
	}
	if forwardedArgs == nil {
		t.Fatal("expected forwarded SubagentArgs")
	}
	if got := forwardedArgs.GetParentConversationId(); got != parentConversationID {
		t.Fatalf("ParentConversationId = %q, want %q (ephemeral upstream UUID must be rewritten)", got, parentConversationID)
	}
	if got := forwardedArgs.GetRootParentConversationId(); got != parentConversationID {
		t.Fatalf("RootParentConversationId = %q, want %q", got, parentConversationID)
	}

	// Simulate the child subagent running and saving partial progress before stopping.
	service.rememberChildParentLink(childRequestID, childRunSSEHeader(parentRequestID, toolCallID))
	childConv := testConversation([]HistoryEntry{
		testUserMessageEntry(t, 1, childRequestID, "inspect codebase"),
		newAssistantTextEntry(1, childRequestID, "Found 3 key files in internal/backend/forwarder.", "", ""),
	})
	childConv.ConversationID = childConversationID
	service.applyChildParentLink(childConv, childRequestID)
	if _, err := service.store.SaveConversationWithEntries(childConversationID, childConv, childConv.Entries); err != nil {
		t.Fatal(err)
	}

	// Now cancel the parent turn (user clicked Stop while subagent was running) and verify
	// the parent conversation records a Task tool_result with childConversationID (agent_id) and partial progress.
	if err := service.handleCancelIntent(InboundIntent{
		Kind:           "cancel",
		RequestID:      parentRequestID,
		ConversationID: parentConversationID,
		CancelReason:   "user_stopped_generation",
	}); err != nil {
		t.Fatalf("handleCancelIntent() error = %v", err)
	}

	loadedParent, err := service.store.LoadConversation(parentConversationID)
	if err != nil {
		t.Fatal(err)
	}
	var foundToolResult string
	for _, entry := range loadedParent.Entries {
		if entry.Kind == "tool_result" {
			foundToolResult = string(entry.Payload)
		}
	}
	if !strings.Contains(foundToolResult, childConversationID) {
		t.Fatalf("parent Task tool_result missing child subagent resume ID %q: %s", childConversationID, foundToolResult)
	}
	if !strings.Contains(foundToolResult, "Found 3 key files in internal/backend/forwarder.") {
		t.Fatalf("parent Task tool_result missing child subagent partial progress: %s", foundToolResult)
	}
}


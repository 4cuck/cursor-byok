package forwarder

import (
	"encoding/json"
	"testing"
	"time"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/proto"
)

// backgroundedByFollowUp sets up a subagent that a new user message sent to
// the background: its exec is no longer awaited by the parent's stream, and
// whatever the client still answers for it is taken in by
// absorbBackgroundedSubagentExecResult.
func backgroundedByFollowUp(t *testing.T) (*Service, string) {
	t.Helper()
	const conversationID = "conv-parent-follow-up"
	service := NewService(t.TempDir(), nilResolver{})
	t.Cleanup(service.multitaskDelegation.Close)
	conversation := testConversation([]HistoryEntry{testUserMessageEntry(t, 1, "req-parent-follow-up", "inspect the routes")})
	conversation.ConversationID = conversationID
	if _, err := service.store.SaveConversationWithEntries(conversationID, conversation, conversation.Entries); err != nil {
		t.Fatal(err)
	}
	registerParentTaskDelegation(t, service, "req-parent-follow-up", conversationID, "call-parent-follow-up")
	service.rememberBackgroundedDelegation(backgroundedDelegationExec{
		ExecID:          "exec-parent-task",
		ExecKind:        "subagent",
		StreamState:     execStreamStateBackgrounded,
		ToolCallID:      "call-parent-follow-up",
		ConversationID:  conversationID,
		ParentRequestID: "req-parent-follow-up",
		TurnSeq:         1,
		BackgroundedAt:  time.Now().UTC(),
	})
	return service, conversationID
}

// recordedDelegationResults returns the results recorded for the model's
// next turn ("the task that was sent to the background has ended").
func recordedDelegationResults(t *testing.T, service *Service, conversationID string) []map[string]any {
	t.Helper()
	conversation, err := service.store.LoadConversation(conversationID)
	if err != nil {
		t.Fatal(err)
	}
	var results []map[string]any
	for _, entry := range conversation.Entries {
		if entry.Kind != "metadata" {
			continue
		}
		var payload metadataPayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Type == metadataTypeBackgroundedDelegationResult {
			results = append(results, payload.Value)
		}
	}
	return results
}

// The client answers the exec of such a subagent by moving it to its own
// background: an agent id, a reason, a transcript, no final message. That
// ends the exec, not the subagent. It used to be recorded as the subagent's
// result, and the model's next turn opened with "the task has ended
// (completed)" and nothing to show for it.
func TestBackgroundHandoffOfABackgroundedSubagentIsNotItsResult(t *testing.T) {
	service, conversationID := backgroundedByFollowUp(t)
	handoff := &agentv1.SubagentResult{Result: &agentv1.SubagentResult_Success{Success: &agentv1.SubagentSuccess{
		AgentId:          "3f2a9c1e-5b7d-4e68-9a10-c4d2e6f8a0b1",
		BackgroundReason: agentv1.SubagentBackgroundReason_SUBAGENT_BACKGROUND_REASON_USER_REQUEST,
		TranscriptPath:   proto.String(`C:\Users\dev\.cursor\projects\repo\agent-transcripts\3f2a9c1e.jsonl`),
	}}}

	if !service.absorbBackgroundedSubagentExecResult("exec-parent-task", subagentTerminalOutcomeFrom(handoff)) {
		t.Fatal("the answer for the backgrounded exec was not taken in")
	}
	if results := recordedDelegationResults(t, service, conversationID); len(results) != 0 {
		t.Fatalf("the handoff was recorded as the task's result: %v", results)
	}
	// Nothing more is awaited for this exec, and it holds no slot.
	if _, kept := service.backgroundedDelegationRecord("exec-parent-task"); kept {
		t.Fatal("the exec is still awaited after the client answered it")
	}
	if active := service.nativeDelegationLimiter.activeCount(); active != 0 {
		t.Fatalf("%d native delegation slots held after the handoff, want 0", active)
	}
}

// A subagent that really ends while backgrounded is still reported.
func TestFinalResultOfABackgroundedSubagentIsStillRecorded(t *testing.T) {
	service, conversationID := backgroundedByFollowUp(t)
	final := &agentv1.SubagentResult{Result: &agentv1.SubagentResult_Success{Success: &agentv1.SubagentSuccess{
		AgentId:      "3f2a9c1e-5b7d-4e68-9a10-c4d2e6f8a0b1",
		FinalMessage: proto.String("Three routes are unused."),
	}}}
	if !service.absorbBackgroundedSubagentExecResult("exec-parent-task", subagentTerminalOutcomeFrom(final)) {
		t.Fatal("the result for the backgrounded exec was not taken in")
	}
	results := recordedDelegationResults(t, service, conversationID)
	if len(results) != 1 || results[0]["result"] != "Three routes are unused." || results[0]["status"] != "completed" {
		t.Fatalf("recorded results = %v, want the subagent's final message as completed", results)
	}
}

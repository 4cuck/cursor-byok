package forwarder

import (
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	"cursor/internal/backend/delegation"

	"google.golang.org/protobuf/proto"
)

const (
	backgroundTestParentRequest      = "req-parent-background"
	backgroundTestParentConversation = "conv-parent-background"
	backgroundTestChildConversation  = "9b8a7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
)

func backgroundSubagentFixture(t *testing.T) (*Service, *ActiveStream) {
	t.Helper()
	service := NewService(t.TempDir(), nilResolver{})
	t.Cleanup(func() { service.multitaskDelegation.Close() })
	parent := testConversation([]HistoryEntry{
		testUserMessageEntry(t, 1, backgroundTestParentRequest, "run five engineers in parallel"),
	})
	parent.ConversationID = backgroundTestParentConversation
	parent.RootConversationID = backgroundTestParentConversation
	persisted, err := service.store.SaveConversationWithEntries(backgroundTestParentConversation, parent, parent.Entries)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := service.broker.OpenStream(backgroundTestParentRequest, backgroundTestParentConversation, 1, "claude-fable-5-2", "Claude Fable 5.2",
		agentv1.AgentMode_AGENT_MODE_AGENT, "run five engineers in parallel")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.replaceCheckpointConversation(stream, persisted); err != nil {
		t.Fatal(err)
	}
	// A Cursor-account turn: tool results are answered on the open stream.
	stream.CursorToolResults = make(chan *agentv1.ExecClientMessage, 8)
	t.Cleanup(func() { stream.mu.Lock(); defer stream.mu.Unlock(); stopAllStreamTimersLocked(stream) })
	return service, stream
}

// launchNativeTask dispatches Cursor's subagent exec and returns the exec the
// client was sent, which is the one its answer names.
func launchNativeTask(t *testing.T, service *Service, stream *ActiveStream, toolCallID string, runInBackground bool) *agentv1.ExecServerMessage {
	t.Helper()
	raw, err := proto.Marshal(&agentv1.ExecServerMessage{
		Id:     41,
		ExecId: "upstream-" + toolCallID,
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{
				ToolCallId:      toolCallID,
				SubagentType:    "generalPurpose",
				ModelId:         "claude-fable-5-2",
				Prompt:          "Ray-traced light on the RTX",
				RunInBackground: proto.Bool(runInBackground),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.dispatchNativeCursorExec(stream, runtimecore.ToolInvocation{
		CallID:      toolCallID,
		ToolName:    "Task",
		ModelCallID: backgroundTestParentRequest,
		ArgsJSON:    []byte(`{"description":"Ray-traced light on the RTX","prompt":"Ray-traced light on the RTX","subagent_type":"generalPurpose"}`),
		NativeExec:  raw,
	}); err != nil {
		t.Fatalf("dispatchNativeCursorExec() error = %v", err)
	}
	events, err := service.broker.ReadFromCursor(stream.RequestID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if exec := event.Message.GetExecServerMessage(); exec.GetSubagentArgs().GetToolCallId() == toolCallID {
			return exec
		}
	}
	t.Fatalf("subagent exec for %s was not sent to the client", toolCallID)
	return nil
}

func answerNativeTask(t *testing.T, service *Service, stream *ActiveStream, sent *agentv1.ExecServerMessage, result *agentv1.SubagentResult) {
	t.Helper()
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: sent.GetId(), ExecId: sent.GetExecId(),
			Message: &agentv1.ExecClientMessage_SubagentResult{SubagentResult: result},
		},
	}); err != nil {
		t.Fatalf("handleExecResult() error = %v", err)
	}
}

// publishedRunState acknowledges the checkpoint's blob writes, as the client
// does, and returns the run state of the tool call in the last checkpoint the
// client was sent.
func publishedRunState(t *testing.T, service *Service, stream *ActiveStream, toolCallID string) *agentv1.SubagentRunState {
	t.Helper()
	if err := service.publishCheckpointForce(stream.RequestID, stream.ConversationID); err != nil {
		t.Fatal(err)
	}
	acked := map[uint32]bool{}
	for round := 0; round < 20; round++ {
		events, err := service.broker.ReadFromCursor(stream.RequestID, 0)
		if err != nil {
			t.Fatal(err)
		}
		progressed := false
		for _, event := range events {
			kv := event.Message.GetKvServerMessage()
			if kv.GetSetBlobArgs() == nil || acked[kv.GetId()] {
				continue
			}
			acked[kv.GetId()] = true
			progressed = true
			if err := service.handleCheckpointBlobResult(stream, &agentv1.KvClientMessage{
				Id:      kv.GetId(),
				Message: &agentv1.KvClientMessage_SetBlobResult{SetBlobResult: &agentv1.SetBlobResult{}},
			}); err != nil {
				t.Fatal(err)
			}
		}
		if !progressed {
			break
		}
	}
	events, err := service.broker.ReadFromCursor(stream.RequestID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var last *agentv1.ConversationStateStructure
	for _, event := range events {
		if checkpoint := event.Message.GetConversationCheckpointUpdate(); checkpoint != nil {
			last = checkpoint
		}
	}
	if last == nil {
		t.Fatal("no checkpoint reached the client")
	}
	return last.GetSubagentRunsByParentToolCallId()[toolCallID]
}

func completedTaskCard(t *testing.T, service *Service, stream *ActiveStream, toolCallID string) *agentv1.TaskToolCall {
	t.Helper()
	events, err := service.broker.ReadFromCursor(stream.RequestID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var card *agentv1.TaskToolCall
	for _, event := range events {
		if completed := event.Message.GetInteractionUpdate().GetToolCallCompleted(); completed.GetCallId() == toolCallID {
			card = completed.GetToolCall().GetTaskToolCall()
		}
	}
	if card == nil {
		t.Fatalf("no completed Task card for %s", toolCallID)
	}
	return card
}

// taskCardDeltas returns the text this process wrote into the Task card.
func taskCardDeltas(t *testing.T, service *Service, stream *ActiveStream) string {
	t.Helper()
	events, err := service.broker.ReadFromCursor(stream.RequestID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, event := range events {
		task := event.Message.GetInteractionUpdate().GetToolCallDelta().GetToolCallDelta().GetTaskToolCallDelta()
		text.WriteString(task.GetInteractionUpdate().GetThinkingDelta().GetText())
	}
	return text.String()
}

func backgroundHandoff(reason agentv1.SubagentBackgroundReason) *agentv1.SubagentResult {
	return &agentv1.SubagentResult{Result: &agentv1.SubagentResult_Success{Success: &agentv1.SubagentSuccess{
		AgentId:          backgroundTestChildConversation,
		BackgroundReason: reason,
		TranscriptPath:   proto.String(`c:\Users\dev\.cursor\projects\repo\agent-transcripts\parent\subagents\` + backgroundTestChildConversation + `.jsonl`),
	}}}
}

// The client answers a background subagent's exec about a second after
// launching it, with a background reason and no final message. In a logged
// session that answer was recorded as the subagent's end: the checkpoint
// carried SUBAGENT_RUN_STATUS_SUCCESS with a completion time, and the client,
// which reads that status before anything else it knows about the card,
// showed "Completed" on a subagent that went on working for minutes.
func TestBackgroundSubagentHandoffIsNotReportedAsFinished(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason agentv1.SubagentBackgroundReason
	}{
		{"launched in the background by the agent", agentv1.SubagentBackgroundReason_SUBAGENT_BACKGROUND_REASON_AGENT_REQUEST},
		{"sent to the background by the user", agentv1.SubagentBackgroundReason_SUBAGENT_BACKGROUND_REASON_USER_REQUEST},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const toolCallID = "toolu_01TaskRunInTheBackground"
			service, stream := backgroundSubagentFixture(t)
			sent := launchNativeTask(t, service, stream, toolCallID, true)

			if run := publishedRunState(t, service, stream, toolCallID); run.GetStatus() != agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_RUNNING {
				t.Fatalf("run while the exec is pending = %v, want RUNNING", run.GetStatus())
			}

			answerNativeTask(t, service, stream, sent, backgroundHandoff(tc.reason))

			run := publishedRunState(t, service, stream, toolCallID)
			if run == nil {
				t.Fatal("the checkpoint dropped the run of a subagent that is still working")
			}
			if run.GetStatus() != agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_BACKGROUNDED {
				t.Fatalf("run after the background handoff = %v, want BACKGROUNDED", run.GetStatus())
			}
			if run.CompletedTimestampMs != nil || run.CompletionReason != nil {
				t.Fatalf("run carries an end (completed_at=%v reason=%v) while the subagent is running", run.CompletedTimestampMs, run.CompletionReason)
			}
			if run.GetSubagentId() != delegationSubagentID(toolCallID) || run.GetParentToolCallId() != toolCallID {
				t.Fatalf("run identity = %q for %q, want the id the Task card was opened with", run.GetSubagentId(), run.GetParentToolCallId())
			}

			card := completedTaskCard(t, service, stream, toolCallID)
			success := card.GetResult().GetSuccess()
			if success == nil || !success.GetIsBackground() || success.GetAgentId() != backgroundTestChildConversation || success.GetBackgroundReason() != tc.reason {
				t.Fatalf("Task card result = %v, want a background success naming the child", card.GetResult())
			}
			if written := taskCardDeltas(t, service, stream); strings.Contains(written, "已完成") || strings.Contains(written, "已结束") {
				t.Fatalf("the Task card was told the subagent finished: %q", written)
			}

			// The exec is answered, so Cursor's stream gets the client's own
			// result and nothing here waits on the subagent any more.
			select {
			case answer := <-stream.CursorToolResults:
				if answer.GetSubagentResult().GetSuccess().GetBackgroundReason() != tc.reason {
					t.Fatalf("answer sent to Cursor = %v, want the client's background handoff", answer)
				}
			default:
				t.Fatal("the handoff was not answered on the open Cursor stream")
			}
			stream.mu.Lock()
			pending := len(stream.PendingExecs)
			stream.mu.Unlock()
			if pending != 0 {
				t.Fatalf("%d execs still pending after the handoff", pending)
			}
			if service.hasActiveDelegation(stream) {
				t.Fatal("a backgrounded subagent still counts as work the parent turn is waiting for")
			}
			item, ok := service.nativeDelegationTask(sentExecID(t, stream, toolCallID, sent))
			if !ok || !delegatedStatusTerminal(item.Status) || item.Error != "" {
				t.Fatalf("delegation bookkeeping = %+v (found %t), want it closed without an error so its slot is free", item, ok)
			}
		})
	}
}

// sentExecID is the id the client's answer carries, which is also the key of
// the delegation's bookkeeping.
func sentExecID(t *testing.T, stream *ActiveStream, toolCallID string, sent *agentv1.ExecServerMessage) string {
	t.Helper()
	if id := strings.TrimSpace(sent.GetExecId()); id != "" {
		return id
	}
	t.Fatalf("exec for %s was sent without an exec id", toolCallID)
	return ""
}

// A subagent that ran in the foreground is answered when it ends. That answer
// is its end and is still reported as one.
func TestForegroundSubagentResultIsStillReportedAsFinished(t *testing.T) {
	const toolCallID = "toolu_foreground_task"
	service, stream := backgroundSubagentFixture(t)
	sent := launchNativeTask(t, service, stream, toolCallID, false)

	answerNativeTask(t, service, stream, sent, &agentv1.SubagentResult{Result: &agentv1.SubagentResult_Success{Success: &agentv1.SubagentSuccess{
		AgentId:       backgroundTestChildConversation,
		FinalMessage:  proto.String("The builder runs; three frames differ."),
		ToolCallCount: 12,
	}}})

	run := publishedRunState(t, service, stream, toolCallID)
	if run.GetStatus() != agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_SUCCESS || run.CompletedTimestampMs == nil {
		t.Fatalf("run after the subagent ended = %v (completed_at=%v), want SUCCESS with a completion time", run.GetStatus(), run.CompletedTimestampMs)
	}
	if success := completedTaskCard(t, service, stream, toolCallID).GetResult().GetSuccess(); success == nil || success.GetIsBackground() || len(success.GetConversationSteps()) != 1 {
		t.Fatalf("Task card result = %v, want the final message of a finished subagent", success)
	}
	item, ok := service.nativeDelegationTask(sentExecID(t, stream, toolCallID, sent))
	if !ok || item.Status != delegation.TaskCompleted {
		t.Fatalf("delegation bookkeeping = %+v (found %t), want completed", item, ok)
	}
}

// A failure is an end as well, also when the subagent had been asked to run
// in the background.
func TestFailedSubagentLaunchIsReportedAsFailed(t *testing.T) {
	const toolCallID = "toolu_failed_task"
	service, stream := backgroundSubagentFixture(t)
	sent := launchNativeTask(t, service, stream, toolCallID, true)

	answerNativeTask(t, service, stream, sent, &agentv1.SubagentResult{Result: &agentv1.SubagentResult_Error{Error: &agentv1.SubagentError{
		Error: "Subagent could not be started",
	}}})

	if run := publishedRunState(t, service, stream, toolCallID); run.GetStatus() != agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_ERROR {
		t.Fatalf("run after a failed launch = %v, want ERROR", run.GetStatus())
	}
}

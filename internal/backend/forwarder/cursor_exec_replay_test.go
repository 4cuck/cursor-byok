package forwarder

import (
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func replayTestShellInvocation(t *testing.T, id uint32, execID string, args *agentv1.ShellArgs, dispatchCallID string) (*agentv1.ExecServerMessage, runtimecore.ToolInvocation) {
	t.Helper()
	msg := &agentv1.ExecServerMessage{Id: id, ExecId: execID, Message: &agentv1.ExecServerMessage_ShellStreamArgs{ShellStreamArgs: args}}
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	argsJSON, err := protojson.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return msg, runtimecore.ToolInvocation{ToolName: "Shell", CallID: dispatchCallID, ModelCallID: "model-replay", ArgsJSON: argsJSON, NativeExec: raw}
}

func replayTestApply(t *testing.T, service *Service, stream *ActiveStream, invocation runtimecore.ToolInvocation) {
	t.Helper()
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:           modeladapter.ModelEventKindToolLikeCompleted,
		ToolCallID:     invocation.CallID,
		ToolInvocation: &invocation,
	}); err != nil {
		t.Fatal(err)
	}
}

// replayTestLoseStream leaves the stream as the end of a pass does: Cursor's
// stream is gone, and with it the ids its execs were to be answered under.
func replayTestLoseStream(stream *ActiveStream) {
	stream.mu.Lock()
	stream.CursorToolResults = nil
	stream.nativeExecSlots = nil
	stream.mu.Unlock()
}

func replayTestResume(stream *ActiveStream) chan *agentv1.ExecClientMessage {
	answers := make(chan *agentv1.ExecClientMessage, 4)
	stream.mu.Lock()
	stream.CursorToolResults = answers
	stream.mu.Unlock()
	return answers
}

func replayTestToolEntries(stream *ActiveStream, toolCallID string) (calls, results int) {
	for _, entry := range snapshotHistoryEntries(stream) {
		if historyEntryToolCallID(entry) != toolCallID {
			continue
		}
		switch entry.Kind {
		case "tool_call":
			calls++
		case "tool_result":
			results++
		}
	}
	return calls, results
}

// The stream to Cursor is lost while a Read is running in the client. The
// client finishes it; the turn is resumed from Cursor's checkpoint, in which
// the Read is still pending, so Cursor requests it again under the same tool
// call id. It gets the result the turn already has. The file is not read a
// second time and the call does not appear twice.
func TestCursorExecRequestedAgainAfterLostStreamIsAnsweredFromRecord(t *testing.T) {
	const toolCallID = "toolu_01ReadDuringTheDrop"
	const path = "/repo/notes/plan.md"
	const content = "step one\nstep two\n"
	service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
	stream.CurrentModelCallID = "model-replay"
	args := &agentv1.ReadArgs{Path: path, ToolCallId: toolCallID}

	_, first := nativeReadInvocation(t, 7, "exec-before-the-drop", args, toolCallID, false)
	replayTestApply(t, service, stream, first)
	sent := writeFixtureExecs(stream)
	if len(sent) != 1 || sent[0].GetReadArgs().GetPath() != path {
		t.Fatalf("the Read was not sent to the client: %v", sent)
	}

	replayTestLoseStream(stream)
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: sent[0].Id, ExecId: sent[0].ExecId,
			Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
				Path: path, TotalLines: 2, FileSize: int64(len(content)), Output: &agentv1.ReadSuccess_Content{Content: content},
			}}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if calls, results := replayTestToolEntries(stream, toolCallID); calls != 1 || results != 1 {
		t.Fatalf("history after the client finished the Read: %d calls, %d results, want 1 and 1", calls, results)
	}

	answers := replayTestResume(stream)
	stream.mu.Lock()
	countedBefore := stream.ToolInvocationCount
	stream.mu.Unlock()
	again, second := nativeReadInvocation(t, 3, "exec-after-the-resume", args, toolCallID, false)
	replayTestApply(t, service, stream, second)

	if got := writeFixtureExecs(stream); len(got) != 1 {
		t.Fatalf("the Read was sent to the client %d times, want once: the turn already has its result", len(got))
	}
	select {
	case answer := <-answers:
		if answer.GetId() != again.Id || answer.GetExecId() != again.ExecId {
			t.Fatalf("answered as id=%d exec=%q, want the ids of the new request id=%d exec=%q", answer.GetId(), answer.GetExecId(), again.Id, again.ExecId)
		}
		if got := answer.GetReadResult().GetSuccess().GetContent(); got != content {
			t.Fatalf("answered with content %q, want the recorded %q", got, content)
		}
	default:
		t.Fatal("Cursor's second request for the Read was not answered")
	}
	if calls, results := replayTestToolEntries(stream, toolCallID); calls != 1 || results != 1 {
		t.Fatalf("history after the replay: %d calls, %d results, want still 1 and 1", calls, results)
	}
	stream.mu.Lock()
	counted, pending := stream.ToolInvocationCount, len(stream.PendingExecs)
	stream.mu.Unlock()
	if counted != countedBefore || pending != 0 {
		t.Fatalf("the replay was handled as a new tool call: counted %d -> %d, pending execs %d", countedBefore, counted, pending)
	}
}

// A finished shell is answered with two messages: the text the model reads,
// then the exit. Both are kept and both are replayed, in that order.
func TestCursorShellRequestedAgainIsAnsweredWithOutputAndExit(t *testing.T) {
	const toolCallID = "toolu_01ShellDuringTheDrop"
	const output = "ok  	cursor/internal/backend/forwarder	16.5s\n"
	service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
	stream.CurrentModelCallID = "model-replay"
	args := &agentv1.ShellArgs{Command: "go test ./...", ToolCallId: toolCallID}

	// The shell was dispatched, the stream was lost, the shell then ended.
	noteCursorExecCall(stream, toolCallID, "Shell")
	replayTestLoseStream(stream)
	pending := runtimecore.PendingExec{ToolCallID: toolCallID, ExecKind: "shell", StdoutBuffer: output}
	exit := &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{
		Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: 0}},
	}}}
	if service.answerCursorAccountShellTerminal(stream, pending, exit) {
		t.Fatal("the shell's end was reported as delivered with no stream to Cursor")
	}

	answers := replayTestResume(stream)
	again, invocation := replayTestShellInvocation(t, 5, "exec-after-the-resume", args, toolCallID)
	if err := service.handleToolInvocation(stream, invocation); err != nil {
		t.Fatal(err)
	}
	if got := writeFixtureExecs(stream); len(got) != 0 {
		t.Fatalf("the command was sent to the client again: %v", got)
	}
	if len(answers) != 2 {
		t.Fatalf("%d messages replayed, want the output and the exit", len(answers))
	}
	text, end := <-answers, <-answers
	if got := text.GetShellStream().GetStdout().GetData(); got != output {
		t.Fatalf("first replayed message carries %q, want the shell's output %q", got, output)
	}
	if end.GetShellStream().GetExit() == nil {
		t.Fatalf("second replayed message is %v, want the shell's exit", end)
	}
	for _, message := range []*agentv1.ExecClientMessage{text, end} {
		if message.GetId() != again.Id || message.GetExecId() != again.ExecId {
			t.Fatalf("replayed as id=%d exec=%q, want id=%d exec=%q", message.GetId(), message.GetExecId(), again.Id, again.ExecId)
		}
	}
}

// Only the same tool call, requested again as the same tool, is answered from
// the record. Everything else runs as Cursor asks.
func TestCursorExecReplayLeavesOtherExecsAlone(t *testing.T) {
	const toolCallID = "toolu_01Recorded"
	const path = "/repo/a.txt"
	recorded := func(t *testing.T) (*Service, *ActiveStream) {
		service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
		stream.CurrentModelCallID = "model-replay"
		noteCursorExecCall(stream, toolCallID, "Read")
		service.rememberCursorExecAnswer(stream, runtimecore.PendingExec{ToolCallID: toolCallID, ExecKind: "read"}, &agentv1.ExecClientMessage{
			Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
				Path: path, Output: &agentv1.ReadSuccess_Content{Content: "recorded"},
			}}}},
		})
		return service, stream
	}
	for _, tc := range []struct {
		name       string
		invocation func(t *testing.T) runtimecore.ToolInvocation
		prepare    func(stream *ActiveStream)
	}{
		{
			// The reads of a wait or an edit come many times under one id.
			name: "an internal exec of the same tool call",
			invocation: func(t *testing.T) runtimecore.ToolInvocation {
				_, invocation := nativeReadInvocation(t, 11, "exec-internal", &agentv1.ReadArgs{Path: path, ToolCallId: toolCallID}, "exec-internal", true)
				return invocation
			},
		},
		{
			// A second exec of one tool call gets a suffixed id that differs
			// from pass to pass: it cannot be matched to a record.
			name: "an exec under an id made unique within the pass",
			invocation: func(t *testing.T) runtimecore.ToolInvocation {
				_, invocation := nativeReadInvocation(t, 12, "exec-second", &agentv1.ReadArgs{Path: path, ToolCallId: toolCallID}, toolCallID+"-3", false)
				return invocation
			},
		},
		{
			name: "another tool under the same id",
			invocation: func(t *testing.T) runtimecore.ToolInvocation {
				_, invocation := replayTestShellInvocation(t, 13, "exec-shell", &agentv1.ShellArgs{Command: "dir", ToolCallId: toolCallID}, toolCallID)
				return invocation
			},
		},
		{
			name: "the same tool call in a later turn",
			invocation: func(t *testing.T) runtimecore.ToolInvocation {
				_, invocation := nativeReadInvocation(t, 14, "exec-next-turn", &agentv1.ReadArgs{Path: path, ToolCallId: toolCallID}, toolCallID, false)
				return invocation
			},
			prepare: func(stream *ActiveStream) {
				stream.mu.Lock()
				forgetCursorExecAnswersLocked(stream)
				stream.mu.Unlock()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, stream := recorded(t)
			if tc.prepare != nil {
				tc.prepare(stream)
			}
			invocation := tc.invocation(t)
			invocation.ModelCallID = stream.CurrentModelCallID
			if err := service.handleToolInvocation(stream, invocation); err != nil {
				t.Fatal(err)
			}
			if got := writeFixtureExecs(stream); len(got) != 1 {
				t.Fatalf("%d execs sent to the client, want this one to run", len(got))
			}
			if len(stream.CursorToolResults) != 0 {
				t.Fatalf("answered from the record of another exec: %v", <-stream.CursorToolResults)
			}
		})
	}
}

// The memory holds the tool calls of the step that was running, not the turn.
func TestCursorExecAnswerMemoryIsBounded(t *testing.T) {
	service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
	answer := &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
		Output: &agentv1.ReadSuccess_Content{Content: "x"},
	}}}}}
	ids := make([]string, 0, cursorExecAnswerLimit+10)
	for i := 0; i < cursorExecAnswerLimit+10; i++ {
		id := "toolu_" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		ids = append(ids, id)
		noteCursorExecCall(stream, id, "Read")
		service.rememberCursorExecAnswer(stream, runtimecore.PendingExec{ToolCallID: id, ExecKind: "read"}, answer)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if len(stream.cursorExecAnswers) != cursorExecAnswerLimit || len(stream.cursorExecAnswerOrder) != cursorExecAnswerLimit {
		t.Fatalf("memory holds %d answers (order %d), want the newest %d", len(stream.cursorExecAnswers), len(stream.cursorExecAnswerOrder), cursorExecAnswerLimit)
	}
	if _, kept := stream.cursorExecAnswers[ids[0]]; kept {
		t.Fatal("the oldest answer was kept past the limit")
	}
	if _, kept := stream.cursorExecAnswers[ids[len(ids)-1]]; !kept {
		t.Fatal("the newest answer was dropped")
	}
}

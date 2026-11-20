package forwarder

import (
	"fmt"
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func makeNumberedLines(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("Line %d", i+1)
	}
	return strings.Join(lines, "\n") + "\n"
}

func nativeReadInvocation(t *testing.T, id uint32, execID string, args *agentv1.ReadArgs, dispatchCallID string, silentUI bool) (*agentv1.ExecServerMessage, runtimecore.ToolInvocation) {
	t.Helper()
	msg := &agentv1.ExecServerMessage{
		Id:      id,
		ExecId:  execID,
		Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: args},
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	argsJSON, err := protojson.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return msg, runtimecore.ToolInvocation{
		ToolName:    "Read",
		CallID:      dispatchCallID,
		ModelCallID: "model-edit",
		ArgsJSON:    argsJSON,
		NativeExec:  raw,
		SilentUI:    silentUI,
	}
}

func nativeWriteInvocationWithID(t *testing.T, id uint32, execID string, args *agentv1.WriteArgs, dispatchCallID string, silentUI bool) (*agentv1.ExecServerMessage, runtimecore.ToolInvocation) {
	t.Helper()
	msg := &agentv1.ExecServerMessage{
		Id:      id,
		ExecId:  execID,
		Message: &agentv1.ExecServerMessage_WriteArgs{WriteArgs: args},
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	argsJSON, err := protojson.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return msg, runtimecore.ToolInvocation{
		ToolName:    "Write",
		CallID:      dispatchCallID,
		ModelCallID: "model-edit",
		ArgsJSON:    argsJSON,
		NativeExec:  raw,
		SilentUI:    silentUI,
	}
}

func TestCursorOwnedEditCreateThenRemove30LinesShowsMinus30WithoutSpuriousReadCards(t *testing.T) {
	const path = `c:\Users\admin\Documents\cursor-byok\test_50_lines.txt`
	content50 := makeNumberedLines(50)
	content20 := makeNumberedLines(20)
	diffMinus30, added, removed := computeEditDiff(content50, content20)
	if added != 0 || removed != 30 {
		t.Fatalf("computeEditDiff(50->20) = +%d/-%d, want +0/-30", added, removed)
	}

	service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
	stream.CurrentModelCallID = "model-edit"

	// Thinking before first edit
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind: modeladapter.ModelEventKindThinkingDelta,
		Text: "Creating 50-line file",
	}); err != nil {
		t.Fatal(err)
	}

	// Step 1: Write (create 50-line file) owned by Cursor upstream EditToolCall
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:       modeladapter.ModelEventKindCursorEditStarted,
		ToolCallID: "edit-1",
		CursorEditStarted: &agentv1.ToolCallStartedUpdate{
			CallId: "edit-1",
			ToolCall: &agentv1.ToolCall{
				ToolCallId: proto.String("edit-1"),
				Tool: &agentv1.ToolCall_EditToolCall{
					EditToolCall: &agentv1.EditToolCall{
						Args: &agentv1.EditArgs{Path: path, StreamContent: proto.String(content50)},
					},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Silent pre-read returns FileNotFound
	origRead1, readInv1 := nativeReadInvocation(t, 10, "edit-1-read", &agentv1.ReadArgs{Path: path, ToolCallId: "edit-1"}, "edit-1-read", true)
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:           modeladapter.ModelEventKindToolLikeCompleted,
		ToolCallID:     readInv1.CallID,
		ToolInvocation: &readInv1,
	}); err != nil {
		t.Fatal(err)
	}
	execs := writeFixtureExecs(stream)
	if len(execs) != 1 || execs[0].GetReadArgs().GetPath() != path {
		t.Fatalf("expected silent ReadArgs exec, got %+v", execs)
	}
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: execs[0].Id, ExecId: execs[0].ExecId,
			Message: &agentv1.ExecClientMessage_ReadResult{
				ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_FileNotFound{FileNotFound: &agentv1.ReadFileNotFound{Path: path}}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	gotRead1 := <-stream.CursorToolResults
	if gotRead1.GetId() != origRead1.Id || gotRead1.GetExecId() != origRead1.ExecId || gotRead1.GetReadResult().GetFileNotFound() == nil {
		t.Fatalf("unexpected upstream read1 reply: %v", gotRead1)
	}

	// Silent WriteArgs writes 50 lines
	origWrite1, writeInv1 := nativeWriteInvocationWithID(t, 11, "edit-1-write", &agentv1.WriteArgs{Path: path, FileText: content50, ToolCallId: "edit-1"}, "edit-1-write", true)
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:           modeladapter.ModelEventKindToolLikeCompleted,
		ToolCallID:     writeInv1.CallID,
		ToolInvocation: &writeInv1,
	}); err != nil {
		t.Fatal(err)
	}
	execs = writeFixtureExecs(stream)
	if len(execs) != 2 || execs[1].GetWriteArgs().GetPath() != path {
		t.Fatalf("expected silent WriteArgs exec, got %+v", execs)
	}
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: execs[1].Id, ExecId: execs[1].ExecId,
			Message: &agentv1.ExecClientMessage_WriteResult{
				WriteResult: &agentv1.WriteResult{Result: &agentv1.WriteResult_Success{Success: &agentv1.WriteSuccess{Path: path, LinesCreated: 50, FileSize: int32(len(content50))}}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	gotWrite1 := <-stream.CursorToolResults
	if gotWrite1.GetId() != origWrite1.Id || gotWrite1.GetExecId() != origWrite1.ExecId || gotWrite1.GetWriteResult().GetSuccess().GetLinesCreated() != 50 {
		t.Fatalf("unexpected upstream write1 reply: %v", gotWrite1)
	}

	// Upstream authoritative completion for create (+50, BeforeFullFileContent == nil)
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:       modeladapter.ModelEventKindCursorEditCompleted,
		ToolCallID: "edit-1",
		CursorEditCompleted: &agentv1.ToolCallCompletedUpdate{
			CallId: "edit-1",
			ToolCall: &agentv1.ToolCall{
				ToolCallId: proto.String("edit-1"),
				Tool: &agentv1.ToolCall_EditToolCall{
					EditToolCall: &agentv1.EditToolCall{
						Args: &agentv1.EditArgs{Path: path, StreamContent: proto.String(content50)},
						Result: &agentv1.EditResult{
							Result: &agentv1.EditResult_Success{
								Success: &agentv1.EditSuccess{
									Path:                 path,
									LinesAdded:           proto.Int32(50),
									LinesRemoved:         proto.Int32(0),
									AfterFullFileContent: content50,
								},
							},
						},
					},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Step 2: StrReplace (remove 30 lines) owned by Cursor upstream EditToolCall
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:       modeladapter.ModelEventKindCursorEditStarted,
		ToolCallID: "edit-2",
		CursorEditStarted: &agentv1.ToolCallStartedUpdate{
			CallId: "edit-2",
			ToolCall: &agentv1.ToolCall{
				ToolCallId: proto.String("edit-2"),
				Tool: &agentv1.ToolCall_EditToolCall{
					EditToolCall: &agentv1.EditToolCall{
						Args: &agentv1.EditArgs{Path: path, StreamContent: proto.String("Line 20")},
					},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Silent pre-read returns 50 lines
	_, readInv2 := nativeReadInvocation(t, 12, "edit-2-read", &agentv1.ReadArgs{Path: path, ToolCallId: "edit-2"}, "edit-2-read", true)
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:           modeladapter.ModelEventKindToolLikeCompleted,
		ToolCallID:     readInv2.CallID,
		ToolInvocation: &readInv2,
	}); err != nil {
		t.Fatal(err)
	}
	execs = writeFixtureExecs(stream)
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: execs[2].Id, ExecId: execs[2].ExecId,
			Message: &agentv1.ExecClientMessage_ReadResult{
				ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
					Path: path, TotalLines: 50, Output: &agentv1.ReadSuccess_Content{Content: content50},
				}}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	<-stream.CursorToolResults

	// Silent WriteArgs writes 20 lines
	_, writeInv2 := nativeWriteInvocationWithID(t, 13, "edit-2-write", &agentv1.WriteArgs{Path: path, FileText: content20, ToolCallId: "edit-2"}, "edit-2-write", true)
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:           modeladapter.ModelEventKindToolLikeCompleted,
		ToolCallID:     writeInv2.CallID,
		ToolInvocation: &writeInv2,
	}); err != nil {
		t.Fatal(err)
	}
	execs = writeFixtureExecs(stream)
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: execs[3].Id, ExecId: execs[3].ExecId,
			Message: &agentv1.ExecClientMessage_WriteResult{
				WriteResult: &agentv1.WriteResult{Result: &agentv1.WriteResult_Success{Success: &agentv1.WriteSuccess{Path: path, LinesCreated: 20, FileSize: int32(len(content20))}}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	<-stream.CursorToolResults

	// Upstream authoritative completion for StrReplace (-30, BeforeFullFileContent == content50)
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:       modeladapter.ModelEventKindCursorEditCompleted,
		ToolCallID: "edit-2",
		CursorEditCompleted: &agentv1.ToolCallCompletedUpdate{
			CallId: "edit-2",
			ToolCall: &agentv1.ToolCall{
				ToolCallId: proto.String("edit-2"),
				Tool: &agentv1.ToolCall_EditToolCall{
					EditToolCall: &agentv1.EditToolCall{
						Args: &agentv1.EditArgs{Path: path, StreamContent: proto.String("Line 20")},
						Result: &agentv1.EditResult{
							Result: &agentv1.EditResult_Success{
								Success: &agentv1.EditSuccess{
									Path:                  path,
									LinesAdded:            proto.Int32(0),
									LinesRemoved:          proto.Int32(30),
									DiffString:            proto.String(diffMinus30),
									BeforeFullFileContent: proto.String(content50),
									AfterFullFileContent:  content20,
								},
							},
						},
					},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Verify UI cards emitted to Cursor IDE
	var editStarts, editCompletes []*agentv1.EditToolCall
	var readCards int
	for _, event := range stream.Backlog {
		u := event.Message.GetInteractionUpdate()
		if s := u.GetToolCallStarted(); s != nil {
			if s.GetToolCall().GetReadToolCall() != nil {
				readCards++
			}
			if e := s.GetToolCall().GetEditToolCall(); e != nil {
				editStarts = append(editStarts, e)
			}
		}
		if c := u.GetToolCallCompleted(); c != nil {
			if c.GetToolCall().GetReadToolCall() != nil {
				readCards++
			}
			if e := c.GetToolCall().GetEditToolCall(); e != nil {
				editCompletes = append(editCompletes, e)
			}
		}
	}
	if readCards != 0 {
		t.Fatalf("got %d spurious Read UI cards for internal edit pre-reads, want 0", readCards)
	}
	if len(editStarts) != 2 || len(editCompletes) != 2 {
		t.Fatalf("got %d edit starts and %d edit completes, want 2 and 2", len(editStarts), len(editCompletes))
	}
	first := editCompletes[0].GetResult().GetSuccess()
	if first.GetLinesAdded() != 50 || first.GetLinesRemoved() != 0 || first.BeforeFullFileContent != nil {
		t.Fatalf("first edit = +%d/-%d before=%v, want +50/-0 before=nil", first.GetLinesAdded(), first.GetLinesRemoved(), first.BeforeFullFileContent)
	}
	second := editCompletes[1].GetResult().GetSuccess()
	if second.GetLinesAdded() != 0 || second.GetLinesRemoved() != 30 || second.GetBeforeFullFileContent() != content50 || second.GetAfterFullFileContent() != content20 {
		t.Fatalf("second edit = +%d/-%d beforeLen=%d afterLen=%d, want +0/-30", second.GetLinesAdded(), second.GetLinesRemoved(), len(second.GetBeforeFullFileContent()), len(second.GetAfterFullFileContent()))
	}
	if !strings.Contains(second.GetDiffString(), "-Line 21") || !strings.Contains(second.GetDiffString(), "-Line 50") {
		t.Fatalf("second edit diff missing removed lines: %q", second.GetDiffString())
	}
	if n := countToolResultEntries(t, snapshotHistoryEntries(stream), "edit-1"); n != 1 {
		t.Fatalf("history edit-1 results = %d, want 1", n)
	}
	if n := countToolResultEntries(t, snapshotHistoryEntries(stream), "edit-2"); n != 1 {
		t.Fatalf("history edit-2 results = %d, want 1", n)
	}
	if len(stream.activeCursorEdits) != 0 {
		t.Fatalf("activeCursorEdits not drained: %+v", stream.activeCursorEdits)
	}
}

func TestStandaloneNativeWriteUsesObservedBeforeContentForDiffFallback(t *testing.T) {
	const path = `c:\repo\test_50_lines.txt`
	content50 := makeNumberedLines(50)
	content20 := makeNumberedLines(20)

	service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)

	// 1. Standalone native Read returns 50 lines
	_, readInv := nativeReadInvocation(t, 1, "r1", &agentv1.ReadArgs{Path: path, ToolCallId: "call-read"}, "call-read", false)
	if err := service.handleToolInvocation(stream, readInv); err != nil {
		t.Fatal(err)
	}
	execs := writeFixtureExecs(stream)
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: execs[0].Id, ExecId: execs[0].ExecId,
			Message: &agentv1.ExecClientMessage_ReadResult{
				ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
					Path: path, TotalLines: 50, Output: &agentv1.ReadSuccess_Content{Content: content50},
				}}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	<-stream.CursorToolResults

	// 2. Standalone native Write (no upstream EditToolCall InteractionUpdate) writes 20 lines
	_, writeInv := nativeWriteInvocationWithID(t, 2, "w2", &agentv1.WriteArgs{Path: path, FileText: content20, ToolCallId: "call-write"}, "call-write", false)
	if err := service.handleToolInvocation(stream, writeInv); err != nil {
		t.Fatal(err)
	}
	execs = writeFixtureExecs(stream)
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: execs[1].Id, ExecId: execs[1].ExecId,
			Message: &agentv1.ExecClientMessage_WriteResult{
				WriteResult: &agentv1.WriteResult{Result: &agentv1.WriteResult_Success{Success: &agentv1.WriteSuccess{
					Path: path, LinesCreated: 20, FileSize: int32(len(content20)),
				}}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	<-stream.CursorToolResults

	var completedEdit *agentv1.EditSuccess
	for _, event := range stream.Backlog {
		if c := event.Message.GetInteractionUpdate().GetToolCallCompleted(); c != nil && c.GetCallId() == "call-write" {
			completedEdit = c.GetToolCall().GetEditToolCall().GetResult().GetSuccess()
		}
	}
	if completedEdit == nil {
		t.Fatal("missing completed EditSuccess for standalone native write")
	}
	if completedEdit.GetLinesAdded() != 0 || completedEdit.GetLinesRemoved() != 30 {
		t.Fatalf("standalone native write diff = +%d/-%d, want +0/-30", completedEdit.GetLinesAdded(), completedEdit.GetLinesRemoved())
	}
	if completedEdit.GetBeforeFullFileContent() != content50 || completedEdit.GetAfterFullFileContent() != content20 {
		t.Fatalf("unexpected before/after content lengths: before=%d after=%d", len(completedEdit.GetBeforeFullFileContent()), len(completedEdit.GetAfterFullFileContent()))
	}
	if !strings.Contains(completedEdit.GetDiffString(), "-Line 21") || !strings.Contains(completedEdit.GetDiffString(), "-Line 50") {
		t.Fatalf("diffString missing removed lines: %q", completedEdit.GetDiffString())
	}
}

func TestPartialSliceReadDoesNotCorruptStandaloneNativeWriteDiff(t *testing.T) {
	const path = `/remote/workspace/slice.txt`
	content20 := makeNumberedLines(20)
	service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)

	// Partial slice Read (offset=1, limit=5) must NOT be cached as full beforeContent.
	_, readInv := nativeReadInvocation(t, 1, "r1", &agentv1.ReadArgs{Path: path, Offset: proto.Int32(1), Limit: proto.Uint32(5), ToolCallId: "call-read"}, "call-read", false)
	if err := service.handleToolInvocation(stream, readInv); err != nil {
		t.Fatal(err)
	}
	execs := writeFixtureExecs(stream)
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: execs[0].Id, ExecId: execs[0].ExecId,
			Message: &agentv1.ExecClientMessage_ReadResult{
				ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
					Path: path, TotalLines: 50, Output: &agentv1.ReadSuccess_Content{Content: makeNumberedLines(5)},
				}}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	<-stream.CursorToolResults

	_, writeInv := nativeWriteInvocationWithID(t, 2, "w2", &agentv1.WriteArgs{Path: path, FileText: content20, ToolCallId: "call-write"}, "call-write", false)
	if err := service.handleToolInvocation(stream, writeInv); err != nil {
		t.Fatal(err)
	}
	execs = writeFixtureExecs(stream)
	if err := service.handleExecResult(InboundIntent{
		RequestID: stream.RequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id: execs[1].Id, ExecId: execs[1].ExecId,
			Message: &agentv1.ExecClientMessage_WriteResult{
				WriteResult: &agentv1.WriteResult{Result: &agentv1.WriteResult_Success{Success: &agentv1.WriteSuccess{
					Path: path, LinesCreated: 20, FileSize: int32(len(content20)),
				}}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	<-stream.CursorToolResults

	for _, event := range stream.Backlog {
		if c := event.Message.GetInteractionUpdate().GetToolCallCompleted(); c != nil && c.GetCallId() == "call-write" {
			s := c.GetToolCall().GetEditToolCall().GetResult().GetSuccess()
			if s.BeforeFullFileContent != nil || s.GetLinesAdded() != 20 {
				t.Fatalf("partial read polluted write diff: before=%v added=%d", s.BeforeFullFileContent, s.GetLinesAdded())
			}
			return
		}
	}
	t.Fatal("missing completed write card")
}

func TestSilentNativeExecErrorAndUnreportedCursorEditCleanup(t *testing.T) {
	const path = `/remote/workspace/file.txt`
	t.Run("plan mode rejects silent edit write with typed WriteError", func(t *testing.T) {
		service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_PLAN)
		orig, inv := nativeWriteInvocationWithID(t, 21, "w-plan", &agentv1.WriteArgs{Path: path, FileText: "hi", ToolCallId: "edit-plan"}, "w-plan", true)
		if err := service.handleToolInvocation(stream, inv); err != nil {
			t.Fatal(err)
		}
		if len(writeFixtureExecs(stream)) != 0 {
			t.Fatal("plan-denied silent write reached client executor")
		}
		got := <-stream.CursorToolResults
		if got.GetId() != orig.Id || got.GetExecId() != orig.ExecId || got.GetWriteResult().GetError() == nil {
			t.Fatalf("expected typed WriteError reply with original wire IDs, got %v", got)
		}
	})

	t.Run("exec control throw on silent Read returns typed ReadError", func(t *testing.T) {
		service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
		orig, inv := nativeReadInvocation(t, 22, "r-throw", &agentv1.ReadArgs{Path: path, ToolCallId: "edit-1"}, "r-throw", true)
		if err := service.handleToolInvocation(stream, inv); err != nil {
			t.Fatal(err)
		}
		execs := writeFixtureExecs(stream)
		if err := service.handleExecControl(InboundIntent{
			RequestID: stream.RequestID,
			ExecClientControlMessage: &agentv1.ExecClientControlMessage{
				Message: &agentv1.ExecClientControlMessage_Throw{Throw: &agentv1.ExecClientThrow{Id: execs[0].Id, Error: "disk I/O failure"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		got := <-stream.CursorToolResults
		if got.GetId() != orig.Id || got.GetExecId() != orig.ExecId || got.GetReadResult().GetError() == nil {
			t.Fatalf("expected typed ReadError reply, got %v", got)
		}
	})

	t.Run("unreported CursorEditStarted closes cleanly on stream end and cancel", func(t *testing.T) {
		for _, canceled := range []bool{false, true} {
			service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
			if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
				Kind:       modeladapter.ModelEventKindCursorEditStarted,
				ToolCallID: "edit-dangling",
				CursorEditStarted: &agentv1.ToolCallStartedUpdate{
					CallId: "edit-dangling",
					ToolCall: &agentv1.ToolCall{
						ToolCallId: proto.String("edit-dangling"),
						Tool: &agentv1.ToolCall_EditToolCall{
							EditToolCall: &agentv1.EditToolCall{Args: &agentv1.EditArgs{Path: path}},
						},
					},
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := service.closeUnreportedCursorEdits(stream, "stopped", canceled); err != nil {
				t.Fatal(err)
			}
			var found *agentv1.EditResult
			for _, event := range stream.Backlog {
				if c := event.Message.GetInteractionUpdate().GetToolCallCompleted(); c != nil && c.GetCallId() == "edit-dangling" {
					found = c.GetToolCall().GetEditToolCall().GetResult()
				}
			}
			if found == nil {
				t.Fatal("dangling edit card was not closed")
			}
			if canceled && found.GetRejected() == nil {
				t.Fatalf("canceled edit expected Rejected result, got %v", found)
			}
			if !canceled && found.GetError() == nil {
				t.Fatalf("ended-stream edit expected Error result, got %v", found)
			}
		}
	})
}

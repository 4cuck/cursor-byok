package forwarder

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"cursor/gen/agentv1"
	execbridge "cursor/internal/backend/agent/bridge/exec"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The path and page are fixtures owned by a remote IDE, never by the test host.
func TestNativeFetchedFileWriteAcknowledgment(t *testing.T) {
	const path = "/remote/workspace/tool-outputs/c300efea-0000-4000-a123-123456789abc.txt"
	page := strings.Repeat("<p>Offline page fixture.</p>\n", 452)
	after := strings.Replace(page, "fixture", "client-normalized fixture", 1)
	for _, tc := range []struct {
		name   string
		result *agentv1.WriteResult
	}{
		{"success", &agentv1.WriteResult{Result: &agentv1.WriteResult_Success{Success: &agentv1.WriteSuccess{Path: path, LinesCreated: 452, FileSize: int32(len(after)), FileContentAfterWrite: &after}}}},
		{"denied", &agentv1.WriteResult{Result: &agentv1.WriteResult_PermissionDenied{PermissionDenied: &agentv1.WritePermissionDenied{Path: path, Directory: "/remote/workspace", Operation: "write", Error: "read-only mount", IsReadonly: true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
			args := &agentv1.WriteArgs{Path: path, FileBytes: []byte(page), ToolCallId: "page-write", ReturnFileContentAfterWrite: true, EncodingHint: proto.String("UTF-8"), PlanMode: true}
			original, invocation := nativeWriteInvocation(t, args)
			if err := service.handleToolInvocation(stream, invocation); err != nil {
				t.Fatal(err)
			}
			execs := writeFixtureExecs(stream)
			if len(execs) != 1 {
				t.Fatalf("dispatched %d execs, want one", len(execs))
			}
			client := execs[0]
			if client.GetWriteArgs() == nil {
				t.Fatalf("expected one native Write and no pre-read; got %T", client.GetMessage())
			}
			wantRequest := proto.CloneOf(original)
			wantRequest.Id, wantRequest.ExecId = client.Id, client.ExecId // local transport identities
			if !proto.Equal(client, wantRequest) {
				t.Fatal("client did not receive original native request fields")
			}
			reply := &agentv1.ExecClientMessage{Id: client.Id, ExecId: client.ExecId, LocalExecutionTimeMs: proto.Int32(12), HookAdditionalContexts: []*agentv1.HookAdditionalContext{{HookEventName: "afterFileEdit", Content: "offline hook"}}, Message: &agentv1.ExecClientMessage_WriteResult{WriteResult: tc.result}}
			reply.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
			wantReply := proto.CloneOf(reply)
			wantReply.Id, wantReply.ExecId = original.Id, original.ExecId
			// A retried terminal frame must not start another write or generate another card.
			for range 2 {
				if err := service.handleExecResult(InboundIntent{RequestID: stream.RequestID, ExecClientMessage: reply}); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(stream.CursorToolResults); got != 1 {
				t.Fatalf("upstream terminal results = %d, want 1 without answering a read-back", got)
			}
			if got := <-stream.CursorToolResults; !proto.Equal(got, wantReply) {
				t.Fatal("cloud acknowledgment lost result fields or original IDs")
			}
			if got := len(writeFixtureExecs(stream)); got != 1 || len(stream.PendingExecs) != 0 || !stream.CursorInlineToolTurn {
				t.Fatalf("execs = %d, pending = %d, inline = %t", got, len(stream.PendingExecs), stream.CursorInlineToolTurn)
			}
			started, completed := 0, 0
			for _, event := range stream.Backlog {
				update := event.Message.GetInteractionUpdate()
				if s := update.GetToolCallStarted(); s != nil {
					started++
					if s.GetCallId() != args.ToolCallId || s.GetToolCall().GetEditToolCall() == nil {
						t.Fatal("start not attached to native edit card")
					}
				}
				if c := update.GetToolCallCompleted(); c != nil {
					completed++
					if c.GetCallId() != args.ToolCallId || c.GetToolCall().GetEditToolCall() == nil {
						t.Fatal("completion not attached to native edit card")
					}
					if tc.result.GetSuccess() != nil && c.GetToolCall().GetEditToolCall().GetResult().GetSuccess().GetLinesAdded() != 452 {
						t.Fatal("missing +452 edit result")
					}
				}
			}
			if started != 1 || completed != 1 {
				t.Fatalf("edit cards: %d starts, %d completions", started, completed)
			}
			if n := countToolResultEntries(t, snapshotHistoryEntries(stream), args.ToolCallId); n != 1 {
				t.Fatalf("history results = %d, want 1", n)
			}
		})
	}
}

func nativeWriteFixture(t *testing.T, mode agentv1.AgentMode) (*Service, *ActiveStream) {
	t.Helper()
	service := &Service{broker: NewStreamBroker(), execBridge: execbridge.NewBridge(), projector: NewHistoryProjector()}
	stream, err := service.broker.OpenStream("req-write", "conv-write", 1, "model", "model", mode, "offline fixture")
	if err != nil {
		t.Fatal(err)
	}
	setupCheckpointConversation(stream)
	stream.CursorToolResults = make(chan *agentv1.ExecClientMessage, 4)
	t.Cleanup(func() { stream.mu.Lock(); defer stream.mu.Unlock(); stopAllStreamTimersLocked(stream) })
	return service, stream
}

func nativeWriteInvocation(t *testing.T, args *agentv1.WriteArgs) (*agentv1.ExecServerMessage, runtimecore.ToolInvocation) {
	t.Helper()
	msg := &agentv1.ExecServerMessage{Id: 731, ExecId: "cloud-page-exec", MachineId: proto.String("remote-fixture"), AcceptHookAdditionalContexts: proto.Bool(true), Message: &agentv1.ExecServerMessage_WriteArgs{WriteArgs: args}}
	msg.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	argsJSON, err := protojson.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return msg, runtimecore.ToolInvocation{ToolName: "Write", CallID: args.ToolCallId, ModelCallID: "model-write", ArgsJSON: argsJSON, NativeExec: raw}
}

func writeFixtureExecs(stream *ActiveStream) (execs []*agentv1.ExecServerMessage) {
	for _, event := range stream.Backlog {
		if exec := event.Message.GetExecServerMessage(); exec != nil {
			execs = append(execs, exec)
		}
	}
	return execs
}

func TestNativeWriteRoutingSafeguards(t *testing.T) {
	for _, tc := range []struct {
		name, path  string
		mode        agentv1.AgentMode
		compatible  bool
		wantPreRead bool
	}{
		{"plan-denied", "/remote/page.txt", agentv1.AgentMode_AGENT_MODE_PLAN, false, false},
		{"relative-path-denied", "relative/page.txt", agentv1.AgentMode_AGENT_MODE_AGENT, false, false},
		{"canvas", "/remote/.cursor/projects/test/canvases/report.canvas.tsx", agentv1.AgentMode_AGENT_MODE_AGENT, false, true},
		{"compatible-write", "/remote/page.txt", agentv1.AgentMode_AGENT_MODE_AGENT, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, stream := nativeWriteFixture(t, tc.mode)
			_, invocation := nativeWriteInvocation(t, &agentv1.WriteArgs{Path: tc.path, FileText: "offline", ToolCallId: "write"})
			if tc.compatible {
				invocation.NativeExec = nil
			}
			if err := service.handleToolInvocation(stream, invocation); err != nil {
				t.Fatal(err)
			}
			execs := writeFixtureExecs(stream)
			if tc.wantPreRead {
				if len(execs) != 1 || execs[0].GetReadArgs().GetPath() != tc.path {
					t.Fatal("managed editor no longer opens with a pre-read")
				}
			} else if len(execs) != 0 || len(stream.PendingExecs) != 0 {
				t.Fatal("rejected write reached client executor")
			}
		})
	}
}

func TestNativeInternalAgentToolsSpillWriteIsSilentInAllModes(t *testing.T) {
	const spillPath = "/root/cursor-byok/agent-tools/226f3f4f-f560-4513-a657-107666fbc3d7.txt"
	for _, mode := range []agentv1.AgentMode{agentv1.AgentMode_AGENT_MODE_AGENT, agentv1.AgentMode_AGENT_MODE_PLAN} {
		t.Run(mode.String(), func(t *testing.T) {
			service, stream := nativeWriteFixture(t, mode)
			args := &agentv1.WriteArgs{
				Path:       spillPath,
				FileText:   "Releases · 4cuck/cursor-byok · GitHub\nv1.0.50\n",
				ToolCallId: "call-webfetch",
			}
			original, invocation := nativeWriteInvocation(t, args)
			invocation.CallID = original.ExecId // gateway preserves exec ID for spill writes
			if err := service.handleToolInvocation(stream, invocation); err != nil {
				t.Fatal(err)
			}
			execs := writeFixtureExecs(stream)
			if len(execs) != 1 || execs[0].GetWriteArgs().GetPath() != spillPath {
				t.Fatalf("spill write not dispatched directly to IDE: %+v", execs)
			}
			reply := &agentv1.ExecClientMessage{
				Id: execs[0].Id, ExecId: execs[0].ExecId,
				Message: &agentv1.ExecClientMessage_WriteResult{
					WriteResult: &agentv1.WriteResult{
						Result: &agentv1.WriteResult_Success{
							Success: &agentv1.WriteSuccess{Path: spillPath, LinesCreated: 2, FileSize: 46},
						},
					},
				},
			}
			if err := service.handleExecResult(InboundIntent{RequestID: stream.RequestID, ExecClientMessage: reply}); err != nil {
				t.Fatal(err)
			}
			if len(stream.CursorToolResults) != 1 {
				t.Fatalf("upstream results = %d, want 1", len(stream.CursorToolResults))
			}
			got := <-stream.CursorToolResults
			if got.GetId() != original.Id || got.GetExecId() != original.ExecId || got.GetWriteResult().GetSuccess().GetPath() != spillPath {
				t.Fatalf("unexpected upstream WriteResult: %v", got)
			}
			// Must emit ZERO EditToolCall start/complete UI cards and ZERO fake Write history entries.
			for _, event := range stream.Backlog {
				if u := event.Message.GetInteractionUpdate(); u != nil {
					if u.GetToolCallStarted() != nil || u.GetToolCallCompleted() != nil {
						t.Fatalf("internal agent-tools spill write must not emit UI tool card: %v", u)
					}
				}
			}
			if entries := snapshotHistoryEntries(stream); len(entries) != 0 {
				t.Fatalf("internal spill write polluted history with %d entries", len(entries))
			}
			if stream.ToolInvocationCount != 0 {
				t.Fatalf("internal spill write incremented ToolInvocationCount=%d", stream.ToolInvocationCount)
			}
		})
	}
}

func TestComputeEditDiffEmitsUnescapedMultilineUnifiedDiffForNewFile(t *testing.T) {
	after := "Line 1\nLine 2\nLine 3\nLine 4\nLine 5\nLine 6\nLine 7\nLine 8\nLine 9\nLine 10\n"
	diffString, linesAdded, linesRemoved := computeEditDiff("", after)
	if linesAdded != 10 || linesRemoved != 0 {
		t.Fatalf("linesAdded=%d linesRemoved=%d, want 10 and 0", linesAdded, linesRemoved)
	}
	if strings.Contains(diffString, "%0A") {
		t.Fatalf("diffString must not URL-encode newlines as %%0A: %q", diffString)
	}
	want := strings.Join([]string{
		"@@ -0,0 +1,10 @@",
		"+Line 1",
		"+Line 2",
		"+Line 3",
		"+Line 4",
		"+Line 5",
		"+Line 6",
		"+Line 7",
		"+Line 8",
		"+Line 9",
		"+Line 10",
	}, "\n")
	if diffString != want {
		t.Fatalf("diffString =\n%s\nwant:\n%s", diffString, want)
	}

	result := buildSuccessfulWriteResult(`C:\repo\test10.txt`, "", after)
	success := result.GetSuccess()
	if success == nil {
		t.Fatal("expected EditResult_Success")
	}
	if success.BeforeFullFileContent != nil {
		t.Fatalf("new file BeforeFullFileContent must stay nil so Cursor IDE tracks newlyCreatedFiles, got %q", success.GetBeforeFullFileContent())
	}
	if success.GetDiffString() != want {
		t.Fatalf("EditSuccess.DiffString = %q, want %q", success.GetDiffString(), want)
	}
}

func TestComputeEditDiffHandlesNoTrailingNewlineAndSpecialCharacters(t *testing.T) {
	after := "first %0A literal\n@@ not a header @@\n--- literal"
	diffString, linesAdded, linesRemoved := computeEditDiff("", after)
	if linesAdded != 3 || linesRemoved != 0 {
		t.Fatalf("linesAdded=%d linesRemoved=%d, want 3 and 0", linesAdded, linesRemoved)
	}
	want := strings.Join([]string{
		"@@ -0,0 +1,3 @@",
		"+first %0A literal",
		"+@@ not a header @@",
		"+--- literal",
		`\ No newline at end of file`,
	}, "\n")
	if diffString != want {
		t.Fatalf("diffString =\n%s\nwant:\n%s", diffString, want)
	}
}

func TestComputeEditDiffMultiHunkExistingFileAndCRLFNormalization(t *testing.T) {
	beforeLines := make([]string, 12)
	afterLines := make([]string, 12)
	for i := 0; i < 12; i++ {
		line := "line " + string(rune('A'+i))
		beforeLines[i] = line
		afterLines[i] = line
	}
	afterLines[1] = "updated B"
	afterLines[10] = "updated K"

	before := strings.Join(beforeLines, "\n") + "\n"
	afterCRLF := strings.Join(afterLines, "\r\n") + "\r\n"

	diffString, linesAdded, linesRemoved := computeEditDiff(before, afterCRLF)
	if linesAdded != 2 || linesRemoved != 2 {
		t.Fatalf("linesAdded=%d linesRemoved=%d, want 2 and 2", linesAdded, linesRemoved)
	}
	if strings.Contains(diffString, "\r") || strings.Contains(diffString, "%") {
		t.Fatalf("unexpected carriage return or percent-escape in diffString: %q", diffString)
	}
	want := strings.Join([]string{
		"@@ -1,5 +1,5 @@",
		" line A",
		"-line B",
		"+updated B",
		" line C",
		" line D",
		" line E",
		"@@ -8,5 +8,5 @@",
		" line H",
		" line I",
		" line J",
		"-line K",
		"+updated K",
		" line L",
	}, "\n")
	if diffString != want {
		t.Fatalf("diffString =\n%s\nwant:\n%s", diffString, want)
	}
}

func TestDecodeWriteOperationArgsPreservesLeadingAndTrailingWhitespace(t *testing.T) {
	wantContents := "\n  indented first line\nsecond line\n\n"
	writeProto := &agentv1.WriteArgs{
		Path:       `C:\repo\sample.py`,
		FileText:   wantContents,
		ToolCallId: "toolu_write_1",
	}
	raw, err := (protojson.MarshalOptions{EmitUnpopulated: false}).Marshal(writeProto)
	if err != nil {
		t.Fatalf("protojson.Marshal(WriteArgs) error = %v", err)
	}
	decoded, err := decodeWriteOperationArgs(raw)
	if err != nil {
		t.Fatalf("decodeWriteOperationArgs error = %v", err)
	}
	if decoded.Contents != wantContents {
		t.Fatalf("Contents = %q, want %q", decoded.Contents, wantContents)
	}

	started := buildStartedToolCall(runtimecore.ToolInvocation{
		CallID:   "call-write-1",
		ToolName: "Write",
		ArgsJSON: raw,
	})
	if got := started.GetEditToolCall().GetArgs().GetStreamContent(); got != wantContents {
		t.Fatalf("started StreamContent = %q, want %q", got, wantContents)
	}
}

func TestDecodeWriteOperationArgsSupportsFileBytesFallback(t *testing.T) {
	wantContents := "alpha\nbeta\ngamma\n"
	raw, err := json.Marshal(map[string]any{
		"path":      `C:\repo\from_bytes.txt`,
		"fileBytes": base64.StdEncoding.EncodeToString([]byte(wantContents)),
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeWriteOperationArgs(raw)
	if err != nil {
		t.Fatalf("decodeWriteOperationArgs(fileBytes) error = %v", err)
	}
	if decoded.Contents != wantContents {
		t.Fatalf("Contents = %q, want %q", decoded.Contents, wantContents)
	}
}

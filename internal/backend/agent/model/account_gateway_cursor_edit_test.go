package modeladapter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestReadCursorAccountStreamOwnsEditCardsAndSilencesInternalExecs(t *testing.T) {
	lines50 := make([]string, 50)
	for i := range lines50 {
		lines50[i] = fmt.Sprintf("Line %d", i+1)
	}
	content50 := strings.Join(lines50, "\n") + "\n"
	content20 := strings.Join(lines50[:20], "\n") + "\n"
	const targetPath = `c:\repo\test_50_lines.txt`

	frame := func(msg *agentv1.AgentServerMessage) []byte {
		return cursorAccountFrame(mustCursorProto(msg))
	}

	var wire bytes.Buffer
	// 1. Write (create 50-line file): Started -> internal ReadArgs -> internal WriteArgs -> Completed(+50)
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &agentv1.ToolCallStartedUpdate{
						CallId: "edit-create-1",
						ToolCall: &agentv1.ToolCall{
							ToolCallId: proto.String("edit-create-1"),
							Tool: &agentv1.ToolCall_EditToolCall{
								EditToolCall: &agentv1.EditToolCall{
									Args: &agentv1.EditArgs{Path: targetPath, StreamContent: proto.String(content50)},
								},
							},
						},
					},
				},
			},
		},
	}))
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ExecServerMessage{
			ExecServerMessage: &agentv1.ExecServerMessage{
				Id: 1, ExecId: "edit-create-1-read",
				Message: &agentv1.ExecServerMessage_ReadArgs{
					ReadArgs: &agentv1.ReadArgs{Path: targetPath, ToolCallId: "edit-create-1"},
				},
			},
		},
	}))
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ExecServerMessage{
			ExecServerMessage: &agentv1.ExecServerMessage{
				Id: 2, ExecId: "edit-create-1-write",
				Message: &agentv1.ExecServerMessage_WriteArgs{
					WriteArgs: &agentv1.WriteArgs{Path: targetPath, FileText: content50, ToolCallId: "edit-create-1"},
				},
			},
		},
	}))
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ToolCallCompleted{
					ToolCallCompleted: &agentv1.ToolCallCompletedUpdate{
						CallId: "edit-create-1",
						ToolCall: &agentv1.ToolCall{
							ToolCallId: proto.String("edit-create-1"),
							Tool: &agentv1.ToolCall_EditToolCall{
								EditToolCall: &agentv1.EditToolCall{
									Args: &agentv1.EditArgs{Path: targetPath, StreamContent: proto.String(content50)},
									Result: &agentv1.EditResult{
										Result: &agentv1.EditResult_Success{
											Success: &agentv1.EditSuccess{
												Path:                 targetPath,
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
				},
			},
		},
	}))

	// 2. StrReplace (remove 30 lines): Partial -> Delta -> Started -> internal ReadArgs -> internal WriteArgs -> Completed(-30)
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_PartialToolCall{
					PartialToolCall: &agentv1.PartialToolCallUpdate{
						CallId: "edit-trim-2",
						ToolCall: &agentv1.ToolCall{
							ToolCallId: proto.String("edit-trim-2"),
							Tool: &agentv1.ToolCall_EditToolCall{
								EditToolCall: &agentv1.EditToolCall{
									Args: &agentv1.EditArgs{Path: targetPath},
								},
							},
						},
					},
				},
			},
		},
	}))
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ToolCallDelta{
					ToolCallDelta: &agentv1.ToolCallDeltaUpdate{
						CallId: "edit-trim-2",
						ToolCallDelta: &agentv1.ToolCallDelta{
							Delta: &agentv1.ToolCallDelta_EditToolCallDelta{
								EditToolCallDelta: &agentv1.EditToolCallDelta{StreamContentDelta: "Line 20"},
							},
						},
					},
				},
			},
		},
	}))
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &agentv1.ToolCallStartedUpdate{
						CallId: "edit-trim-2",
						ToolCall: &agentv1.ToolCall{
							ToolCallId: proto.String("edit-trim-2"),
							Tool: &agentv1.ToolCall_EditToolCall{
								EditToolCall: &agentv1.EditToolCall{
									Args: &agentv1.EditArgs{Path: targetPath, StreamContent: proto.String("Line 20")},
								},
							},
						},
					},
				},
			},
		},
	}))
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ExecServerMessage{
			ExecServerMessage: &agentv1.ExecServerMessage{
				Id: 3, ExecId: "edit-trim-2-read",
				Message: &agentv1.ExecServerMessage_ReadArgs{
					ReadArgs: &agentv1.ReadArgs{Path: targetPath, ToolCallId: "edit-trim-2"},
				},
			},
		},
	}))
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ExecServerMessage{
			ExecServerMessage: &agentv1.ExecServerMessage{
				Id: 4, ExecId: "edit-trim-2-write",
				Message: &agentv1.ExecServerMessage_WriteArgs{
					WriteArgs: &agentv1.WriteArgs{Path: targetPath, FileText: content20, ToolCallId: "edit-trim-2"},
				},
			},
		},
	}))
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ToolCallCompleted{
					ToolCallCompleted: &agentv1.ToolCallCompletedUpdate{
						CallId: "edit-trim-2",
						ToolCall: &agentv1.ToolCall{
							ToolCallId: proto.String("edit-trim-2"),
							Tool: &agentv1.ToolCall_EditToolCall{
								EditToolCall: &agentv1.EditToolCall{
									Args: &agentv1.EditArgs{Path: targetPath, StreamContent: proto.String("Line 20")},
									Result: &agentv1.EditResult{
										Result: &agentv1.EditResult_Success{
											Success: &agentv1.EditSuccess{
												Path:                  targetPath,
												LinesAdded:            proto.Int32(0),
												LinesRemoved:          proto.Int32(30),
												BeforeFullFileContent: proto.String(content50),
												AfterFullFileContent:  content20,
												DiffString:            proto.String("@@ -18,33 +18,3 @@\n Line 18\n Line 19\n Line 20\n-Line 21"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}))

	// 3. Explicit Read tool call (not owned by an EditToolCall) must remain visible.
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ExecServerMessage{
			ExecServerMessage: &agentv1.ExecServerMessage{
				Id: 5, ExecId: "read-explicit-exec",
				Message: &agentv1.ExecServerMessage_ReadArgs{
					ReadArgs: &agentv1.ReadArgs{Path: targetPath, ToolCallId: "read-explicit", MaxChars: proto.Uint32(100000)},
				},
			},
		},
	}))
	// 4. Non-terminal EditToolCall completion must be ignored.
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_ToolCallCompleted{
					ToolCallCompleted: &agentv1.ToolCallCompletedUpdate{
						CallId: "ignored-empty-edit",
						ToolCall: &agentv1.ToolCall{
							Tool: &agentv1.ToolCall_EditToolCall{
								EditToolCall: &agentv1.EditToolCall{Args: &agentv1.EditArgs{Path: targetPath}},
							},
						},
					},
				},
			},
		},
	}))
	wire.Write(frame(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{}},
			},
		},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	execReplies := make(chan *agentv1.ExecClientMessage, 8)
	var events []ModelEvent
	err := readCursorAccountStream(ctx, func(context.Context, int64, []byte) error { return nil }, "req", "conv", "model", "cursor", 0, bytes.NewReader(wire.Bytes()), nil, execReplies, nil, nil, func(e ModelEvent) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var partials, deltas, editStarts, editCompletes, silentExecs, visibleExecs int
	var completedEdits []*agentv1.ToolCallCompletedUpdate
	for _, e := range events {
		switch e.Kind {
		case ModelEventKindPartialToolCall:
			partials++
			if e.ToolCallID != "edit-trim-2" {
				t.Fatalf("partial callID = %q, want edit-trim-2", e.ToolCallID)
			}
		case ModelEventKindToolCallDelta:
			deltas++
			if e.ToolCallID != "edit-trim-2" {
				t.Fatalf("delta callID = %q, want edit-trim-2", e.ToolCallID)
			}
		case ModelEventKindCursorEditStarted:
			editStarts++
		case ModelEventKindCursorEditCompleted:
			editCompletes++
			completedEdits = append(completedEdits, e.CursorEditCompleted)
		case ModelEventKindToolLikeCompleted:
			if e.ToolInvocation == nil {
				t.Fatal("missing ToolInvocation")
			}
			if e.ToolInvocation.SilentUI {
				silentExecs++
				if e.ToolInvocation.CallID == "edit-create-1" || e.ToolInvocation.CallID == "edit-trim-2" {
					t.Fatalf("silent internal exec collided with parent edit callID %q", e.ToolInvocation.CallID)
				}
			} else {
				visibleExecs++
				if e.ToolInvocation.ToolName != "Read" || e.ToolInvocation.CallID != "read-explicit" {
					t.Fatalf("unexpected visible exec: %+v", e.ToolInvocation)
				}
			}
		}
	}

	if partials != 1 || deltas != 1 || editStarts != 2 || editCompletes != 2 {
		t.Fatalf("counts: partials=%d deltas=%d starts=%d completes=%d", partials, deltas, editStarts, editCompletes)
	}
	if silentExecs != 4 || visibleExecs != 1 {
		t.Fatalf("exec counts: silent=%d visible=%d, want 4 and 1", silentExecs, visibleExecs)
	}
	createSuccess := completedEdits[0].GetToolCall().GetEditToolCall().GetResult().GetSuccess()
	if createSuccess.GetLinesAdded() != 50 || createSuccess.GetLinesRemoved() != 0 || createSuccess.BeforeFullFileContent != nil {
		t.Fatalf("create edit result = %v", createSuccess)
	}
	trimSuccess := completedEdits[1].GetToolCall().GetEditToolCall().GetResult().GetSuccess()
	if trimSuccess.GetLinesAdded() != 0 || trimSuccess.GetLinesRemoved() != 30 || trimSuccess.GetBeforeFullFileContent() != content50 || trimSuccess.GetAfterFullFileContent() != content20 {
		t.Fatalf("trim edit result = %v", trimSuccess)
	}
}

func TestReadCursorAccountStreamEditDisambiguatesIfExecClaimedIDFirst(t *testing.T) {
	var input bytes.Buffer
	for _, raw := range []string{
		`{"execServerMessage":{"id":1,"execId":"r-1","readArgs":{"path":"/repo/a.txt","toolCallId":"shared-1"}}}`,
		`{"interactionUpdate":{"toolCallStarted":{"callId":"shared-1","toolCall":{"toolCallId":"shared-1","editToolCall":{"args":{"path":"/repo/a.txt"}}}}}}`,
		`{"execServerMessage":{"id":2,"execId":"w-2","writeArgs":{"path":"/repo/a.txt","fileText":"after","toolCallId":"shared-1"}}}`,
		`{"interactionUpdate":{"toolCallCompleted":{"callId":"shared-1","toolCall":{"toolCallId":"shared-1","editToolCall":{"args":{"path":"/repo/a.txt"},"result":{"success":{"path":"/repo/a.txt","linesAdded":0,"linesRemoved":30,"beforeFullFileContent":"before","afterFullFileContent":"after"}}}}}}}`,
		`{"interactionUpdate":{"turnEnded":{}}}`,
	} {
		m := &agentv1.AgentServerMessage{}
		if err := protojson.Unmarshal([]byte(raw), m); err != nil {
			t.Fatal(err)
		}
		writeCursorAgentFrame(&input, m)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	go func() {
		_, _ = writer.Write(input.Bytes())
		_ = writer.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	execs := make(chan *agentv1.ExecClientMessage, 4)
	var readID, startID, completeID string
	var writeSilent bool
	err := readCursorAccountStream(ctx, func(context.Context, int64, []byte) error { return nil }, "req", "conv", "model", "cursor", 0, reader, nil, execs, nil, nil, func(e ModelEvent) error {
		switch e.Kind {
		case ModelEventKindToolLikeCompleted:
			if e.ToolInvocation.ToolName == "Read" {
				readID = e.ToolInvocation.CallID
			} else if e.ToolInvocation.ToolName == "Write" {
				writeSilent = e.ToolInvocation.SilentUI
			}
		case ModelEventKindCursorEditStarted:
			startID = e.ToolCallID
		case ModelEventKindCursorEditCompleted:
			completeID = e.ToolCallID
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if readID != "shared-1" || startID == "" || startID == readID || completeID != startID || !writeSilent {
		t.Fatalf("readID=%q startID=%q completeID=%q writeSilent=%t", readID, startID, completeID, writeSilent)
	}
}

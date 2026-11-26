package forwarder

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"cursor/gen/agentv1"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/proto"
)

const (
	awaitTestTerminalFile = `C:\Users\dev\.cursor\projects\repo/terminals/423355.txt`
	awaitTestRunningFile  = "---\npid: 44352\ncwd: \"D:\\\\work\"\ncommand: \"run.ps1 -Seconds 110\"\nstatus: running  \nstarted_at: 2026-11-26T12:32:12.705Z\nrunning_for_ms: 10017    \n---\n"
)

func awaitTestCard(waitID string, blockMS uint32, result *agentv1.AwaitResult) *agentv1.ToolCall {
	return &agentv1.ToolCall{
		ToolCallId: proto.String(waitID),
		Tool: &agentv1.ToolCall_AwaitToolCall{
			AwaitToolCall: &agentv1.AwaitToolCall{
				Args:   &agentv1.AwaitArgs{TaskId: "423355", BlockUntilMs: proto.Uint32(blockMS)},
				Result: result,
			},
		},
	}
}

func applyAwaitStarted(t *testing.T, service *Service, stream *ActiveStream, waitID string, blockMS uint32) {
	t.Helper()
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:       modeladapter.ModelEventKindCursorAwaitStarted,
		ToolCallID: waitID,
		CursorAwaitStarted: &agentv1.ToolCallStartedUpdate{
			CallId:   waitID,
			ToolCall: awaitTestCard(waitID, blockMS, nil),
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func applyAwaitCompleted(t *testing.T, service *Service, stream *ActiveStream, waitID string, blockMS uint32, result *agentv1.AwaitResult) {
	t.Helper()
	if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
		Kind:       modeladapter.ModelEventKindCursorAwaitCompleted,
		ToolCallID: waitID,
		CursorAwaitCompleted: &agentv1.ToolCallCompletedUpdate{
			CallId:   waitID,
			ToolCall: awaitTestCard(waitID, blockMS, result),
		},
	}); err != nil {
		t.Fatal(err)
	}
}

type awaitCards struct {
	started   []*agentv1.ToolCallStartedUpdate
	completed []*agentv1.ToolCallCompletedUpdate
	readCards int
}

func collectAwaitCards(stream *ActiveStream) awaitCards {
	var cards awaitCards
	for _, event := range stream.Backlog {
		update := event.Message.GetInteractionUpdate()
		if started := update.GetToolCallStarted(); started != nil {
			if started.GetToolCall().GetReadToolCall() != nil {
				cards.readCards++
			}
			if started.GetToolCall().GetAwaitToolCall() != nil {
				cards.started = append(cards.started, started)
			}
		}
		if completed := update.GetToolCallCompleted(); completed != nil {
			if completed.GetToolCall().GetReadToolCall() != nil {
				cards.readCards++
			}
			if completed.GetToolCall().GetAwaitToolCall() != nil {
				cards.completed = append(cards.completed, completed)
			}
		}
	}
	return cards
}

func awaitHistory(t *testing.T, stream *ActiveStream) (calls []toolCallEntryPayload, results []toolResultEntryPayload) {
	t.Helper()
	for _, entry := range snapshotHistoryEntries(stream) {
		switch entry.Kind {
		case "tool_call":
			var payload toolCallEntryPayload
			if err := json.Unmarshal(entry.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			calls = append(calls, payload)
		case "tool_result":
			var payload toolResultEntryPayload
			if err := json.Unmarshal(entry.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			results = append(results, payload)
		}
	}
	return calls, results
}

// This is the sequence that ended turns in logged sessions. The model called
// AwaitShell on a running shell; Cursor's server polled the shell's terminal
// file with one identical read after another, about 400ms apart. Each poll was
// handled as a Read the model had made, so the fifth one in a row was refused
// by the repeated-call guard and the turn was closed a few seconds into a
// 110-second command.
func TestCursorAwaitShellPollsAreAnsweredUntilTheWaitEnds(t *testing.T) {
	const waitID = "toolu_01WaitOnTheRunningShell"
	const blockMS = 120000
	service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
	stream.CurrentModelCallID = "model-await"

	applyAwaitStarted(t, service, stream, waitID, blockMS)

	// Three times the number of identical calls the guard allows.
	const polls = 3 * doomLoopHardLimit
	for poll := 1; poll <= polls; poll++ {
		upstream, invocation := nativeReadInvocation(t, uint32(100+poll), fmt.Sprintf("poll-%d", poll),
			&agentv1.ReadArgs{Path: awaitTestTerminalFile, ToolCallId: waitID}, fmt.Sprintf("poll-%d", poll), true)
		if err := service.applyProviderModelEvent(stream, modeladapter.ModelEvent{
			Kind:           modeladapter.ModelEventKindToolLikeCompleted,
			ToolCallID:     invocation.CallID,
			ToolInvocation: &invocation,
		}); err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
		execs := writeFixtureExecs(stream)
		if len(execs) != poll {
			t.Fatalf("poll %d was not sent to the client: %d reads sent so far", poll, len(execs))
		}
		sent := execs[poll-1]
		if sent.GetReadArgs().GetPath() != awaitTestTerminalFile {
			t.Fatalf("poll %d read %q, want the terminal file", poll, sent.GetReadArgs().GetPath())
		}
		if err := service.handleExecResult(InboundIntent{
			RequestID: stream.RequestID,
			ExecClientMessage: &agentv1.ExecClientMessage{
				Id: sent.Id, ExecId: sent.ExecId,
				Message: &agentv1.ExecClientMessage_ReadResult{
					ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
						Path: awaitTestTerminalFile, TotalLines: 10, FileSize: int64(len(awaitTestRunningFile)),
						Output: &agentv1.ReadSuccess_Content{Content: awaitTestRunningFile},
					}}},
				},
			},
		}); err != nil {
			t.Fatalf("poll %d result: %v", poll, err)
		}
		select {
		case answer := <-stream.CursorToolResults:
			if answer.GetId() != upstream.Id || answer.GetExecId() != upstream.ExecId {
				t.Fatalf("poll %d answered as id=%d exec=%q, want Cursor's own id=%d exec=%q", poll, answer.GetId(), answer.GetExecId(), upstream.Id, upstream.ExecId)
			}
			if answer.GetReadResult().GetSuccess().GetContent() != awaitTestRunningFile {
				t.Fatalf("poll %d answer lost the file content: %v", poll, answer)
			}
		default:
			t.Fatalf("poll %d was not answered on the open Cursor stream", poll)
		}
	}

	stream.mu.Lock()
	terminalStatus := isTerminalStreamStatus(stream.Status)
	endsTurn := stream.ProviderTerminalToolInvocation
	stream.mu.Unlock()
	if terminalStatus || endsTurn {
		t.Fatalf("the wait's polls ended the turn: terminal status=%t, turn-ending tool call=%t", terminalStatus, endsTurn)
	}

	exitCode := int32(0)
	applyAwaitCompleted(t, service, stream, waitID, blockMS, &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Success{Success: &agentv1.AwaitSuccess{
		AwaitResult: &agentv1.AwaitSuccess_Complete{Complete: &agentv1.AwaitTaskComplete{
			TaskId: "423355", RuntimeMs: 115180, OutputFilePath: awaitTestTerminalFile, OutputLength: 721, ExitCode: &exitCode,
		}},
	}}})

	cards := collectAwaitCards(stream)
	if cards.readCards != 0 {
		t.Fatalf("%d Read cards shown for the wait's polls, want none", cards.readCards)
	}
	if len(cards.started) != 1 || cards.started[0].GetCallId() != waitID {
		t.Fatalf("wait start cards = %v, want one under %s", cards.started, waitID)
	}
	if len(cards.completed) != 1 || cards.completed[0].GetCallId() != waitID {
		t.Fatalf("wait completion cards = %v, want one under %s", cards.completed, waitID)
	}
	if got := cards.completed[0].GetToolCall().GetAwaitToolCall().GetResult().GetSuccess().GetComplete().GetRuntimeMs(); got != 115180 {
		t.Fatalf("completed card runtime = %d, want Cursor's 115180", got)
	}

	calls, results := awaitHistory(t, stream)
	if len(calls) != 1 || calls[0].ToolCallID != waitID || calls[0].ToolName != awaitShellToolName {
		t.Fatalf("history tool calls = %+v, want the one AwaitShell call", calls)
	}
	if len(results) != 1 || results[0].ToolCallID != waitID || results[0].ToolName != awaitShellToolName {
		t.Fatalf("history tool results = %+v, want the one AwaitShell result", results)
	}
	if strings.Contains(results[0].ResultText, "连续调用") {
		t.Fatalf("the wait was answered with the repeated-call refusal: %s", results[0].ResultText)
	}
	var summary awaitShellResult
	if err := json.Unmarshal([]byte(results[0].ResultText), &summary); err != nil {
		t.Fatalf("history result is not the AwaitShell result shape: %v\n%s", err, results[0].ResultText)
	}
	if summary.Status != backgroundShellStatusCompleted || summary.ExitCode == nil || *summary.ExitCode != 0 || summary.RuntimeMS != 115180 || summary.ShellID != "423355" {
		t.Fatalf("history result = %+v, want completed, exit 0, 115180ms, shell 423355", summary)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(results[0].Arguments), &args); err != nil || args["shell_id"] != "423355" || args["block_until_ms"] != float64(blockMS) {
		t.Fatalf("history arguments = %s (err %v), want shell_id and block_until_ms as AwaitShell takes them", results[0].Arguments, err)
	}

	stream.mu.Lock()
	open, inline, counted := len(stream.activeCursorAwaits), stream.CursorInlineToolTurn, stream.ToolInvocationCount
	stream.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d waits still open after completion", open)
	}
	if !inline {
		t.Fatal("a wait answered on the open Cursor stream must not start another pass when the model stops")
	}
	if counted != 1 {
		t.Fatalf("tool calls counted for the turn = %d, want 1: the wait, not its polls", counted)
	}
}

// A wait whose result never arrives (Cursor's stream ended, or the user
// stopped the turn) still gets a result: the card closes and history does not
// keep a call without one.
func TestCursorAwaitCardClosesWithoutAReportedResult(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(service *Service, stream *ActiveStream) error
		want  string
	}{
		{
			name: "stream ended",
			close: func(service *Service, stream *ActiveStream) error {
				return service.closeUnreportedCursorAwaits(stream, "Cursor ended the stream before the wait reported a result.")
			},
			want: "Cursor ended the stream before the wait reported a result.",
		},
		{
			name: "turn canceled",
			close: func(service *Service, stream *ActiveStream) error {
				service.closeCanceledPendingInteractions(stream, "user_stopped_generation")
				return nil
			},
			want: "user_stopped_generation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const waitID = "toolu_wait_dangling"
			service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
			applyAwaitStarted(t, service, stream, waitID, 60000)
			if err := tc.close(service, stream); err != nil {
				t.Fatal(err)
			}
			cards := collectAwaitCards(stream)
			if len(cards.completed) != 1 || cards.completed[0].GetCallId() != waitID {
				t.Fatalf("completion cards = %v, want the open wait closed", cards.completed)
			}
			if reason := cards.completed[0].GetToolCall().GetAwaitToolCall().GetResult().GetError().GetError(); !strings.Contains(reason, tc.want) {
				t.Fatalf("closing reason = %q, want it to say %q", reason, tc.want)
			}
			if n := countToolResultEntries(t, snapshotHistoryEntries(stream), waitID); n != 1 {
				t.Fatalf("history results for the wait = %d, want 1", n)
			}
			stream.mu.Lock()
			open := len(stream.activeCursorAwaits)
			stream.mu.Unlock()
			if open != 0 {
				t.Fatalf("%d waits still open", open)
			}
			// Closing again must not write a second result.
			if err := service.closeUnreportedCursorAwaits(stream, "again"); err != nil {
				t.Fatal(err)
			}
			if n := countToolResultEntries(t, snapshotHistoryEntries(stream), waitID); n != 1 {
				t.Fatalf("history results after a second close = %d, want 1", n)
			}
		})
	}
}

// A completion whose start was never reported still shows the wait and counts
// it as the turn's tool call; a repeated completion is ignored.
func TestCursorAwaitCompletedWithoutReportedStart(t *testing.T) {
	const waitID = "toolu_wait_no_start"
	service, stream := nativeWriteFixture(t, agentv1.AgentMode_AGENT_MODE_AGENT)
	stillRunning := &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Success{Success: &agentv1.AwaitSuccess{
		AwaitResult: &agentv1.AwaitSuccess_StillRunning{StillRunning: &agentv1.AwaitTaskStillRunning{
			TaskId: "423355", RuntimeMs: 5017, OutputFilePath: awaitTestTerminalFile, OutputLength: 453,
		}},
	}}}

	applyAwaitCompleted(t, service, stream, waitID, 0, stillRunning)
	applyAwaitCompleted(t, service, stream, waitID, 0, stillRunning)

	cards := collectAwaitCards(stream)
	if len(cards.started) != 0 || len(cards.completed) != 1 {
		t.Fatalf("cards: started=%d completed=%d, want only one completion", len(cards.started), len(cards.completed))
	}
	_, results := awaitHistory(t, stream)
	if len(results) != 1 {
		t.Fatalf("history results = %d, want 1", len(results))
	}
	var summary awaitShellResult
	if err := json.Unmarshal([]byte(results[0].ResultText), &summary); err != nil {
		t.Fatal(err)
	}
	// block_until_ms 0 is a status check: still running is its answer, not a
	// wait that ran out.
	if summary.Status != backgroundShellStatusRunning || summary.TimedOut || summary.RuntimeMS != 5017 {
		t.Fatalf("history result = %+v, want running, not timed out, 5017ms", summary)
	}
	stream.mu.Lock()
	inline, counted := stream.CursorInlineToolTurn, stream.ToolInvocationCount
	stream.mu.Unlock()
	if !inline || counted != 1 {
		t.Fatalf("inline=%t counted=%d, want the wait counted once and answered inline", inline, counted)
	}
}

func TestCursorAwaitResultSummary(t *testing.T) {
	match := "listening on :8080"
	exitCode := int32(3)
	blocking := &agentv1.AwaitArgs{TaskId: "77", BlockUntilMs: proto.Uint32(30000), Regex: proto.String("listening")}
	for _, tc := range []struct {
		name   string
		args   *agentv1.AwaitArgs
		result *agentv1.AwaitResult
		check  func(t *testing.T, summary awaitShellResult)
	}{
		{
			name: "finished with a failing exit code",
			args: &agentv1.AwaitArgs{TaskId: "77", BlockUntilMs: proto.Uint32(30000)},
			result: &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Complete{Complete: &agentv1.AwaitTaskComplete{
				TaskId: "77", RuntimeMs: 900, ExitCode: &exitCode,
			}}},
			check: func(t *testing.T, summary awaitShellResult) {
				if summary.Status != backgroundShellStatusCompleted || summary.ExitCode == nil || *summary.ExitCode != 3 || !strings.Contains(summary.Message, "exit code: 3") {
					t.Fatalf("summary = %+v", summary)
				}
			},
		},
		{
			name: "pattern ended a blocking wait while the command runs on",
			args: blocking,
			result: &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Success{Success: &agentv1.AwaitSuccess{
				AwaitResult: &agentv1.AwaitSuccess_StillRunning{StillRunning: &agentv1.AwaitTaskStillRunning{
					TaskId: "77", RuntimeMs: 1200, RegexRequested: true, RegexMatch: &match,
				}},
			}}},
			check: func(t *testing.T, summary awaitShellResult) {
				if summary.Status != backgroundShellStatusRunning || !summary.Matched || summary.TimedOut || summary.RegexMatch == nil || *summary.RegexMatch != match {
					t.Fatalf("summary = %+v", summary)
				}
			},
		},
		{
			name: "blocking wait ran out",
			args: blocking,
			result: &agentv1.AwaitResult{Result: &agentv1.AwaitResult_StillRunning{StillRunning: &agentv1.AwaitTaskStillRunning{
				TaskId: "77", RuntimeMs: 30000, RegexRequested: true,
			}}},
			check: func(t *testing.T, summary awaitShellResult) {
				if summary.Status != backgroundShellStatusRunning || summary.Matched || !summary.TimedOut || !strings.Contains(summary.Message, "Pattern did NOT match") {
					t.Fatalf("summary = %+v", summary)
				}
			},
		},
		{
			name: "plain sleep",
			args: &agentv1.AwaitArgs{BlockUntilMs: proto.Uint32(5000)},
			result: &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Success{Success: &agentv1.AwaitSuccess{
				AwaitResult: &agentv1.AwaitSuccess_Complete{Complete: &agentv1.AwaitTaskComplete{RuntimeMs: 5000}},
			}}},
			check: func(t *testing.T, summary awaitShellResult) {
				if summary.Status != "waited" || summary.Message != "Slept for 5s." || summary.ShellID != "" {
					t.Fatalf("summary = %+v", summary)
				}
			},
		},
		{
			name:   "error",
			args:   &agentv1.AwaitArgs{TaskId: "77"},
			result: &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Error{Error: &agentv1.AwaitError{Error: "No shell found for id 77"}}},
			check: func(t *testing.T, summary awaitShellResult) {
				if summary.Status != "error" || !strings.Contains(summary.Message, "No shell found for id 77") {
					t.Fatalf("summary = %+v", summary)
				}
			},
		},
		{
			name:   "no result at all",
			args:   &agentv1.AwaitArgs{TaskId: "77"},
			result: nil,
			check: func(t *testing.T, summary awaitShellResult) {
				if summary.Status != "error" || summary.Message == "" {
					t.Fatalf("summary = %+v", summary)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, cursorAwaitResultSummary(tc.args, tc.result))
		})
	}
}

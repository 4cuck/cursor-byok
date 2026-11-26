package modeladapter

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/encoding/protojson"
)

// awaitTerminalFile is the path Cursor's server reads while AwaitShell waits:
// the client's terminals folder, a slash, and the shell id.
const awaitTerminalFile = `C:\\Users\\dev\\.cursor\\projects\\repo/terminals/423355.txt`

// readCursorAccountEvents runs the stream reader over the given server
// messages (protojson) and returns every event it emitted.
func readCursorAccountEvents(t *testing.T, messages ...string) []ModelEvent {
	t.Helper()
	var wire bytes.Buffer
	for _, raw := range messages {
		msg := &agentv1.AgentServerMessage{}
		if err := protojson.Unmarshal([]byte(raw), msg); err != nil {
			t.Fatalf("bad test message %s: %v", raw, err)
		}
		writeCursorAgentFrame(&wire, msg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Results are never sent back in these tests; the channel only tells the
	// reader that tool results are answered on the open stream.
	execReplies := make(chan *agentv1.ExecClientMessage, 64)
	var events []ModelEvent
	err := readCursorAccountStream(ctx, func(context.Context, int64, []byte) error { return nil }, "req", "conv", "model", "cursor", 0,
		bytes.NewReader(wire.Bytes()), nil, execReplies, nil, nil, func(event ModelEvent) error {
			events = append(events, event)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func awaitPollExec(id int, toolCallID string) string {
	return fmt.Sprintf(`{"execServerMessage":{"id":%d,"execId":"poll-%d","readArgs":{"path":"%s","toolCallId":"%s"}}}`, id, id, awaitTerminalFile, toolCallID)
}

type awaitStreamSummary struct {
	awaitStarted   []string
	awaitCompleted []string
	silentReads    []string
	visibleReads   []string
	order          []string
}

func summarizeAwaitStream(t *testing.T, events []ModelEvent) awaitStreamSummary {
	t.Helper()
	var summary awaitStreamSummary
	for _, event := range events {
		switch event.Kind {
		case ModelEventKindCursorAwaitStarted:
			if event.CursorAwaitStarted.GetToolCall().GetAwaitToolCall() == nil {
				t.Fatalf("await start without an await card: %v", event.CursorAwaitStarted)
			}
			summary.awaitStarted = append(summary.awaitStarted, event.ToolCallID)
			summary.order = append(summary.order, "started")
		case ModelEventKindCursorAwaitCompleted:
			if event.CursorAwaitCompleted.GetToolCall().GetAwaitToolCall() == nil {
				t.Fatalf("await completion without an await card: %v", event.CursorAwaitCompleted)
			}
			summary.awaitCompleted = append(summary.awaitCompleted, event.ToolCallID)
			summary.order = append(summary.order, "completed")
		case ModelEventKindToolLikeCompleted:
			invocation := event.ToolInvocation
			if invocation == nil || invocation.ToolName != "Read" {
				t.Fatalf("unexpected tool invocation: %+v", invocation)
			}
			if len(invocation.NativeExec) == 0 {
				t.Fatalf("read %q lost the exec it has to answer", invocation.CallID)
			}
			if invocation.SilentUI {
				summary.silentReads = append(summary.silentReads, invocation.CallID)
				summary.order = append(summary.order, "poll")
			} else {
				summary.visibleReads = append(summary.visibleReads, invocation.CallID)
				summary.order = append(summary.order, "read")
			}
		}
	}
	return summary
}

func requireDistinct(t *testing.T, what string, ids []string, reserved ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, id := range reserved {
		seen[id] = true
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			t.Fatalf("%s id %q is empty or already used (all: %v, reserved: %v)", what, id, ids, reserved)
		}
		seen[id] = true
	}
}

// The sequence below is what Cursor's server sent for a blocking AwaitShell in
// a logged session: the same tool call id on one terminal-file read after
// another, about 400ms apart. Each read used to be dispatched as a Read the
// model had called, so the fifth identical one tripped the forwarder's
// repeated-call guard and ended the turn.
func TestReadCursorAccountStreamAnswersAwaitShellPollsSilently(t *testing.T) {
	const waitID = "toolu_01WaitOnTheRunningShell"
	messages := []string{
		`{"interactionUpdate":{"toolCallStarted":{"callId":"` + waitID + `","toolCall":{"toolCallId":"` + waitID + `","awaitToolCall":{"args":{"taskId":"423355","blockUntilMs":120000}}}}}}`,
	}
	const polls = 12
	for i := 1; i <= polls; i++ {
		messages = append(messages, awaitPollExec(i, waitID))
	}
	messages = append(messages,
		`{"interactionUpdate":{"toolCallCompleted":{"callId":"`+waitID+`","toolCall":{"toolCallId":"`+waitID+`","awaitToolCall":{"args":{"taskId":"423355","blockUntilMs":120000},"result":{"success":{"complete":{"taskId":"423355","runtimeMs":"115180","outputFilePath":"`+awaitTerminalFile+`","outputLength":"721","exitCode":0}}}}}}}}`,
		`{"interactionUpdate":{"textDelta":{"text":"The run finished."}}}`,
		`{"interactionUpdate":{"turnEnded":{}}}`,
	)

	summary := summarizeAwaitStream(t, readCursorAccountEvents(t, messages...))

	if len(summary.visibleReads) != 0 {
		t.Fatalf("polls shown as Read calls of the model: %v", summary.visibleReads)
	}
	if len(summary.silentReads) != polls {
		t.Fatalf("silent polls = %d, want %d (every poll still has to be answered)", len(summary.silentReads), polls)
	}
	if len(summary.awaitStarted) != 1 || summary.awaitStarted[0] != waitID {
		t.Fatalf("await start cards = %v, want one under %s", summary.awaitStarted, waitID)
	}
	if len(summary.awaitCompleted) != 1 || summary.awaitCompleted[0] != waitID {
		t.Fatalf("await completion cards = %v, want one under %s", summary.awaitCompleted, waitID)
	}
	// A poll must not share an id with the wait's card or with another poll:
	// the forwarder keys pending work by call id.
	requireDistinct(t, "poll", summary.silentReads, waitID)
	if first, last := summary.order[0], summary.order[len(summary.order)-1]; first != "started" || last != "completed" {
		t.Fatalf("event order = %v, want the card to open before and close after the polls", summary.order)
	}
}

// Without any card update, a tool call that keeps reading is still polling:
// only its first read may look like a Read of the model.
func TestReadCursorAccountStreamSilencesRepeatedReadsOfOneToolCall(t *testing.T) {
	const waitID = "toolu_wait_without_card"
	var messages []string
	for i := 1; i <= 8; i++ {
		messages = append(messages, awaitPollExec(i, waitID))
	}
	messages = append(messages, `{"interactionUpdate":{"turnEnded":{}}}`)

	summary := summarizeAwaitStream(t, readCursorAccountEvents(t, messages...))

	if len(summary.visibleReads) != 1 || summary.visibleReads[0] != waitID {
		t.Fatalf("visible reads = %v, want only the first under %s", summary.visibleReads, waitID)
	}
	if len(summary.silentReads) != 7 {
		t.Fatalf("silent polls = %d, want 7", len(summary.silentReads))
	}
	requireDistinct(t, "poll", summary.silentReads, waitID)
}

// Reads the model itself calls keep their cards, also when it reads the same
// file again and again: every call has its own id.
func TestReadCursorAccountStreamKeepsReadsOfTheModelVisible(t *testing.T) {
	var messages []string
	want := []string{}
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("toolu_read_%d", i)
		want = append(want, id)
		messages = append(messages, fmt.Sprintf(`{"execServerMessage":{"id":%d,"execId":"read-%d","readArgs":{"path":"%s","toolCallId":"%s","maxChars":100000}}}`, i, i, awaitTerminalFile, id))
	}
	messages = append(messages, `{"interactionUpdate":{"turnEnded":{}}}`)

	summary := summarizeAwaitStream(t, readCursorAccountEvents(t, messages...))

	if len(summary.silentReads) != 0 {
		t.Fatalf("reads of the model answered silently: %v", summary.silentReads)
	}
	if strings.Join(summary.visibleReads, ",") != strings.Join(want, ",") {
		t.Fatalf("visible reads = %v, want %v", summary.visibleReads, want)
	}
}

// The card may be announced while its arguments still stream. The polls that
// follow are recognized from that announcement on.
func TestReadCursorAccountStreamRecognizesAwaitFromPartialToolCall(t *testing.T) {
	const waitID = "toolu_wait_partial"
	summary := summarizeAwaitStream(t, readCursorAccountEvents(t,
		`{"interactionUpdate":{"partialToolCall":{"callId":"`+waitID+`","toolCall":{"toolCallId":"`+waitID+`","awaitToolCall":{"args":{"taskId":"423355"}}}}}}`,
		awaitPollExec(1, waitID),
		awaitPollExec(2, waitID),
		`{"interactionUpdate":{"toolCallCompleted":{"callId":"`+waitID+`","toolCall":{"toolCallId":"`+waitID+`","awaitToolCall":{"args":{"taskId":"423355","blockUntilMs":0},"result":{"success":{"stillRunning":{"taskId":"423355","runtimeMs":"5017","outputFilePath":"`+awaitTerminalFile+`","outputLength":"453"}}}}}}}}`,
		`{"interactionUpdate":{"turnEnded":{}}}`,
	))

	if len(summary.visibleReads) != 0 || len(summary.silentReads) != 2 {
		t.Fatalf("visible=%v silent=%v, want both polls silent", summary.visibleReads, summary.silentReads)
	}
	if len(summary.awaitCompleted) != 1 || summary.awaitCompleted[0] != waitID {
		t.Fatalf("await completion cards = %v, want one under %s", summary.awaitCompleted, waitID)
	}
}

// If a poll arrives before the card is announced, that poll already took the
// call's id for its Read card. The wait's card then gets an id of its own and
// start and completion agree on it.
func TestReadCursorAccountStreamAwaitCardAfterFirstPoll(t *testing.T) {
	const waitID = "toolu_wait_late_card"
	summary := summarizeAwaitStream(t, readCursorAccountEvents(t,
		awaitPollExec(1, waitID),
		`{"interactionUpdate":{"toolCallStarted":{"callId":"`+waitID+`","toolCall":{"toolCallId":"`+waitID+`","awaitToolCall":{"args":{"taskId":"423355","blockUntilMs":30000}}}}}}`,
		awaitPollExec(2, waitID),
		awaitPollExec(3, waitID),
		`{"interactionUpdate":{"toolCallCompleted":{"callId":"`+waitID+`","toolCall":{"toolCallId":"`+waitID+`","awaitToolCall":{"args":{"taskId":"423355","blockUntilMs":30000},"result":{"success":{"complete":{"taskId":"423355","runtimeMs":"900","exitCode":1}}}}}}}}`,
		`{"interactionUpdate":{"turnEnded":{}}}`,
	))

	if len(summary.visibleReads) != 1 || summary.visibleReads[0] != waitID {
		t.Fatalf("visible reads = %v, want the first poll under %s", summary.visibleReads, waitID)
	}
	if len(summary.silentReads) != 2 {
		t.Fatalf("silent polls = %d, want 2", len(summary.silentReads))
	}
	if len(summary.awaitStarted) != 1 || len(summary.awaitCompleted) != 1 {
		t.Fatalf("await cards: started=%v completed=%v, want one of each", summary.awaitStarted, summary.awaitCompleted)
	}
	cardID := summary.awaitStarted[0]
	if cardID == waitID || cardID != summary.awaitCompleted[0] {
		t.Fatalf("await card ids: started=%q completed=%q, want one id other than the Read card's %q", cardID, summary.awaitCompleted[0], waitID)
	}
	requireDistinct(t, "poll", summary.silentReads, waitID, cardID)
}

// An edit's internal reads keep working as before: they belong to the edit,
// and the repeated-read rule does not get in their way.
func TestReadCursorAccountStreamAwaitRuleLeavesEditReadsToTheEdit(t *testing.T) {
	events := readCursorAccountEvents(t,
		`{"interactionUpdate":{"toolCallStarted":{"callId":"edit-1","toolCall":{"toolCallId":"edit-1","editToolCall":{"args":{"path":"/repo/a.txt"}}}}}}`,
		`{"execServerMessage":{"id":1,"execId":"edit-1-read","readArgs":{"path":"/repo/a.txt","toolCallId":"edit-1"}}}`,
		`{"execServerMessage":{"id":2,"execId":"edit-1-write","writeArgs":{"path":"/repo/a.txt","fileText":"after","toolCallId":"edit-1"}}}`,
		`{"execServerMessage":{"id":3,"execId":"read-own","readArgs":{"path":"/repo/a.txt","toolCallId":"toolu_read_after_edit","maxChars":100000}}}`,
		`{"interactionUpdate":{"turnEnded":{}}}`,
	)
	var silent, visible []string
	for _, event := range events {
		if event.Kind != ModelEventKindToolLikeCompleted {
			continue
		}
		if event.ToolInvocation.SilentUI {
			silent = append(silent, event.ToolInvocation.ToolName)
		} else {
			visible = append(visible, event.ToolInvocation.CallID)
		}
	}
	if strings.Join(silent, ",") != "Read,Write" {
		t.Fatalf("silent execs = %v, want the edit's Read and Write", silent)
	}
	if strings.Join(visible, ",") != "toolu_read_after_edit" {
		t.Fatalf("visible execs = %v, want the model's own Read", visible)
	}
}

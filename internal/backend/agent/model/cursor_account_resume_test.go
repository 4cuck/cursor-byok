package modeladapter

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/gen/aiserverv1"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// resumeBackend is a Cursor server whose RunSSE streams follow a script. The
// script of a stream gets the frames the client appends to it, the run
// request first.
type resumeBackend struct {
	t        *testing.T
	mu       sync.Mutex
	channels map[string]chan *agentv1.AgentClientMessage
	streams  int
	play     func(stream int, send func(*agentv1.AgentServerMessage), frames <-chan *agentv1.AgentClientMessage)
}

func (b *resumeBackend) channel(requestID string) chan *agentv1.AgentClientMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.channels == nil {
		b.channels = make(map[string]chan *agentv1.AgentClientMessage)
	}
	ch := b.channels[requestID]
	if ch == nil {
		ch = make(chan *agentv1.AgentClientMessage, 16)
		b.channels[requestID] = ch
	}
	return ch
}

func (b *resumeBackend) streamCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.streams
}

// errLoseStream makes a stream script end the way a lost connection does.
var errLoseStream = errors.New("lose the stream")

func startResumeBackend(t *testing.T, play func(stream int, send func(*agentv1.AgentServerMessage), frames <-chan *agentv1.AgentClientMessage)) *resumeBackend {
	t.Helper()
	// Not t.TempDir(): the first log line of the process opens the log file
	// under this home and keeps it open, and a TempDir that cannot be removed
	// fails the test on Windows.
	home, err := os.MkdirTemp("", "cursor-resume-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("CURSOR_BYOK_HOME", home)
	backend := &resumeBackend{t: t, play: play}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		id := &aiserverv1.BidiRequestId{}
		if r.URL.Path != cursorAccountRunPath || len(body) < 5 || proto.Unmarshal(body[5:], id) != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		backend.mu.Lock()
		backend.streams++
		stream := backend.streams
		backend.mu.Unlock()
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == errLoseStream {
					// The connection goes away with the response unfinished.
					panic(http.ErrAbortHandler)
				}
				panic(recovered)
			}
		}()
		backend.play(stream, func(message *agentv1.AgentServerMessage) {
			_, _ = w.Write(cursorAccountFrame(mustCursorProto(message)))
			flusher.Flush()
		}, backend.channel(id.GetRequestId()))
	}))
	t.Cleanup(agent.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		switch r.URL.Path {
		case cursorAccountServerConfigPath:
			raw, _ := proto.Marshal(&aiserverv1.GetServerConfigResponse{AgentUrlConfig: &aiserverv1.AgentUrlConfig{AgentnUrl: agent.URL}})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		case cursorAccountBidiAppendPath:
			request := &aiserverv1.BidiAppendRequest{}
			if err := proto.Unmarshal(body, request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			raw, err := hex.DecodeString(request.GetData())
			message := &agentv1.AgentClientMessage{}
			if err != nil || proto.Unmarshal(raw, message) != nil {
				http.Error(w, "bad frame", http.StatusBadRequest)
				return
			}
			backend.channel(request.GetRequestId().GetRequestId()) <- message
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	previous := cursorAccountBackendRoot
	cursorAccountBackendRoot = api.URL
	t.Cleanup(func() { cursorAccountBackendRoot = previous })
	previousBackoff := cursorAccountRecoveryBackoff
	cursorAccountRecoveryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { cursorAccountRecoveryBackoff = previousBackoff })
	return backend
}

func resumeThinking(text string) *agentv1.AgentServerMessage {
	return &agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_InteractionUpdate{InteractionUpdate: &agentv1.InteractionUpdate{
		Message: &agentv1.InteractionUpdate_ThinkingDelta{ThinkingDelta: &agentv1.ThinkingDeltaUpdate{Text: text}},
	}}}
}

func resumeText(text string) *agentv1.AgentServerMessage {
	return &agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_InteractionUpdate{InteractionUpdate: &agentv1.InteractionUpdate{
		Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: text}},
	}}}
}

func resumeTurnEnded() *agentv1.AgentServerMessage {
	return &agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_InteractionUpdate{InteractionUpdate: &agentv1.InteractionUpdate{
		Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{}},
	}}}
}

func resumeCheckpoint(turns ...string) *agentv1.AgentServerMessage {
	state := &agentv1.ConversationStateStructure{}
	for _, turn := range turns {
		state.Turns = append(state.Turns, []byte(turn))
	}
	return &agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_ConversationCheckpointUpdate{ConversationCheckpointUpdate: state}}
}

// resumePass runs one pass of a run and returns its events and error.
func resumePass(t *testing.T, conversationID, runID string, messages ...Message) ([]ModelEvent, error) {
	t.Helper()
	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var events []ModelEvent
	err := gateway.Stream(ctx, StreamRequest{
		RequestID:       runID,
		RunID:           runID,
		ModelID:         "claude-4.5-opus-high",
		GroupName:       "cursor_account2",
		CursorAccountID: "account-2",
		ConversationID:  conversationID,
		Messages:        messages,
	}, func(event ModelEvent) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

func resumeFinishReason(events []ModelEvent) string {
	for _, event := range events {
		if event.Kind == ModelEventKindTurnFinished {
			return event.FinishReason
		}
	}
	return "(no turn_finished event)"
}

func newResumeRun(t *testing.T) (conversationID, runID string) {
	t.Helper()
	conversationID, runID = uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		ClearCursorAccountState(conversationID)
		cursorAccountForgetInterruptions(runID)
	})
	return conversationID, runID
}

// A stream lost after Cursor sent a checkpoint is not the end of the turn:
// the pass ends as interrupted, and the next pass of the run resumes from the
// checkpoint with a ResumeAction, whatever the transcript ends with.
func TestCursorAccountResumesFromCheckpointAfterLostStream(t *testing.T) {
	var runs []*agentv1.AgentRunRequest
	var mu sync.Mutex
	backend := startResumeBackend(t, func(stream int, send func(*agentv1.AgentServerMessage), frames <-chan *agentv1.AgentClientMessage) {
		run := (<-frames).GetRunRequest()
		mu.Lock()
		runs = append(runs, run)
		mu.Unlock()
		if stream == 1 {
			send(resumeThinking("looking at the file"))
			send(resumeCheckpoint("turn-1-step-1"))
			send(resumeThinking(" and then"))
			panic(errLoseStream)
		}
		send(resumeText("done"))
		send(resumeTurnEnded())
	})
	conversationID, runID := newResumeRun(t)
	question := Message{Role: "user", Content: "what is in the file?"}

	events, err := resumePass(t, conversationID, runID, question)
	if err != nil {
		t.Fatalf("the pass that lost its stream failed: %v", err)
	}
	if got := resumeFinishReason(events); got != FinishReasonCursorStreamInterrupted {
		t.Fatalf("finish reason of the pass that lost its stream = %q, want %q", got, FinishReasonCursorStreamInterrupted)
	}
	var thinkingOpen bool
	for _, event := range events {
		switch event.Kind {
		case ModelEventKindThinkingDelta:
			thinkingOpen = true
		case ModelEventKindThinkingCompleted:
			thinkingOpen = false
		}
	}
	if thinkingOpen {
		t.Fatal("the thinking block that was open when the stream was lost was not closed")
	}

	// The transcript of the next pass still ends with the user's message:
	// nothing of the lost pass was recorded as assistant output.
	events, err = resumePass(t, conversationID, runID, question)
	if err != nil {
		t.Fatalf("the resuming pass failed: %v", err)
	}
	if got := resumeFinishReason(events); got != "stop" {
		t.Fatalf("finish reason of the resuming pass = %q, want stop", got)
	}
	if backend.streamCount() != 2 {
		t.Fatalf("streams opened = %d, want 2", backend.streamCount())
	}
	mu.Lock()
	defer mu.Unlock()
	first, second := runs[0], runs[1]
	if first.GetAction().GetUserMessageAction().GetUserMessage().GetText() != question.Content {
		t.Fatalf("first run action = %v, want the user's message", first.GetAction())
	}
	if second.GetAction().GetResumeAction() == nil {
		t.Fatalf("the resuming run sent %v, want a ResumeAction: the checkpoint already holds the user's message", second.GetAction())
	}
	if turns := second.GetConversationState().GetTurns(); len(turns) != 1 || string(turns[0]) != "turn-1-step-1" {
		t.Fatalf("the resuming run sent state turns %q, want the checkpoint of the lost stream", turns)
	}
	if second.GetConversationId() == first.GetConversationId() {
		t.Fatal("the resuming run reused the upstream conversation id of the lost stream")
	}
}

// The call that is to resume the turn can fail before it reaches Cursor: the
// network that dropped the stream may still be down. The call after it still
// has to resume the turn, not start over from the transcript.
func TestCursorAccountStillResumesAfterAFailedResumingCall(t *testing.T) {
	var runs []*agentv1.AgentRunRequest
	var mu sync.Mutex
	backend := startResumeBackend(t, func(stream int, send func(*agentv1.AgentServerMessage), frames <-chan *agentv1.AgentClientMessage) {
		run := (<-frames).GetRunRequest()
		mu.Lock()
		runs = append(runs, run)
		mu.Unlock()
		if stream == 1 {
			send(resumeThinking("hm"))
			send(resumeCheckpoint("turn-1-step-1"))
			panic(errLoseStream)
		}
		send(resumeText("done"))
		send(resumeTurnEnded())
	})
	conversationID, runID := newResumeRun(t)
	question := Message{Role: "user", Content: "what is in the file?"}
	if events, err := resumePass(t, conversationID, runID, question); err != nil || resumeFinishReason(events) != FinishReasonCursorStreamInterrupted {
		t.Fatalf("the pass that lost its stream: err=%v finish=%q", err, resumeFinishReason(events))
	}

	// The resuming call never gets anywhere.
	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gateway.Stream(dead, StreamRequest{
		RequestID: runID, RunID: runID, ModelID: "claude-4.5-opus-high", GroupName: "cursor_account2", CursorAccountID: "account-2",
		ConversationID: conversationID, Messages: []Message{question},
	}, func(ModelEvent) error { return nil }); err == nil {
		t.Fatal("a call on a dead context succeeded")
	}
	if backend.streamCount() != 1 {
		t.Fatalf("the failed call opened a stream: %d streams", backend.streamCount())
	}

	events, err := resumePass(t, conversationID, runID, question)
	if err != nil || resumeFinishReason(events) != "stop" {
		t.Fatalf("the call after the failed one: err=%v finish=%q", err, resumeFinishReason(events))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 2 || runs[1].GetAction().GetResumeAction() == nil {
		t.Fatalf("the call after the failed one sent %v, want a ResumeAction from the checkpoint", runs[len(runs)-1].GetAction())
	}
	if turns := runs[1].GetConversationState().GetTurns(); len(turns) != 1 || string(turns[0]) != "turn-1-step-1" {
		t.Fatalf("it sent state turns %q, want the checkpoint of the lost stream", turns)
	}
}

// A stream lost before any checkpoint leaves nothing upstream. The next pass
// sends the same request again, even though the local transcript now ends
// with the partial answer of the lost pass.
func TestCursorAccountRepeatsRequestWhenStreamLostBeforeCheckpoint(t *testing.T) {
	var runs []*agentv1.AgentRunRequest
	var mu sync.Mutex
	startResumeBackend(t, func(stream int, send func(*agentv1.AgentServerMessage), frames <-chan *agentv1.AgentClientMessage) {
		run := (<-frames).GetRunRequest()
		mu.Lock()
		runs = append(runs, run)
		mu.Unlock()
		if stream == 1 {
			send(resumeText("The answer is"))
			panic(errLoseStream)
		}
		send(resumeText("The answer is 391."))
		send(resumeTurnEnded())
	})
	conversationID, runID := newResumeRun(t)
	// An earlier turn left a checkpoint; this turn's message is not in it.
	cursorAccountRememberState(conversationID, &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("earlier-turn")}})
	question := Message{Role: "user", Content: "what is 17 times 23?"}

	events, err := resumePass(t, conversationID, runID, question)
	if err != nil {
		t.Fatalf("the pass that lost its stream failed: %v", err)
	}
	if got := resumeFinishReason(events); got != FinishReasonCursorStreamInterrupted {
		t.Fatalf("finish reason = %q, want %q", got, FinishReasonCursorStreamInterrupted)
	}
	events, err = resumePass(t, conversationID, runID, question, Message{Role: "assistant", Content: "The answer is"})
	if err != nil {
		t.Fatalf("the repeating pass failed: %v", err)
	}
	if got := resumeFinishReason(events); got != "stop" {
		t.Fatalf("finish reason of the repeating pass = %q, want stop", got)
	}
	mu.Lock()
	defer mu.Unlock()
	first, second := runs[0].GetAction().GetUserMessageAction().GetUserMessage(), runs[1].GetAction().GetUserMessageAction().GetUserMessage()
	if first.GetText() != question.Content {
		t.Fatalf("first run action = %v", runs[0].GetAction())
	}
	if second.GetText() != question.Content || second.GetMessageId() != first.GetMessageId() {
		t.Fatalf("second run sent %v, want the same user message again (text %q, id %q): the saved checkpoint does not hold it", runs[1].GetAction(), first.GetText(), first.GetMessageId())
	}
	if turns := runs[1].GetConversationState().GetTurns(); len(turns) != 1 || string(turns[0]) != "earlier-turn" {
		t.Fatalf("second run sent state turns %q, want the state the lost attempt had sent", turns)
	}
}

// A stream that is lost before anything came of it is a failed call, as
// before: the router makes the call again.
func TestCursorAccountStreamLostBeforeAnyOutputIsAnError(t *testing.T) {
	startResumeBackend(t, func(stream int, send func(*agentv1.AgentServerMessage), frames <-chan *agentv1.AgentClientMessage) {
		<-frames
		panic(errLoseStream)
	})
	conversationID, runID := newResumeRun(t)
	events, err := resumePass(t, conversationID, runID, Message{Role: "user", Content: "hi"})
	if err == nil {
		t.Fatalf("a stream lost before any output ended the pass without an error: events=%v", events)
	}
	if _, pending := cursorAccountTakeRecovery(runID); pending {
		t.Fatal("a stream lost before any output left a recovery behind")
	}
}

// A run that keeps losing its stream before a checkpoint stops resuming: it
// would only repeat the same work.
func TestCursorAccountStopsResumingARunThatMakesNoProgress(t *testing.T) {
	startResumeBackend(t, func(stream int, send func(*agentv1.AgentServerMessage), frames <-chan *agentv1.AgentClientMessage) {
		<-frames
		send(resumeThinking("hm"))
		panic(errLoseStream)
	})
	conversationID, runID := newResumeRun(t)
	question := Message{Role: "user", Content: "hi"}
	for attempt := 1; attempt <= cursorAccountMaxBarrenRecoveries; attempt++ {
		events, err := resumePass(t, conversationID, runID, question)
		if err != nil || resumeFinishReason(events) != FinishReasonCursorStreamInterrupted {
			t.Fatalf("lost stream %d: err=%v finish=%q, want an interrupted pass", attempt, err, resumeFinishReason(events))
		}
	}
	events, err := resumePass(t, conversationID, runID, question)
	if err == nil {
		t.Fatalf("lost stream %d without a checkpoint still ended as %q, want the error", cursorAccountMaxBarrenRecoveries+1, resumeFinishReason(events))
	}
	if !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("error = %v, want the read error of the lost stream", err)
	}
}

func TestCursorAccountStreamRecoveryLimits(t *testing.T) {
	runID := uuid.NewString()
	t.Cleanup(func() { cursorAccountForgetInterruptions(runID) })
	run := &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: &agentv1.AgentRunRequest{}}}
	for lost := 1; lost <= cursorAccountMaxStreamRecoveries; lost++ {
		recoveries, ok := cursorAccountNoteStreamLost(runID, true, run)
		if !ok || recoveries != lost {
			t.Fatalf("lost stream %d with a checkpoint: recoveries=%d resumable=%t", lost, recoveries, ok)
		}
		if _, pending := cursorAccountTakeRecovery(runID); !pending {
			t.Fatalf("lost stream %d left no recovery to take", lost)
		}
		if _, again := cursorAccountTakeRecovery(runID); again {
			t.Fatalf("the recovery of lost stream %d was handed out twice", lost)
		}
	}
	if _, ok := cursorAccountNoteStreamLost(runID, true, run); ok {
		t.Fatalf("a run resumed after %d lost streams, the limit is %d", cursorAccountMaxStreamRecoveries+1, cursorAccountMaxStreamRecoveries)
	}
	if _, ok := cursorAccountNoteStreamLost("", true, run); ok {
		t.Fatal("a call without a run id cannot be resumed by a later pass")
	}
}

// Cancelling a turn also fails the read. That is not a lost stream.
func TestCursorAccountCancelledStreamIsNotResumed(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	startResumeBackend(t, func(stream int, send func(*agentv1.AgentServerMessage), frames <-chan *agentv1.AgentClientMessage) {
		<-frames
		send(resumeThinking("hm"))
		send(resumeCheckpoint("turn-1"))
		close(started)
		<-release
	})
	// Registered after the backend, so it runs before the servers are closed:
	// closing them waits for the stream script to return.
	t.Cleanup(func() { close(release) })
	conversationID, runID := newResumeRun(t)
	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	var finish string
	err := gateway.Stream(ctx, StreamRequest{
		RequestID: runID, RunID: runID, ModelID: "claude-4.5-opus-high", GroupName: "cursor_account2", CursorAccountID: "account-2",
		ConversationID: conversationID, Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(event ModelEvent) error {
		if event.Kind == ModelEventKindTurnFinished {
			finish = event.FinishReason
		}
		return nil
	})
	if err == nil || finish == FinishReasonCursorStreamInterrupted {
		t.Fatalf("a cancelled stream ended with err=%v finish=%q, want the cancellation", err, finish)
	}
	if _, pending := cursorAccountTakeRecovery(runID); pending {
		t.Fatal("a cancelled stream left a recovery behind")
	}
}

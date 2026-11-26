package forwarder

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"cursor/gen/agentv1"
	modeladapter "cursor/internal/backend/agent/model"
)

const lostStreamWaitID = "toolu_01WaitOpenAtTheDrop"

// lostStreamProvider plays a turn whose first pass loses its stream to Cursor
// while a wait is open. The second pass is the resumed turn: Cursor runs the
// wait again and reports its result, then the model answers.
type lostStreamProvider struct {
	mu     sync.Mutex
	passes int
	done   chan struct{}
}

func (provider *lostStreamProvider) StartStream(_ context.Context, _ ProviderRequest, sink func(modeladapter.ModelEvent) error) error {
	provider.mu.Lock()
	provider.passes++
	pass := provider.passes
	provider.mu.Unlock()
	if pass == 1 {
		for _, event := range []modeladapter.ModelEvent{
			{Kind: modeladapter.ModelEventKindTextDelta, Text: "Waiting for the build."},
			{
				Kind:       modeladapter.ModelEventKindCursorAwaitStarted,
				ToolCallID: lostStreamWaitID,
				CursorAwaitStarted: &agentv1.ToolCallStartedUpdate{
					CallId:   lostStreamWaitID,
					ToolCall: awaitTestCard(lostStreamWaitID, 600000, nil),
				},
			},
			{Kind: modeladapter.ModelEventKindTurnFinished, FinishReason: modeladapter.FinishReasonCursorStreamInterrupted},
		} {
			if err := sink(event); err != nil {
				return err
			}
		}
		return nil
	}
	exitCode := int32(0)
	for _, event := range []modeladapter.ModelEvent{
		{
			Kind:       modeladapter.ModelEventKindCursorAwaitStarted,
			ToolCallID: lostStreamWaitID,
			CursorAwaitStarted: &agentv1.ToolCallStartedUpdate{
				CallId:   lostStreamWaitID,
				ToolCall: awaitTestCard(lostStreamWaitID, 600000, nil),
			},
		},
		{
			Kind:       modeladapter.ModelEventKindCursorAwaitCompleted,
			ToolCallID: lostStreamWaitID,
			CursorAwaitCompleted: &agentv1.ToolCallCompletedUpdate{
				CallId: lostStreamWaitID,
				ToolCall: awaitTestCard(lostStreamWaitID, 600000, &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Success{Success: &agentv1.AwaitSuccess{
					AwaitResult: &agentv1.AwaitSuccess_Complete{Complete: &agentv1.AwaitTaskComplete{TaskId: "423355", RuntimeMs: 412000, ExitCode: &exitCode}},
				}}}),
			},
		},
		{Kind: modeladapter.ModelEventKindTextDelta, Text: "BUILD-FINISHED"},
		{Kind: modeladapter.ModelEventKindTurnFinished, FinishReason: "stop"},
	} {
		if err := sink(event); err != nil {
			return err
		}
	}
	close(provider.done)
	return nil
}

// What arrived before the stream was lost is a fragment of an answer. The
// local response cache must not keep it as the answer to that request.
func TestPassThatLostItsCursorStreamIsNotCachedAsAnAnswer(t *testing.T) {
	record := func(finishReason string) *streamRecorder {
		recorder := &streamRecorder{}
		recorder.record(modeladapter.ModelEvent{Kind: modeladapter.ModelEventKindTextDelta, Text: "The answer is"})
		recorder.record(modeladapter.ModelEvent{Kind: modeladapter.ModelEventKindTurnFinished, FinishReason: finishReason})
		return recorder
	}
	if record(modeladapter.FinishReasonCursorStreamInterrupted).completedCleanly() {
		t.Fatal("a pass that lost its stream counts as a complete answer for the response cache")
	}
	if !record("stop").completedCleanly() {
		t.Fatal("a pass that ended normally no longer counts as a complete answer")
	}
}

// lostWhileThinkingProvider loses its first stream while the model is still
// thinking: no tool call, no text. The second pass is the resumed turn.
type lostWhileThinkingProvider struct {
	mu     sync.Mutex
	passes int
	done   chan struct{}
}

func (provider *lostWhileThinkingProvider) StartStream(_ context.Context, _ ProviderRequest, sink func(modeladapter.ModelEvent) error) error {
	provider.mu.Lock()
	provider.passes++
	pass := provider.passes
	provider.mu.Unlock()
	events := []modeladapter.ModelEvent{
		{Kind: modeladapter.ModelEventKindThinkingDelta, Text: "The user wants"},
		{Kind: modeladapter.ModelEventKindThinkingCompleted},
		{Kind: modeladapter.ModelEventKindTurnFinished, FinishReason: modeladapter.FinishReasonCursorStreamInterrupted},
	}
	if pass > 1 {
		events = []modeladapter.ModelEvent{
			{Kind: modeladapter.ModelEventKindTextDelta, Text: "ANSWERED-AFTER-RESUME"},
			{Kind: modeladapter.ModelEventKindTurnFinished, FinishReason: "stop"},
		}
	}
	for _, event := range events {
		if err := sink(event); err != nil {
			return err
		}
	}
	if pass > 1 {
		close(provider.done)
	}
	return nil
}

// A stream lost before the model called any tool or wrote anything is the
// logged failure of 01:16: the turn must still go on, not end as if the model
// had nothing to say.
func TestTurnContinuesAfterItsCursorStreamIsLostWhileThinking(t *testing.T) {
	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation([]HistoryEntry{
		testUserMessageEntry(t, 1, "request-lost-thinking", "explain the failure"),
	})
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatal(err)
	}
	provider := &lostWhileThinkingProvider{done: make(chan struct{})}
	broker := NewStreamBroker()
	service := newServiceWithDependencies(store, NewHistoryProjector(), contextProjectionLifecycleCompiler{}, provider, broker)
	stream, err := broker.OpenStream("request-lost-thinking", persisted.ConversationID, 1, "model-a", "model-a", agentv1.AgentMode_AGENT_MODE_AGENT, "explain")
	if err != nil {
		t.Fatal(err)
	}
	stream.CheckpointConversation = cloneConversationFile(persisted)
	t.Cleanup(func() {
		_ = broker.Cancel(stream.RequestID, "test cleanup")
		stream.mu.Lock()
		done := stream.ActorDone
		stream.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-time.After(time.Second):
			}
		}
	})
	if err := service.requestProviderAction(stream, providerActionStart); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.done:
	case <-time.After(5 * time.Second):
		stream.mu.Lock()
		status, passes := stream.Status, stream.ProviderPassCount
		stream.mu.Unlock()
		t.Fatalf("the turn did not go on after its stream was lost while thinking: status=%s provider passes=%d", status, passes)
	}
}

// Cursor's server closed the connection in the middle of a turn (the logged
// "http2: server sent GOAWAY and closed the connection"). The turn used to
// fail there. It now goes on in another pass, and the wait that was open is
// neither closed as failed nor shown twice.
func TestTurnContinuesAfterItsCursorStreamIsLost(t *testing.T) {
	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation([]HistoryEntry{
		testUserMessageEntry(t, 1, "request-lost-stream", "build it and tell me when it is done"),
	})
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatal(err)
	}
	provider := &lostStreamProvider{done: make(chan struct{})}
	broker := NewStreamBroker()
	service := newServiceWithDependencies(store, NewHistoryProjector(), contextProjectionLifecycleCompiler{}, provider, broker)
	stream, err := broker.OpenStream("request-lost-stream", persisted.ConversationID, 1, "model-a", "model-a", agentv1.AgentMode_AGENT_MODE_AGENT, "build it")
	if err != nil {
		t.Fatal(err)
	}
	stream.CheckpointConversation = cloneConversationFile(persisted)
	t.Cleanup(func() {
		_ = broker.Cancel(stream.RequestID, "test cleanup")
		stream.mu.Lock()
		done := stream.ActorDone
		stream.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-time.After(time.Second):
			}
		}
	})

	if err := service.requestProviderAction(stream, providerActionStart); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.done:
	case <-time.After(5 * time.Second):
		stream.mu.Lock()
		status, passes := stream.Status, stream.ProviderPassCount
		stream.mu.Unlock()
		t.Fatalf("the turn did not go on after its stream was lost: status=%s provider passes=%d", status, passes)
	}

	var texts []string
	var waitCalls int
	var waitResults []awaitShellResult
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		texts, waitCalls, waitResults = nil, 0, nil
		for _, entry := range snapshotHistoryEntries(stream) {
			switch {
			case entry.Kind == "assistant_text":
				var payload assistantTextPayload
				if err := json.Unmarshal(entry.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				texts = append(texts, payload.Text)
			case entry.Kind == "tool_call" && historyEntryToolCallID(entry) == lostStreamWaitID:
				waitCalls++
			case entry.Kind == "tool_result" && historyEntryToolCallID(entry) == lostStreamWaitID:
				var payload toolResultEntryPayload
				if err := json.Unmarshal(entry.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				var summary awaitShellResult
				if err := json.Unmarshal([]byte(payload.ResultText), &summary); err != nil {
					t.Fatalf("wait result is not the AwaitShell result shape: %v\n%s", err, payload.ResultText)
				}
				waitResults = append(waitResults, summary)
			}
		}
		if len(texts) == 2 && len(waitResults) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(texts) != 2 || texts[0] != "Waiting for the build." || texts[1] != "BUILD-FINISHED" {
		t.Fatalf("assistant texts in history = %q, want what was said before the drop and the answer after the resume", texts)
	}
	if waitCalls != 1 {
		t.Fatalf("the wait is in history %d times, want once", waitCalls)
	}
	if len(waitResults) != 1 {
		t.Fatalf("the wait has %d results, want the one Cursor reported after the resume", len(waitResults))
	}
	if got := waitResults[0]; got.Status != backgroundShellStatusCompleted || got.RuntimeMS != 412000 {
		t.Fatalf("the wait's result is %+v, want Cursor's completed result: the wait was closed when the stream was lost", got)
	}
	stream.mu.Lock()
	status := stream.Status
	stream.mu.Unlock()
	if status == StreamStatusFailed || status == StreamStatusCanceled {
		t.Fatalf("stream status = %s after the resumed turn", status)
	}
}

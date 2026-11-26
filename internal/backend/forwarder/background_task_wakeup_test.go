package forwarder

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"cursor/gen/agentv1"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/proto"
)

const (
	wakeupTaskToolCallID = "toolu_01TaskSentToBackground"
	wakeupSubagentID     = "3f2a9c1e-5b7d-4e68-9a10-c4d2e6f8a0b1"
)

func wakeupSubagentFinished() *agentv1.BackgroundTaskCompletionAction {
	return &agentv1.BackgroundTaskCompletionAction{Completions: []*agentv1.BackgroundTaskCompletion{{
		TaskId:     wakeupSubagentID,
		Kind:       agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SUBAGENT,
		Status:     agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS,
		Title:      "Audit the forwarder tests",
		Detail:     proto.String("All 212 tests pass; two are skipped on Windows."),
		OutputPath: proto.String(`C:\Users\dev\.cursor\projects\repo\agent-transcripts\3f2a9c1e.jsonl`),
		Reason:     agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED,
		SubagentId: proto.String(wakeupSubagentID),
		ToolCallId: proto.String(wakeupTaskToolCallID),
	}}}
}

// The notice is what the model reads in place of a user message. It is
// written the way Cursor's server writes it (captured from the live server
// for a finished shell), so a model sees the same thing on either path.
func TestBackgroundTaskNoticeIsWrittenAsCursorWritesIt(t *testing.T) {
	got := backgroundTaskNoticeText(&agentv1.BackgroundTaskCompletionAction{Completions: []*agentv1.BackgroundTaskCompletion{{
		TaskId:     "424242",
		Kind:       agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SHELL,
		Status:     agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS,
		Title:      "ping -n 30 127.0.0.1",
		Detail:     proto.String("exit code 0"),
		OutputPath: proto.String(`C:\Users\dev\.cursor\projects\wakeup-check/terminals/424242.txt`),
		Reason:     agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED,
	}}})
	want := "<system_notification>\n" +
		"The following task has finished. If you were already aware, ignore this notification and do not restate prior responses.\n" +
		"\n" +
		"<task>\n" +
		"kind: shell\n" +
		"status: success\n" +
		"task_id: 424242\n" +
		"title: ping -n 30 127.0.0.1\n" +
		"detail: exit code 0\n" +
		"output_path: C:\\Users\\dev\\.cursor\\projects\\wakeup-check/terminals/424242.txt\n" +
		"</task>\n" +
		"</system_notification>\n" +
		"<user_query>Briefly inform the user about the task result and perform any follow-up actions (if needed).</user_query>"
	if got != want {
		t.Fatalf("notice:\n%s\n\nwant:\n%s", got, want)
	}
	if text := backgroundTaskNoticeText(&agentv1.BackgroundTaskCompletionAction{Completions: []*agentv1.BackgroundTaskCompletion{{TaskId: "1", RecordOnly: true}}}); text != "" {
		t.Fatalf("a record-only entry produced a notice: %q", text)
	}
}

type wakeupFixture struct {
	service  *Service
	provider *contextProjectionRequestProvider
	convID   string
}

func newWakeupFixture(t *testing.T, cursorAccount bool) wakeupFixture {
	t.Helper()
	home, err := os.MkdirTemp("", "cursor-byok-wakeup-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("CURSOR_BYOK_HOME", home)
	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation(nil)
	appendEntriesInPlace(conversation, []HistoryEntry{
		testUserMessageEntry(t, 1, "request-1", "audit the tests in the background"),
		newAssistantTextEntry(1, "request-1", "The audit is running in the background.", "", ""),
	})
	conversation.CurrentTurnSeq = 1
	conversation.NextTurnSeq = 2
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatal(err)
	}
	provider := &contextProjectionRequestProvider{requests: make(chan ProviderRequest, 4)}
	service := newServiceWithDependencies(store, NewHistoryProjector(), contextProjectionLifecycleCompiler{}, provider, NewStreamBroker())
	if cursorAccount {
		service.resolver = cursorAccountSavedStateResolver{}
		t.Cleanup(func() { modeladapter.ClearCursorAccountStateAndSnapshots(persisted.ConversationID) })
	}
	return wakeupFixture{service: service, provider: provider, convID: persisted.ConversationID}
}

// run starts a run the way the IDE does and returns the request of its first
// provider pass and its stream.
func (fixture wakeupFixture) run(t *testing.T, requestID string, action *agentv1.ConversationAction, userMessage *agentv1.UserMessage) (ProviderRequest, *ActiveStream) {
	t.Helper()
	message := &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: &agentv1.AgentRunRequest{
		ConversationId: proto.String(fixture.convID),
		Action:         action,
	}}}
	intent := InboundIntent{
		Kind:                 "run",
		StartsRun:            true,
		RequestID:            requestID,
		ConversationID:       fixture.convID,
		ModelID:              "cursor_account1:claude-fable-5-2",
		ModelName:            "Claude Fable 5.2",
		Mode:                 agentv1.AgentMode_AGENT_MODE_AGENT,
		UserMessage:          userMessage,
		BackgroundCompletion: inboundBackgroundCompletion(message.GetRunRequest()),
		ClientMessage:        message,
	}
	if err := fixture.service.handleRunIntent(intent); err != nil {
		t.Fatalf("handleRunIntent: %v", err)
	}
	stream, ok := fixture.service.broker.Get(requestID)
	if !ok || stream == nil {
		t.Fatal("the run has no stream")
	}
	t.Cleanup(func() {
		_ = fixture.service.broker.Cancel(requestID, "test cleanup")
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
	select {
	case request := <-fixture.provider.requests:
		return request, stream
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not reach the provider")
		return ProviderRequest{}, nil
	}
}

func wakeupNoticesSentToClient(stream *ActiveStream) []*agentv1.UserMessage {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	var notices []*agentv1.UserMessage
	for _, event := range stream.Backlog {
		if appended := event.Message.GetInteractionUpdate().GetUserMessageAppended(); appended != nil {
			notices = append(notices, appended.GetUserMessage())
		}
	}
	return notices
}

func wakeupLastUserMessage(request ProviderRequest) string {
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == "user" {
			return request.Messages[i].Content
		}
	}
	return ""
}

// A background subagent finished and the IDE started a run of the parent
// conversation to say so. On a Cursor account that news is the run's action,
// sent to Cursor's server as it is. It used to go out as a plain resume: the
// model was started and told nothing.
func TestRunStartedByFinishedBackgroundTaskCarriesTheNewsToCursor(t *testing.T) {
	fixture := newWakeupFixture(t, true)
	finished := wakeupSubagentFinished()
	request, stream := fixture.run(t, "request-wakeup", &agentv1.ConversationAction{
		Action: &agentv1.ConversationAction_BackgroundTaskCompletionAction{BackgroundTaskCompletionAction: finished},
	}, nil)

	relayed := modeladapter.CursorBackgroundTaskCompletions(request.CursorConversationAction)
	if len(relayed) != 1 || !proto.Equal(relayed[0], finished.GetCompletions()[0]) {
		t.Fatalf("the first pass carries action %v, want the finished task the IDE reported", request.CursorConversationAction)
	}
	// The notice is also in this process's own history, for the day the
	// conversation goes on without Cursor's checkpoint.
	if notice := wakeupLastUserMessage(request); !strings.Contains(notice, "task_id: "+wakeupSubagentID) || !strings.Contains(notice, "All 212 tests pass") {
		t.Fatalf("the transcript does not end with the notice of the finished task: %q", notice)
	}
	// Cursor's server writes the notice the IDE shows; a second one from
	// here would show it twice.
	if notices := wakeupNoticesSentToClient(stream); len(notices) != 0 {
		t.Fatalf("%d notices were sent to the IDE before Cursor's own", len(notices))
	}
	// The Task call that started the subagent was BACKGROUNDED while it ran.
	stream.mu.Lock()
	run := stream.DelegationRunTerminals[wakeupTaskToolCallID]
	stream.mu.Unlock()
	if run.GetStatus() != agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_SUCCESS {
		t.Fatalf("the finished subagent's run is %v, want SUCCESS", run.GetStatus())
	}

	// The notice Cursor's server then writes is passed on unchanged.
	fromCursor := &agentv1.UserMessage{
		Text:               "<system_notification>…</system_notification>",
		MessageId:          "7c1d4e2f-8a9b-4c0d-b1e2-f3a4b5c6d7e8",
		IsSimulatedMsg:     proto.Bool(true),
		SimulatedMsgReason: agentv1.SimulatedMsgReason_SIMULATED_MSG_REASON_BACKGROUND_TASK_COMPLETION.Enum(),
		SimulatedMessageMetadata: &agentv1.UserMessage_SimulatedMessageMetadata{
			Title: proto.String("Audit the forwarder tests"), TaskId: proto.String(wakeupSubagentID),
		},
	}
	if err := fixture.service.applyCursorUserMessageAppended(stream, fromCursor); err != nil {
		t.Fatal(err)
	}
	if notices := wakeupNoticesSentToClient(stream); len(notices) != 1 || !proto.Equal(notices[0], fromCursor) {
		t.Fatalf("notices sent to the IDE = %v, want Cursor's own, unchanged", notices)
	}
}

// For a model that is not a Cursor account model nobody else writes the
// notice: the model reads it from the transcript and the IDE is sent it.
func TestRunStartedByFinishedBackgroundTaskTellsOtherModelsItself(t *testing.T) {
	fixture := newWakeupFixture(t, false)
	request, stream := fixture.run(t, "request-wakeup-byok", &agentv1.ConversationAction{
		Action: &agentv1.ConversationAction_BackgroundTaskCompletionAction{BackgroundTaskCompletionAction: wakeupSubagentFinished()},
	}, nil)

	if request.CursorConversationAction != nil {
		t.Fatalf("a call to another provider carries Cursor's action: %v", request.CursorConversationAction)
	}
	notice := wakeupLastUserMessage(request)
	for _, want := range []string{"<system_notification>", "The following task has finished.", "kind: subagent", "status: success", "task_id: " + wakeupSubagentID, "All 212 tests pass", "<user_query>"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("the model is not told %q; the transcript ends with:\n%s", want, notice)
		}
	}
	notices := wakeupNoticesSentToClient(stream)
	if len(notices) != 1 {
		t.Fatalf("%d notices sent to the IDE, want one", len(notices))
	}
	shown := notices[0]
	if !shown.GetIsSimulatedMsg() || shown.GetSimulatedMsgReason() != agentv1.SimulatedMsgReason_SIMULATED_MSG_REASON_BACKGROUND_TASK_COMPLETION {
		t.Fatalf("the notice is not marked as a background task completion: simulated=%t reason=%v", shown.GetIsSimulatedMsg(), shown.GetSimulatedMsgReason())
	}
	if shown.GetText() != notice || shown.GetSimulatedMessageMetadata().GetTaskId() != wakeupSubagentID || shown.GetSimulatedMessageMetadata().GetTitle() != "Audit the forwarder tests" || shown.GetMessageId() == "" {
		t.Fatalf("the notice shown is %v, want the text the model reads and the task it is about", shown)
	}

	var recorded int
	for _, entry := range snapshotHistoryEntries(stream) {
		if entry.Kind != "metadata" {
			continue
		}
		var payload metadataPayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Type == "background_task_completion_action" {
			recorded++
		}
	}
	if recorded != 1 {
		t.Fatalf("%d records of the finished task in history, want 1", recorded)
	}
}

// An ordinary run is not touched.
func TestRunStartedByTheUserCarriesNoBackgroundTaskNews(t *testing.T) {
	fixture := newWakeupFixture(t, true)
	userMessage := &agentv1.UserMessage{Text: "and now?", MessageId: "msg-2", Mode: agentv1.AgentMode_AGENT_MODE_AGENT}
	request, stream := fixture.run(t, "request-user", &agentv1.ConversationAction{
		Action: &agentv1.ConversationAction_UserMessageAction{UserMessageAction: &agentv1.UserMessageAction{UserMessage: userMessage}},
	}, userMessage)
	if request.CursorConversationAction != nil {
		t.Fatalf("an ordinary run carries a relayed action: %v", request.CursorConversationAction)
	}
	if last := wakeupLastUserMessage(request); strings.Contains(last, "<system_notification>") || !strings.Contains(last, "and now?") {
		t.Fatalf("the transcript of an ordinary run ends with %q", last)
	}
	if notices := wakeupNoticesSentToClient(stream); len(notices) != 0 {
		t.Fatalf("an ordinary run sent %d notices to the IDE", len(notices))
	}
}

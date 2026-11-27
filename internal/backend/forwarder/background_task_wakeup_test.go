package forwarder

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"cursor/gen/agentv1"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	wakeupTaskToolCallID = "toolu_01TaskSentToBackground"
	wakeupSubagentID     = "3f2a9c1e-5b7d-4e68-9a10-c4d2e6f8a0b1"
	wakeupWorkspace      = `C:\work\repo`
	wakeupTerminals      = `C:\Users\dev\.cursor\projects\repo\terminals`
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

// The notices are what the model reads in place of a user message. They are
// written the way Cursor's server writes them, so a model sees the same thing
// on either path. The expected texts are what the live server wrote for the
// same reports (cursor_account_task_notice_live_test.go), without the
// timestamp line it puts before every user message and without the part of
// the subagent instruction that is about cloud agents.
func TestBackgroundTaskNoticesAreWrittenAsCursorWritesThem(t *testing.T) {
	const (
		opening = "<system_notification>\n" +
			"The following task has finished. If you were already aware, ignore this notification and do not restate prior responses.\n" +
			"\n"
		shellTask = "<task>\n" +
			"kind: shell\n" +
			"status: success\n" +
			"task_id: 424242\n" +
			"title: ping -n 30 127.0.0.1\n" +
			"tool_call_id: toolu_01ShellLeftRunning\n" +
			"output_path: C:\\Users\\dev\\.cursor\\projects\\wakeup-check/terminals/424242.txt\n" +
			"</task>\n" +
			"</system_notification>"
		subagentTask = "<task>\n" +
			"kind: subagent\n" +
			"status: success\n" +
			"task_id: 3f2a9c1e-5b7d-4e68-9a10-c4d2e6f8a0b1\n" +
			"title: Pong check\n" +
			"tool_call_id: toolu_01TaskSentToBackground\n" +
			"agent_id: 3f2a9c1e-5b7d-4e68-9a10-c4d2e6f8a0b1\n" +
			"detail: pong\n" +
			"output_path: C:\\Users\\dev\\.cursor\\projects\\wakeup-check\\agent-transcripts\\3f2a9c1e-5b7d-4e68-9a10-c4d2e6f8a0b1.jsonl\n" +
			"</task>\n" +
			"</system_notification>"
		shellQuery    = "\n<user_query>Briefly inform the user about the task result and perform any follow-up actions (if needed).</user_query>"
		subagentQuery = "\n<user_query>Perform any necessary follow-up actions in response to the subagent completion above. If no follow-up work is needed, no further action is required. If you mention an agent or subagent in your response, link it with the `[Name](id)` Don't use generic label such as `[agent]`, `[worker]`, or `[subagent]`. Don't repeat the same confirmation every time.</user_query>"
	)
	shell := &agentv1.BackgroundTaskCompletion{
		TaskId:     "424242",
		Kind:       agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SHELL,
		Status:     agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS,
		Title:      "ping -n 30 127.0.0.1",
		OutputPath: proto.String(`C:\Users\dev\.cursor\projects\wakeup-check/terminals/424242.txt`),
		Reason:     agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED,
		ToolCallId: proto.String("toolu_01ShellLeftRunning"),
	}
	subagent := &agentv1.BackgroundTaskCompletion{
		TaskId:     wakeupSubagentID,
		Kind:       agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SUBAGENT,
		Status:     agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS,
		Title:      "Pong check",
		Detail:     proto.String("pong"),
		OutputPath: proto.String(`C:\Users\dev\.cursor\projects\wakeup-check\agent-transcripts\` + wakeupSubagentID + `.jsonl`),
		Reason:     agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED,
		SubagentId: proto.String(wakeupSubagentID),
		ToolCallId: proto.String(wakeupTaskToolCallID),
	}
	recordOnly := &agentv1.BackgroundTaskCompletion{TaskId: "1", RecordOnly: true}

	for _, tc := range []struct {
		name     string
		reported []*agentv1.BackgroundTaskCompletion
		want     []backgroundTaskNotice
	}{
		{"a shell", []*agentv1.BackgroundTaskCompletion{shell}, []backgroundTaskNotice{
			{Text: opening + shellTask + shellQuery, Title: "ping -n 30 127.0.0.1", TaskID: "424242"},
		}},
		{"a subagent", []*agentv1.BackgroundTaskCompletion{subagent}, []backgroundTaskNotice{
			{Text: opening + subagentTask + subagentQuery, Title: "Pong check", TaskID: wakeupSubagentID},
		}},
		// One notice per task, in the order reported; only the last one
		// says what to do, and says it for its own kind of task.
		{"a shell and a subagent in one run", []*agentv1.BackgroundTaskCompletion{shell, recordOnly, subagent}, []backgroundTaskNotice{
			{Text: opening + shellTask, Title: "ping -n 30 127.0.0.1", TaskID: "424242"},
			{Text: opening + subagentTask + subagentQuery, Title: "Pong check", TaskID: wakeupSubagentID},
		}},
		{"a record-only entry", []*agentv1.BackgroundTaskCompletion{recordOnly}, nil},
	} {
		got := backgroundTaskNotices(&agentv1.BackgroundTaskCompletionAction{Completions: tc.reported})
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %d notices, want %d: %v", tc.name, len(got), len(tc.want), got)
		}
		for index := range got {
			if got[index] != tc.want[index] {
				t.Fatalf("%s: notice %d:\n%+v\n\nwant:\n%+v", tc.name, index+1, got[index], tc.want[index])
			}
		}
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
	// The first run of a conversation names its workspace; later ones are
	// stored without it.
	startedIn, err := protojson.Marshal(&agentv1.RequestContext{Env: &agentv1.RequestContextEnv{
		OsVersion: "win32 10.0.26200", Shell: "powershell",
		WorkspacePaths: []string{wakeupWorkspace}, ProjectFolder: wakeupWorkspace, TerminalsFolder: wakeupTerminals,
	}})
	if err != nil {
		t.Fatal(err)
	}
	appendEntriesInPlace(conversation, []HistoryEntry{
		{TurnSeq: 1, RequestID: "request-1", Role: "user", Kind: "request_context", Payload: startedIn},
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

// Work that finished while the conversation was busy is reported by one run.
// The IDE is sent a notice for each task, in the order reported: it pairs
// the notices with the tasks it reported one by one. The model reads the
// same notices, in the same order, as the one message of this turn.
func TestRunStartedByTwoFinishedTasksReportsEach(t *testing.T) {
	fixture := newWakeupFixture(t, false)
	both := &agentv1.BackgroundTaskCompletionAction{Completions: []*agentv1.BackgroundTaskCompletion{
		wakeupShellFailed().GetCompletions()[0],
		wakeupSubagentFinished().GetCompletions()[0],
	}}
	request, stream := fixture.run(t, "request-wakeup-two", &agentv1.ConversationAction{
		Action: &agentv1.ConversationAction_BackgroundTaskCompletionAction{BackgroundTaskCompletionAction: both},
	}, nil)

	shown := wakeupNoticesSentToClient(stream)
	if len(shown) != 2 {
		t.Fatalf("%d notices sent to the IDE, want one per task", len(shown))
	}
	for index, want := range []struct{ taskID, title string }{{"731245", "npm run build"}, {wakeupSubagentID, "Audit the forwarder tests"}} {
		notice := shown[index]
		if !strings.Contains(notice.GetText(), "task_id: "+want.taskID) || notice.GetSimulatedMessageMetadata().GetTaskId() != want.taskID || notice.GetSimulatedMessageMetadata().GetTitle() != want.title {
			t.Fatalf("notice %d sent to the IDE is about task=%q title=%q, want task=%q title=%q:\n%s",
				index+1, notice.GetSimulatedMessageMetadata().GetTaskId(), notice.GetSimulatedMessageMetadata().GetTitle(), want.taskID, want.title, notice.GetText())
		}
		if !notice.GetIsSimulatedMsg() || notice.GetSimulatedMsgReason() != agentv1.SimulatedMsgReason_SIMULATED_MSG_REASON_BACKGROUND_TASK_COMPLETION {
			t.Fatalf("notice %d is not marked as a background task completion", index+1)
		}
	}
	if shown[0].GetMessageId() == "" || shown[0].GetMessageId() == shown[1].GetMessageId() {
		t.Fatalf("the notices do not have ids of their own: %q and %q", shown[0].GetMessageId(), shown[1].GetMessageId())
	}
	if strings.Contains(shown[0].GetText(), "<user_query>") || !strings.Contains(shown[1].GetText(), "<user_query>Perform any necessary follow-up actions in response to the subagent completion above.") {
		t.Fatalf("only the last notice says what to do, for its kind of task:\n%s\n---\n%s", shown[0].GetText(), shown[1].GetText())
	}

	// One message in the transcript: a Cursor account pass would send an
	// earlier one to Cursor's server as history, next to the server's own.
	var read []string
	for _, message := range request.Messages {
		if message.Role == "user" && strings.Contains(message.Content, "<system_notification>") {
			read = append(read, message.Content)
		}
	}
	if len(read) != 1 || read[0] != shown[0].GetText()+"\n"+shown[1].GetText() {
		t.Fatalf("the model reads %d messages about the finished tasks, want one made of the two notices the IDE is sent:\n%s", len(read), strings.Join(read, "\n---\n"))
	}
	if last := wakeupLastUserMessage(request); last != read[0] {
		t.Fatalf("the notices are not the last message of the transcript: %q", last)
	}

	// The subagent's Task card is settled; the shell has none.
	stream.mu.Lock()
	settled := len(stream.DelegationRunTerminals)
	run := stream.DelegationRunTerminals[wakeupTaskToolCallID]
	stream.mu.Unlock()
	if settled != 1 || run.GetStatus() != agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_SUCCESS {
		t.Fatalf("%d Task cards settled, the subagent's as %v; want its card alone, as SUCCESS", settled, run.GetStatus())
	}
}

// wakeupShellFailed is what the client's exec extension reports for a
// background command that ended with a non-zero exit code.
func wakeupShellFailed() *agentv1.BackgroundTaskCompletionAction {
	return &agentv1.BackgroundTaskCompletionAction{Completions: []*agentv1.BackgroundTaskCompletion{{
		TaskId:     "731245",
		Kind:       agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SHELL,
		Status:     agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_ERROR,
		Title:      "npm run build",
		Detail:     proto.String("exit_code=1"),
		OutputPath: proto.String(`C:\Users\dev\.cursor\projects\repo\terminals\731245.txt`),
		Reason:     agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED,
		ToolCallId: proto.String("toolu_01ShellLeftRunning"),
	}}}
}

// wakeupRunRequest is the request the client sends to start such a run: the
// conversation, its state, the model, and the finished tasks as the action.
// It carries no user message and no request context.
func wakeupRunRequest(conversationID string, finished *agentv1.BackgroundTaskCompletionAction) *agentv1.AgentClientMessage {
	return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: &agentv1.AgentRunRequest{
		ConversationId:    proto.String(conversationID),
		ConversationState: &agentv1.ConversationStateStructure{Mode: agentv1.AgentMode_AGENT_MODE_AGENT.Enum()},
		Action: &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_BackgroundTaskCompletionAction{BackgroundTaskCompletionAction: finished},
		},
		ModelDetails: &agentv1.ModelDetails{ModelId: "byok-channel-1", DisplayName: "Some Model"},
	}}}
}

// The tests above hand the run to the service ready-made. A run from the
// client arrives as an append and is decoded first. It has no user message,
// like a resume, and a resume of a conversation whose last turn is over is
// dropped; a run that reports finished tasks must not be.
func TestWakeupRunRequestFromTheClientIsDecodedAsARun(t *testing.T) {
	fixture := newWakeupFixture(t, false)
	finished := wakeupShellFailed()

	intent, err := fixture.service.decodeInboundIntent("request-wakeup-wire", wakeupRunRequest(fixture.convID, finished), "run_request")
	if err != nil {
		t.Fatalf("decodeInboundIntent: %v", err)
	}
	if intent.Kind != "run" || !intent.StartsRun || intent.IgnoredReason != "" {
		t.Fatalf("the run is decoded as kind=%q starts_run=%t ignored=%q, want a run that starts", intent.Kind, intent.StartsRun, intent.IgnoredReason)
	}
	if intent.UserMessage != nil {
		t.Fatalf("the run carries a user message: %v", intent.UserMessage)
	}
	if reported := intent.BackgroundCompletion.GetCompletions(); len(reported) != 1 || !proto.Equal(reported[0], finished.GetCompletions()[0]) {
		t.Fatalf("the run reports %v, want the finished shell", reported)
	}
	if intent.ConversationID != fixture.convID || intent.ModelID != "byok-channel-1" || intent.Mode != agentv1.AgentMode_AGENT_MODE_AGENT {
		t.Fatalf("the run is for conversation=%q model=%q mode=%v", intent.ConversationID, intent.ModelID, intent.Mode)
	}

	// The same request with nothing to report is not a run about tasks.
	nothing := &agentv1.BackgroundTaskCompletionAction{Completions: []*agentv1.BackgroundTaskCompletion{{TaskId: "731245", RecordOnly: true}}}
	intent, err = fixture.service.decodeInboundIntent("request-wakeup-empty", wakeupRunRequest(fixture.convID, nothing), "run_request")
	if err != nil {
		t.Fatalf("decodeInboundIntent: %v", err)
	}
	if intent.BackgroundCompletion != nil {
		t.Fatalf("a record-only report is treated as news: %v", intent.BackgroundCompletion)
	}
}

// From the append to the model: the decoded run goes through the
// conversation's run queue and its stream like a run the user started, the
// model is told what finished, and the IDE is shown the notice. A finished
// shell is not a subagent: no Task card is settled for it.
func TestWakeupRunRequestFromTheClientReachesTheModel(t *testing.T) {
	fixture := newWakeupFixture(t, false)
	const requestID = "request-wakeup-dispatched"

	intent, err := fixture.service.decodeInboundIntent(requestID, wakeupRunRequest(fixture.convID, wakeupShellFailed()), "run_request")
	if err != nil {
		t.Fatalf("decodeInboundIntent: %v", err)
	}
	if err := fixture.service.dispatchInboundIntent(intent); err != nil {
		t.Fatalf("dispatchInboundIntent: %v", err)
	}
	t.Cleanup(func() {
		_ = fixture.service.broker.Cancel(requestID, "test cleanup")
		if stream, ok := fixture.service.broker.Get(requestID); ok && stream != nil {
			stream.mu.Lock()
			done := stream.ActorDone
			stream.mu.Unlock()
			if done != nil {
				select {
				case <-done:
				case <-time.After(time.Second):
				}
			}
		}
	})

	var request ProviderRequest
	select {
	case request = <-fixture.provider.requests:
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not reach the provider")
	}
	// The run states its mode, as every run from the client does, so a mode
	// note may follow the notice in the transcript.
	notice := ""
	for _, message := range request.Messages {
		if message.Role == "user" && strings.Contains(message.Content, "<system_notification>") {
			notice = message.Content
		}
	}
	for _, want := range []string{"The following task has finished.", "kind: shell", "status: error", "task_id: 731245", "title: npm run build", "detail: exit_code=1", `output_path: C:\Users\dev\.cursor\projects\repo\terminals\731245.txt`} {
		if !strings.Contains(notice, want) {
			t.Fatalf("the model is not told %q; the transcript ends with:\n%s", want, notice)
		}
	}

	stream, ok := fixture.service.broker.Get(requestID)
	if !ok || stream == nil {
		t.Fatal("the run has no stream")
	}
	notices := wakeupNoticesSentToClient(stream)
	if len(notices) != 1 || notices[0].GetText() != notice || notices[0].GetSimulatedMessageMetadata().GetTaskId() != "731245" {
		t.Fatalf("notices sent to the IDE = %v, want the one the model reads", notices)
	}
	stream.mu.Lock()
	settled := len(stream.DelegationRunTerminals)
	stream.mu.Unlock()
	if settled != 0 {
		t.Fatalf("a finished shell settled %d Task cards", settled)
	}
}

// The run the client starts for finished tasks names no workspace, and it
// has a request id, so a stream, of its own. It works in the workspace and
// with the terminals folder the conversation was started with: without them
// a wait cannot find the terminal file of the very shell the run is about.
func TestRunStartedByFinishedBackgroundTaskWorksInItsConversationsWorkspace(t *testing.T) {
	fixture := newWakeupFixture(t, false)
	const requestID = "request-wakeup-workspace"

	intent, err := fixture.service.decodeInboundIntent(requestID, wakeupRunRequest(fixture.convID, wakeupShellFailed()), "run_request")
	if err != nil {
		t.Fatalf("decodeInboundIntent: %v", err)
	}
	if env := intent.RequestContext.GetEnv(); len(env.GetWorkspacePaths()) != 0 || env.GetTerminalsFolder() != "" {
		t.Fatalf("the run names a workspace of its own: %v", env)
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
	case <-fixture.provider.requests:
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not reach the provider")
	}

	paths := snapshotStreamPathContext(stream)
	if len(paths.workspacePaths) != 1 || paths.workspacePaths[0] != wakeupWorkspace || paths.terminalsFolder != wakeupTerminals {
		t.Fatalf("the run works in workspace=%v terminals=%q, want the conversation's own (%q, %q)", paths.workspacePaths, paths.terminalsFolder, wakeupWorkspace, wakeupTerminals)
	}
	if file, found := terminalFilePathForShell(paths.terminalsFolder, "731245"); !found || file != wakeupTerminals+"/731245.txt" {
		t.Fatalf("the finished shell's terminal file is %q (found=%t)", file, found)
	}
}

// What a run does name is kept: only what is missing comes from the
// conversation.
func TestRestoreStreamPathContextKeepsWhatTheRunNamed(t *testing.T) {
	fixture := newWakeupFixture(t, false)
	conversation, err := fixture.service.store.LoadConversation(fixture.convID)
	if err != nil || conversation == nil {
		t.Fatalf("LoadConversation: %v", err)
	}
	stream, err := fixture.service.broker.OpenStream("request-own-workspace", fixture.convID, 2, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "")
	if err != nil {
		t.Fatal(err)
	}
	const elsewhere = `D:\other\checkout`
	updateStreamRequestContextData(stream, &agentv1.RequestContext{Env: &agentv1.RequestContextEnv{WorkspacePaths: []string{elsewhere}}})
	restoreStreamPathContext(stream, conversation)

	paths := snapshotStreamPathContext(stream)
	if len(paths.workspacePaths) != 1 || paths.workspacePaths[0] != elsewhere {
		t.Fatalf("the workspace the run named was replaced: %v", paths.workspacePaths)
	}
	if paths.terminalsFolder != wakeupTerminals {
		t.Fatalf("terminals folder = %q, want the conversation's (%q): the run named none", paths.terminalsFolder, wakeupTerminals)
	}

	// A conversation that never named a workspace has none to give.
	bare, err := fixture.service.broker.OpenStream("request-no-history", "conversation-without-context", 1, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "")
	if err != nil {
		t.Fatal(err)
	}
	restoreStreamPathContext(bare, testConversation(nil))
	if paths := snapshotStreamPathContext(bare); len(paths.workspacePaths) != 0 || paths.terminalsFolder != "" {
		t.Fatalf("a workspace appeared from nowhere: %v %q", paths.workspacePaths, paths.terminalsFolder)
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

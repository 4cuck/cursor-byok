package modeladapter

import (
	"testing"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/proto"
)

func taskNoticeTestAction() *agentv1.ConversationAction {
	return &agentv1.ConversationAction{Action: &agentv1.ConversationAction_BackgroundTaskCompletionAction{
		BackgroundTaskCompletionAction: &agentv1.BackgroundTaskCompletionAction{Completions: []*agentv1.BackgroundTaskCompletion{{
			TaskId: "424242",
			Kind:   agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SHELL,
			Status: agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS,
			Title:  "npm run build",
		}}},
	}}
}

// A run the IDE started because a background task finished is sent to Cursor
// with that action. A resume in its place starts a turn in which the model is
// told nothing.
func TestCursorAccountRunCarriesTheActionTheIDEStartedItWith(t *testing.T) {
	transcript := []Message{
		{Role: "user", Content: "build it in the background"},
		{Role: "assistant", Content: "The build is running."},
	}
	plain := cursorAccountRunMessage(StreamRequest{Messages: transcript}, "claude-fable-5-2").GetRunRequest()
	if plain.GetAction().GetResumeAction() == nil {
		t.Fatalf("a run with nothing new to say sends %v, want a resume", plain.GetAction())
	}

	relayed := taskNoticeTestAction()
	run := cursorAccountRunMessage(StreamRequest{Messages: transcript, CursorConversationAction: relayed}, "claude-fable-5-2").GetRunRequest()
	if !proto.Equal(run.GetAction(), relayed) {
		t.Fatalf("the run sends %v, want the action the IDE started it with: %v", run.GetAction(), relayed)
	}
	if run.GetAction() == relayed {
		t.Fatal("the run shares the caller's action instead of a copy")
	}

	// The forwarder also records its own notice of the finished task as the
	// newest message of the transcript. It is not sent on top of the action.
	withNotice := append(append([]Message(nil), transcript...), Message{Role: "user", Content: "<system_notification>\nThe following task has finished.\n</system_notification>"})
	run = cursorAccountRunMessage(StreamRequest{Messages: withNotice, CursorConversationAction: relayed}, "claude-fable-5-2").GetRunRequest()
	if !proto.Equal(run.GetAction(), relayed) {
		t.Fatalf("with the notice in the transcript the run sends %v, want only the relayed action", run.GetAction())
	}

	// A side call (a compaction summary, a title) is not a turn of the
	// conversation and never carries it.
	side := cursorAccountRunMessage(StreamRequest{Messages: transcript, CursorConversationAction: relayed, CompileSummary: "generate task title"}, "claude-fable-5-2").GetRunRequest()
	if side.GetAction().GetBackgroundTaskCompletionAction() != nil {
		t.Fatalf("a side call carries the relayed action: %v", side.GetAction())
	}
}

// Cursor's server answers such a run by appending its notice of the finished
// task. The IDE can only show it if it is passed on. The echo of the user's
// own message arrives the same way and is not: the IDE already shows that.
func TestReadCursorAccountStreamPassesOnTheTaskNotice(t *testing.T) {
	events := readCursorAccountEvents(t,
		`{"interactionUpdate":{"userMessageAppended":{"userMessage":{"text":"build it in the background","messageId":"user-1"}}}}`,
		`{"interactionUpdate":{"userMessageAppended":{"userMessage":{"text":"<system_notification>\nThe following task has finished.\n</system_notification>","messageId":"notice-1","isSimulatedMsg":true,"simulatedMsgReason":"SIMULATED_MSG_REASON_BACKGROUND_TASK_COMPLETION","simulatedMessageMetadata":{"title":"npm run build","taskId":"424242"}}}}}`,
		`{"interactionUpdate":{"userMessageAppended":{"userMessage":{"text":"commit reminder","messageId":"reminder-1","isSimulatedMsg":true,"simulatedMsgReason":"SIMULATED_MSG_REASON_COMMIT_REMINDER"}}}}`,
		`{"interactionUpdate":{"textDelta":{"text":"The build finished."}}}`,
		`{"interactionUpdate":{"turnEnded":{}}}`,
	)
	var notices []*agentv1.UserMessage
	var text string
	for _, event := range events {
		switch event.Kind {
		case ModelEventKindCursorUserMessageAppended:
			notices = append(notices, event.CursorUserMessage)
		case ModelEventKindTextDelta:
			text += event.Text
		}
	}
	if len(notices) != 1 {
		t.Fatalf("%d appended messages passed on, want only the notice of the finished task", len(notices))
	}
	notice := notices[0]
	if notice.GetMessageId() != "notice-1" || notice.GetSimulatedMessageMetadata().GetTaskId() != "424242" || notice.GetSimulatedMessageMetadata().GetTitle() != "npm run build" {
		t.Fatalf("the notice passed on is %v, want Cursor's own with its task", notice)
	}
	if text != "The build finished." {
		t.Fatalf("the reply after the notice = %q", text)
	}
}

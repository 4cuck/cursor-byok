package modeladapter

import "cursor/gen/agentv1"

// When a background task of a conversation finishes (a subagent the model
// sent to the background, a shell it left running), the IDE starts a run of
// that conversation whose action is the list of what finished. Cursor's
// server answers it by appending a simulated user message, the notice the
// model reads, and the model then reacts to it. The gateway relays that
// action (StreamRequest.CursorConversationAction) and passes the notice on.

// cursorAccountIsTaskNotice reports whether a message the server appended is
// its notice that a background task finished.
func cursorAccountIsTaskNotice(message *agentv1.UserMessage) bool {
	return message != nil && message.GetIsSimulatedMsg() &&
		message.GetSimulatedMsgReason() == agentv1.SimulatedMsgReason_SIMULATED_MSG_REASON_BACKGROUND_TASK_COMPLETION
}

// CursorBackgroundTaskCompletions returns the tasks a conversation action
// reports as finished, nil when the action is of another kind.
func CursorBackgroundTaskCompletions(action *agentv1.ConversationAction) []*agentv1.BackgroundTaskCompletion {
	return action.GetBackgroundTaskCompletionAction().GetCompletions()
}

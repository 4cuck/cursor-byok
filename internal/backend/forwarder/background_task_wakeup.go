// background_task_wakeup.go handles the run the IDE starts when background
// tasks of a conversation have finished.
//
// A subagent the model sent to the background, or a shell it left running,
// ends some time after the turn that started it. The IDE then starts a run of
// the parent conversation that carries no user message: its action is the
// list of what finished (BackgroundTaskCompletionAction). Cursor's server
// answers such a run by putting a notice per task into the conversation,
// each a simulated user message, and the model reacts to them: it tells the
// user, reads the result, goes on with the work.
//
// Without this file the run was started as a plain resume and the model was
// told nothing.
//
// On a Cursor account the action is relayed to Cursor's server as it is, the
// server writes the notices, and they are passed on to the IDE. For any other
// model the forwarder writes the same notices itself.
//
// The IDE starts these runs only while its long_running_jobs gate is on, and
// it takes the gate from the bootstrap this process serves
// (server/upstream/mocks.go).
package forwarder

import (
	"fmt"
	"strings"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	modeladapter "cursor/internal/backend/agent/model"
	"cursor/internal/logger"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// promptContextSourceBackgroundTaskCompletion marks the notice in history.
const promptContextSourceBackgroundTaskCompletion = "background_task_completion"

// inboundBackgroundCompletion returns the "background tasks finished" action
// of a run request, nil for any other run.
func inboundBackgroundCompletion(runRequest *agentv1.AgentRunRequest) *agentv1.BackgroundTaskCompletionAction {
	action := runRequest.GetAction().GetBackgroundTaskCompletionAction()
	if action == nil || len(reportedBackgroundTasks(action)) == 0 {
		return nil
	}
	return proto.CloneOf(action)
}

// reportedBackgroundTasks returns the tasks the model is to be told about.
// A record-only entry is bookkeeping for the server, not news.
func reportedBackgroundTasks(action *agentv1.BackgroundTaskCompletionAction) []*agentv1.BackgroundTaskCompletion {
	var tasks []*agentv1.BackgroundTaskCompletion
	for _, task := range action.GetCompletions() {
		if task != nil && !task.GetRecordOnly() {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func backgroundTaskKindLabel(kind agentv1.BackgroundTaskKind) string {
	switch kind {
	case agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SHELL:
		return "shell"
	case agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SUBAGENT:
		return "subagent"
	default:
		return "task"
	}
}

func backgroundTaskStatusLabel(status agentv1.BackgroundTaskStatus) string {
	switch status {
	case agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS:
		return "success"
	case agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_ERROR:
		return "error"
	case agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_ABORTED:
		return "aborted"
	default:
		return "unknown"
	}
}

// backgroundTaskNotice is the notice of one background task: what the model
// reads, and the task it is about.
type backgroundTaskNotice struct {
	Text   string
	Title  string
	TaskID string
}

const (
	// What the last notice of a run asks of the model, as Cursor's server
	// asks it. For a subagent the server goes on about linking cloud agents,
	// which no other model has; that part is left out.
	backgroundShellFollowUpQuery    = "Briefly inform the user about the task result and perform any follow-up actions (if needed)."
	backgroundSubagentFollowUpQuery = "Perform any necessary follow-up actions in response to the subagent completion above. If no follow-up work is needed, no further action is required. If you mention an agent or subagent in your response, link it with the `[Name](id)` Don't use generic label such as `[agent]`, `[worker]`, or `[subagent]`. Don't repeat the same confirmation every time."
)

func backgroundTaskHasFinished(task *agentv1.BackgroundTaskCompletion) bool {
	switch task.GetReason() {
	case agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_UNSPECIFIED,
		agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED:
		return true
	default:
		// Progress, a message, a request for attention: it is still running.
		return false
	}
}

// backgroundTaskNotices writes the notices the model reads, in the words and
// the layout Cursor's server uses for them (captured from the live server for
// a finished shell, a finished subagent, and both in one run): one notice per
// task, in the order reported, and on the last one what the model is asked to
// do. The opening line for work that is still running was not captured; it is
// this file's own.
func backgroundTaskNotices(action *agentv1.BackgroundTaskCompletionAction) []backgroundTaskNotice {
	tasks := reportedBackgroundTasks(action)
	notices := make([]backgroundTaskNotice, 0, len(tasks))
	for index, task := range tasks {
		var text strings.Builder
		text.WriteString("<system_notification>\n")
		if backgroundTaskHasFinished(task) {
			text.WriteString("The following task has finished.")
		} else {
			text.WriteString("The following background work has an update.")
		}
		text.WriteString(" If you were already aware, ignore this notification and do not restate prior responses.\n")
		line := func(name, value string) {
			if value = strings.TrimSpace(value); value != "" {
				fmt.Fprintf(&text, "%s: %s\n", name, value)
			}
		}
		text.WriteString("\n<task>\n")
		line("kind", backgroundTaskKindLabel(task.GetKind()))
		line("status", backgroundTaskStatusLabel(task.GetStatus()))
		line("task_id", task.GetTaskId())
		line("title", task.GetTitle())
		line("tool_call_id", task.GetToolCallId())
		line("agent_id", task.GetSubagentId())
		line("detail", task.GetDetail())
		line("output_path", task.GetOutputPath())
		text.WriteString("</task>\n")
		text.WriteString("</system_notification>")
		if index == len(tasks)-1 {
			text.WriteString("\n")
			if reminder := strings.TrimSpace(action.GetSystemReminder()); reminder != "" {
				text.WriteString(wrapSystemReminder(reminder))
				text.WriteString("\n")
			}
			query := backgroundShellFollowUpQuery
			if task.GetKind() == agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SUBAGENT {
				query = backgroundSubagentFollowUpQuery
			}
			text.WriteString("<user_query>" + query + "</user_query>")
		}
		notices = append(notices, backgroundTaskNotice{
			Text:   text.String(),
			Title:  strings.TrimSpace(task.GetTitle()),
			TaskID: strings.TrimSpace(task.GetTaskId()),
		})
	}
	return notices
}

// backgroundCompletionRunEntries are the history entries of a run that was
// started because background tasks finished: the notices, which are what the
// model reads in place of a user message, and one record per task.
//
// The notices stand in the transcript as one message. A Cursor account pass
// takes the last message of the transcript as the turn's own and everything
// before it as history; the relayed action replaces the turn's message, and
// Cursor's server writes its notices from it. A notice left in the history
// would be told to the model twice whenever that history is sent.
func backgroundCompletionRunEntries(turnSeq int64, requestID string, action *agentv1.BackgroundTaskCompletionAction) []HistoryEntry {
	notices := backgroundTaskNotices(action)
	if len(notices) == 0 {
		return nil
	}
	texts := make([]string, 0, len(notices))
	for _, notice := range notices {
		texts = append(texts, notice.Text)
	}
	entries := []HistoryEntry{newPromptContextEntry(turnSeq, requestID, newPromptContextMessage(
		promptContextSourceBackgroundTaskCompletion,
		modeladapter.Message{Role: "user", Content: strings.Join(texts, "\n")},
		true,
	))}
	return append(entries, backgroundTaskCompletionMetadataEntries(turnSeq, requestID, &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ConversationAction{ConversationAction: &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_BackgroundTaskCompletionAction{BackgroundTaskCompletionAction: action},
		}},
	})...)
}

// beginBackgroundCompletionRun prepares a stream for a run that was started
// because background tasks finished.
func (service *Service) beginBackgroundCompletionRun(stream *ActiveStream, action *agentv1.BackgroundTaskCompletionAction) {
	if service == nil || stream == nil || action == nil {
		return
	}
	tasks := reportedBackgroundTasks(action)
	// The relayed action carries the tasks alone. The IDE's own envelope
	// refers to request-context blobs it stored with this process, which
	// Cursor's server does not have.
	stream.mu.Lock()
	stream.cursorWakeupAction = &agentv1.ConversationAction{
		Action: &agentv1.ConversationAction_BackgroundTaskCompletionAction{BackgroundTaskCompletionAction: proto.CloneOf(action)},
	}
	stream.mu.Unlock()
	for _, task := range tasks {
		logger.Infof("forwarder background task finished request_id=%s conversation_id=%s kind=%s status=%s task_id=%s tool_call_id=%s reason=%s",
			strings.TrimSpace(stream.RequestID), strings.TrimSpace(stream.ConversationID), backgroundTaskKindLabel(task.GetKind()), backgroundTaskStatusLabel(task.GetStatus()),
			strings.TrimSpace(task.GetTaskId()), strings.TrimSpace(task.GetToolCallId()), task.GetReason().String())
		service.recordFinishedBackgroundSubagent(stream, task)
	}
}

// recordFinishedBackgroundSubagent puts the outcome of a background subagent
// into this turn's checkpoints. While it ran, its Task call was reported as
// BACKGROUNDED (native_subagent_background.go); this is its end.
func (service *Service) recordFinishedBackgroundSubagent(stream *ActiveStream, task *agentv1.BackgroundTaskCompletion) {
	toolCallID := strings.TrimSpace(task.GetToolCallId())
	if task.GetKind() != agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SUBAGENT || toolCallID == "" || !backgroundTaskHasFinished(task) {
		return
	}
	var status agentv1.SubagentRunStatus
	detail := ""
	switch task.GetStatus() {
	case agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS:
		status = agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_SUCCESS
	case agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_ERROR:
		status, detail = agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_ERROR, strings.TrimSpace(task.GetDetail())
	case agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_ABORTED:
		status, detail = agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_ABORTED, strings.TrimSpace(task.GetDetail())
	default:
		return
	}
	service.recordDelegationRunTerminal(stream, runtimecore.PendingExec{ToolCallID: toolCallID}, status, "Cursor 子代理", detail)
}

// announceBackgroundCompletion shows the notices in the IDE for a model that
// is not a Cursor account model, one message per task as Cursor's server
// sends them: the IDE pairs each message with the task it reported, in
// order. A Cursor account turn gets the notices from Cursor's server
// (applyCursorUserMessageAppended).
func (service *Service) announceBackgroundCompletion(stream *ActiveStream, action *agentv1.BackgroundTaskCompletionAction) error {
	if service == nil || stream == nil || service.cursorAccountModel(activeStreamModelID(stream)) {
		return nil
	}
	notices := backgroundTaskNotices(action)
	if len(notices) == 0 {
		return nil
	}
	stream.mu.Lock()
	mode := stream.Mode
	stream.mu.Unlock()
	for _, notice := range notices {
		message := &agentv1.UserMessage{
			Text:               notice.Text,
			MessageId:          uuid.NewString(),
			Mode:               mode,
			IsSimulatedMsg:     proto.Bool(true),
			SimulatedMsgReason: agentv1.SimulatedMsgReason_SIMULATED_MSG_REASON_BACKGROUND_TASK_COMPLETION.Enum(),
			SimulatedMessageMetadata: &agentv1.UserMessage_SimulatedMessageMetadata{
				Title:  proto.String(notice.Title),
				TaskId: proto.String(notice.TaskID),
			},
		}
		if err := service.broker.Publish(stream.RequestID, StreamEvent{Message: buildUserMessageAppendedMessage(message)}); err != nil {
			return err
		}
	}
	return nil
}

// applyCursorUserMessageAppended passes on the notice Cursor's server wrote.
// The IDE shows it and takes the turn as the answer to it.
func (service *Service) applyCursorUserMessageAppended(stream *ActiveStream, message *agentv1.UserMessage) error {
	if service == nil || stream == nil || message == nil {
		return nil
	}
	stream.mu.Lock()
	terminal := isTerminalStreamStatus(stream.Status)
	stream.mu.Unlock()
	if terminal {
		return nil
	}
	return service.broker.Publish(stream.RequestID, StreamEvent{Message: buildUserMessageAppendedMessage(proto.CloneOf(message))})
}

func buildUserMessageAppendedMessage(message *agentv1.UserMessage) *agentv1.AgentServerMessage {
	return &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_UserMessageAppended{
					UserMessageAppended: &agentv1.UserMessageAppendedUpdate{UserMessage: message},
				},
			},
		},
	}
}

// cursorWakeupActionOf returns the action a Cursor account pass of this turn
// is to start with, nil for an ordinary turn and once a pass has carried it.
func cursorWakeupActionOf(stream *ActiveStream) *agentv1.ConversationAction {
	if stream == nil {
		return nil
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return proto.CloneOf(stream.cursorWakeupAction)
}

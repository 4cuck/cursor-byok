// background_shell_state.go tracks what the client reports about shells that
// outlive their tool call: stream events, spawn results and completion
// notifications observed on the current request stream.
package forwarder

import (
	"strconv"
	"strings"
	"time"

	"cursor/gen/agentv1"
	execbridge "cursor/internal/backend/agent/bridge/exec"
	runtimecore "cursor/internal/backend/agent/core"
)

const (
	backgroundShellStatusBackgrounded     = "backgrounded"
	backgroundShellStatusRunning          = "running"
	backgroundShellStatusCompleted        = "completed"
	backgroundShellStatusRejected         = "rejected"
	backgroundShellStatusPermissionDenied = "permission_denied"
	backgroundShellStatusTransportClosed  = "transport_closed"
	backgroundShellStatusUnknown          = "unknown"

	backgroundShellActionSourceClient            = "client"
	backgroundShellActionSourceLocalBackgrounded = "local_shell_backgrounded"
)

func isBackgroundShellTerminalStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case backgroundShellStatusCompleted, backgroundShellStatusRejected, backgroundShellStatusPermissionDenied, backgroundShellStatusTransportClosed:
		return true
	default:
		return false
	}
}

func (service *Service) observeBackgroundShellExecClientMessage(stream *ActiveStream, pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) {
	if stream == nil || message == nil {
		return
	}
	now := time.Now().UTC()
	stream.mu.Lock()
	defer stream.mu.Unlock()
	service.observeBackgroundShellSpawnResultLocked(stream, pending, message.GetBackgroundShellSpawnResult(), now)
	service.observeForceBackgroundShellResultLocked(stream, pending, message.GetForceBackgroundShellResult(), now)
}

func (service *Service) observeMissingBackgroundShellExecClientMessage(stream *ActiveStream, message *agentv1.ExecClientMessage) bool {
	if stream == nil || message == nil {
		return false
	}
	if message.GetBackgroundShellSpawnResult() == nil && message.GetForceBackgroundShellResult() == nil {
		return false
	}
	now := time.Now().UTC()
	stream.mu.Lock()
	defer stream.mu.Unlock()
	pending := runtimecore.PendingExec{
		MessageID: message.GetId(),
		ExecID:    strings.TrimSpace(message.GetExecId()),
		ExecKind:  "shell",
	}
	return service.observeBackgroundShellSpawnResultLocked(stream, pending, message.GetBackgroundShellSpawnResult(), now) || service.observeForceBackgroundShellResultLocked(stream, pending, message.GetForceBackgroundShellResult(), now)
}

func (service *Service) observeBackgroundShellSpawnResultLocked(stream *ActiveStream, pending runtimecore.PendingExec, result *agentv1.BackgroundShellSpawnResult, now time.Time) bool {
	if stream == nil || result == nil {
		return false
	}
	if stream.BackgroundShells == nil {
		stream.BackgroundShells = make(map[string]*BackgroundShellState)
	}
	if stream.BackgroundShellsByMessageID == nil {
		stream.BackgroundShellsByMessageID = make(map[uint32]string)
	}
	if stream.BackgroundShellsByExecID == nil {
		stream.BackgroundShellsByExecID = make(map[string]string)
	}
	success := result.GetSuccess()
	if success != nil {
		shellID := strconv.FormatUint(uint64(success.GetShellId()), 10)
		state := stream.BackgroundShells[shellID]
		if state == nil {
			state = &BackgroundShellState{ShellID: shellID, CreatedAt: now}
			stream.BackgroundShells[shellID] = state
		}
		pid := success.Pid
		state.Command = firstNonEmpty(strings.TrimSpace(success.GetCommand()), state.Command)
		state.WorkingDirectory = firstNonEmpty(strings.TrimSpace(success.GetWorkingDirectory()), state.WorkingDirectory)
		state.PID = pid
		state.Status = backgroundShellStatusBackgrounded
		state.OriginalToolCallID = firstNonEmpty(strings.TrimSpace(pending.ToolCallID), state.OriginalToolCallID)
		state.OriginalExecID = firstNonEmpty(strings.TrimSpace(pending.ExecID), state.OriginalExecID)
		if state.OriginalMessageID == 0 {
			state.OriginalMessageID = pending.MessageID
		}
		if len(state.ArgsJSON) == 0 && len(pending.ArgsJSON) > 0 {
			state.ArgsJSON = append([]byte(nil), pending.ArgsJSON...)
		}
		state.ModelCallID = firstNonEmpty(strings.TrimSpace(pending.ModelCallID), state.ModelCallID)
		state.LastActivityAt = now
		if pending.MessageID != 0 {
			stream.BackgroundShellsByMessageID[pending.MessageID] = shellID
		}
		if strings.TrimSpace(pending.ExecID) != "" {
			stream.BackgroundShellsByExecID[strings.TrimSpace(pending.ExecID)] = shellID
		}
		stream.UpdatedAt = now
		return true
	}
	shellID := backgroundShellIDForMessageLocked(stream, pending.MessageID, pending.ExecID)
	if shellID == "" {
		return false
	}
	state := ensureBackgroundShellStateLocked(stream, shellID, pending, now)
	if state == nil {
		return false
	}
	switch {
	case result.GetRejected() != nil:
		state.Status = backgroundShellStatusRejected
		state.StderrBuffer += strings.TrimSpace(result.GetRejected().GetReason())
	case result.GetPermissionDenied() != nil:
		state.Status = backgroundShellStatusPermissionDenied
		state.StderrBuffer += strings.TrimSpace(result.GetPermissionDenied().GetError())
	case result.GetError() != nil:
		state.Status = backgroundShellStatusTransportClosed
		state.StderrBuffer += strings.TrimSpace(result.GetError().GetError())
	default:
		return false
	}
	state.LastActivityAt = now
	state.CompletedAt = now
	stream.UpdatedAt = now
	return true
}

func (service *Service) observeForceBackgroundShellResultLocked(stream *ActiveStream, pending runtimecore.PendingExec, result *agentv1.ForceBackgroundShellResult, now time.Time) bool {
	if stream == nil || result == nil || result.GetShellResult() == nil {
		return false
	}
	shellResult := result.GetShellResult()
	shellIDValue := uint32(0)
	if success := shellResult.GetSuccess(); success != nil {
		shellIDValue = success.GetShellId()
	}
	if shellIDValue == 0 {
		return false
	}
	shellID := strconv.FormatUint(uint64(shellIDValue), 10)
	state := ensureBackgroundShellStateLocked(stream, shellID, pending, now)
	if state == nil {
		return false
	}
	state.Status = backgroundShellStatusBackgrounded
	state.LastActivityAt = now
	stream.UpdatedAt = now
	return true
}

func (service *Service) observeShellExecClientMessage(stream *ActiveStream, pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) {
	if stream == nil || message == nil || strings.TrimSpace(pending.ExecKind) != "shell" {
		return
	}
	shellStream := message.GetShellStream()
	if shellStream == nil {
		return
	}
	now := time.Now().UTC()
	stream.mu.Lock()
	defer stream.mu.Unlock()
	service.observeShellStreamLocked(stream, pending, shellStream, now)
}

func (service *Service) observeMissingShellExecClientMessage(stream *ActiveStream, message *agentv1.ExecClientMessage) bool {
	if stream == nil || message == nil || message.GetShellStream() == nil {
		return false
	}
	now := time.Now().UTC()
	stream.mu.Lock()
	defer stream.mu.Unlock()
	shellID := backgroundShellIDForMessageLocked(stream, message.GetId(), message.GetExecId())
	if shellID == "" {
		return false
	}
	pending := runtimecore.PendingExec{
		MessageID: message.GetId(),
		ExecID:    strings.TrimSpace(message.GetExecId()),
		ExecKind:  "shell",
	}
	if state := stream.BackgroundShells[shellID]; state != nil {
		pending.ToolCallID = state.OriginalToolCallID
		pending.ArgsJSON = append([]byte(nil), state.ArgsJSON...)
		pending.ModelCallID = state.ModelCallID
	}
	service.observeShellStreamLocked(stream, pending, message.GetShellStream(), now)
	return true
}

func (service *Service) observeShellStreamLocked(stream *ActiveStream, pending runtimecore.PendingExec, shellStream *agentv1.ShellStream, now time.Time) {
	if stream.BackgroundShells == nil {
		stream.BackgroundShells = make(map[string]*BackgroundShellState)
	}
	if stream.BackgroundShellsByMessageID == nil {
		stream.BackgroundShellsByMessageID = make(map[uint32]string)
	}
	if stream.BackgroundShellsByExecID == nil {
		stream.BackgroundShellsByExecID = make(map[string]string)
	}

	shellID := backgroundShellIDForMessageLocked(stream, pending.MessageID, pending.ExecID)
	switch event := shellStream.GetEvent().(type) {
	case *agentv1.ShellStream_Backgrounded:
		shellID = strconv.FormatUint(uint64(event.Backgrounded.GetShellId()), 10)
		state := stream.BackgroundShells[shellID]
		if state == nil {
			state = &BackgroundShellState{ShellID: shellID, CreatedAt: now}
			stream.BackgroundShells[shellID] = state
		}
		state.Command = firstNonEmpty(strings.TrimSpace(event.Backgrounded.GetCommand()), state.Command)
		state.WorkingDirectory = firstNonEmpty(strings.TrimSpace(event.Backgrounded.GetWorkingDirectory()), state.WorkingDirectory)
		state.PID = event.Backgrounded.Pid
		state.Status = backgroundShellStatusBackgrounded
		state.OriginalToolCallID = strings.TrimSpace(pending.ToolCallID)
		state.OriginalExecID = strings.TrimSpace(pending.ExecID)
		state.OriginalMessageID = pending.MessageID
		state.ArgsJSON = append([]byte(nil), pending.ArgsJSON...)
		state.ModelCallID = strings.TrimSpace(pending.ModelCallID)
		state.LastActivityAt = now
		if pending.MessageID != 0 {
			stream.BackgroundShellsByMessageID[pending.MessageID] = shellID
		}
		if strings.TrimSpace(pending.ExecID) != "" {
			stream.BackgroundShellsByExecID[strings.TrimSpace(pending.ExecID)] = shellID
		}
	case *agentv1.ShellStream_Stdout:
		state := ensureBackgroundShellStateLocked(stream, shellID, pending, now)
		if state == nil {
			return
		}
		state.StdoutBuffer += execbridge.DecodeShellStdout(event.Stdout)
		state.Status = backgroundShellStatusRunning
		state.LastActivityAt = now
	case *agentv1.ShellStream_Stderr:
		state := ensureBackgroundShellStateLocked(stream, shellID, pending, now)
		if state == nil {
			return
		}
		state.StderrBuffer += event.Stderr.GetData()
		state.Status = backgroundShellStatusRunning
		state.LastActivityAt = now
	case *agentv1.ShellStream_Exit:
		state := ensureBackgroundShellStateLocked(stream, shellID, pending, now)
		if state == nil {
			return
		}
		exitCode := int32(event.Exit.GetCode())
		state.ExitCode = &exitCode
		state.WorkingDirectory = firstNonEmpty(strings.TrimSpace(event.Exit.GetCwd()), state.WorkingDirectory)
		state.Status = backgroundShellStatusCompleted
		state.LastActivityAt = now
		state.CompletedAt = now
	case *agentv1.ShellStream_Rejected:
		state := ensureBackgroundShellStateLocked(stream, shellID, pending, now)
		if state == nil {
			return
		}
		state.Status = backgroundShellStatusRejected
		state.StderrBuffer += strings.TrimSpace(event.Rejected.GetReason())
		state.LastActivityAt = now
		state.CompletedAt = now
	case *agentv1.ShellStream_PermissionDenied:
		state := ensureBackgroundShellStateLocked(stream, shellID, pending, now)
		if state == nil {
			return
		}
		state.Status = backgroundShellStatusPermissionDenied
		state.StderrBuffer += strings.TrimSpace(event.PermissionDenied.GetError())
		state.LastActivityAt = now
		state.CompletedAt = now
	}
	stream.UpdatedAt = now
}

func observeBackgroundTaskCompletionAction(stream *ActiveStream, message *agentv1.AgentClientMessage) {
	if stream == nil || message == nil {
		return
	}
	action := message.GetConversationAction()
	if action == nil {
		return
	}
	item, ok := action.GetAction().(*agentv1.ConversationAction_BackgroundTaskCompletionAction)
	if !ok || item.BackgroundTaskCompletionAction == nil {
		return
	}
	now := time.Now().UTC()
	stream.mu.Lock()
	defer stream.mu.Unlock()
	for _, completion := range item.BackgroundTaskCompletionAction.GetCompletions() {
		observeBackgroundTaskCompletionLocked(stream, completion, now)
	}
}

func observeBackgroundTaskCompletionLocked(stream *ActiveStream, completion *agentv1.BackgroundTaskCompletion, now time.Time) bool {
	if stream == nil || completion == nil {
		return false
	}
	// 不再只处理 shell 类型；子代理/Task 的完成通知也需要处理
	if completion.GetKind() == agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SHELL {
		return observeShellBackgroundTaskCompletionLocked(stream, completion, now)
	}
	// 非 shell 类型（如 Task 子代理）：记录 metadata 但不阻塞
	// 真正的终态通过 exec result / exec control 路径完成
	return false
}

func observeShellBackgroundTaskCompletionLocked(stream *ActiveStream, completion *agentv1.BackgroundTaskCompletion, now time.Time) bool {
	if stream == nil || completion == nil || completion.GetKind() != agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SHELL {
		return false
	}
	shellID := strings.TrimSpace(completion.GetTaskId())
	if shellID == "" {
		return false
	}
	state := ensureBackgroundShellStateLocked(stream, shellID, runtimecore.PendingExec{}, now)
	if state == nil {
		return false
	}
	detail := strings.TrimSpace(completion.GetDetail())
	switch completion.GetStatus() {
	case agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS:
		state.Status = backgroundShellStatusCompleted
		if detail != "" {
			state.StdoutBuffer = appendBackgroundShellBuffer(state.StdoutBuffer, detail)
		}
	case agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_ERROR:
		state.Status = backgroundShellStatusTransportClosed
		if detail != "" {
			state.StderrBuffer = appendBackgroundShellBuffer(state.StderrBuffer, detail)
		}
	case agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_ABORTED:
		state.Status = backgroundShellStatusTransportClosed
		if detail != "" {
			state.StderrBuffer = appendBackgroundShellBuffer(state.StderrBuffer, detail)
		}
	default:
		if completion.GetReason() == agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_PROGRESS {
			state.Status = backgroundShellStatusRunning
		} else {
			return false
		}
	}
	state.LastActivityAt = now
	if completion.GetReason() == agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED || isBackgroundShellTerminalStatus(state.Status) {
		state.CompletedAt = now
	}
	stream.UpdatedAt = now
	return true
}

func observeBackgroundShellAction(stream *ActiveStream, message *agentv1.AgentClientMessage) (string, bool) {
	if stream == nil || message == nil {
		return "", false
	}
	action := message.GetConversationAction()
	if action == nil {
		return "", false
	}
	item, ok := action.GetAction().(*agentv1.ConversationAction_BackgroundShellAction)
	if !ok || item.BackgroundShellAction == nil {
		return "", false
	}
	return recordBackgroundShellActionMemory(stream, item.BackgroundShellAction.GetToolCallId(), time.Now().UTC())
}

// observeBackgroundSubagentAction 处理客户端把子代理转入后台的通知，
// 标记对应 pending subagent exec 为 backgrounded 并返回 tool_call_id 供 metadata 记录。
func observeBackgroundSubagentAction(stream *ActiveStream, message *agentv1.AgentClientMessage) (string, bool) {
	if stream == nil || message == nil {
		return "", false
	}
	action := message.GetConversationAction()
	if action == nil {
		return "", false
	}
	item, ok := action.GetAction().(*agentv1.ConversationAction_BackgroundSubagentAction)
	if !ok || item.BackgroundSubagentAction == nil {
		return "", false
	}
	toolCallID := strings.TrimSpace(item.BackgroundSubagentAction.GetToolCallId())
	if toolCallID == "" {
		return "", false
	}
	now := time.Now().UTC()
	stream.mu.Lock()
	defer stream.mu.Unlock()
	for execID, pending := range stream.PendingExecs {
		if pending.ExecKind != "subagent" {
			continue
		}
		if strings.TrimSpace(pending.ToolCallID) != toolCallID {
			continue
		}
		pending.StreamState = "backgrounded"
		pending.LastShellActivityAt = now
		stream.PendingExecs[execID] = pending
	}
	stream.UpdatedAt = now
	return toolCallID, true
}

// newBackgroundSubagentActionMetadataEntry 构造子代理后台化的 metadata entry。
func newBackgroundSubagentActionMetadataEntry(turnSeq int64, requestID string, toolCallID string, source string) HistoryEntry {
	return newMetadataEntry(turnSeq, requestID, "background_subagent_action", map[string]any{
		"tool_call_id": strings.TrimSpace(toolCallID),
		"source":       strings.TrimSpace(source),
	})
}

func recordBackgroundShellActionMemory(stream *ActiveStream, toolCallID string, now time.Time) (string, bool) {
	trimmedToolCallID := strings.TrimSpace(toolCallID)
	if stream == nil || trimmedToolCallID == "" {
		return "", false
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return recordBackgroundShellActionLocked(stream, trimmedToolCallID, now)
}

func recordBackgroundShellActionLocked(stream *ActiveStream, toolCallID string, now time.Time) (string, bool) {
	trimmedToolCallID := strings.TrimSpace(toolCallID)
	if stream == nil || trimmedToolCallID == "" {
		return "", false
	}
	if stream.BackgroundShellActions == nil {
		stream.BackgroundShellActions = make(map[string]time.Time)
	}
	if _, exists := stream.BackgroundShellActions[trimmedToolCallID]; exists {
		return trimmedToolCallID, false
	}
	stream.BackgroundShellActions[trimmedToolCallID] = now
	stream.UpdatedAt = now
	return trimmedToolCallID, true
}

func newBackgroundShellActionMetadataEntry(turnSeq int64, requestID string, toolCallID string, source string) HistoryEntry {
	return newMetadataEntry(turnSeq, requestID, "background_shell_action", map[string]any{
		"tool_call_id": strings.TrimSpace(toolCallID),
		"source":       strings.TrimSpace(source),
	})
}

func backgroundTaskCompletionMetadataEntries(turnSeq int64, requestID string, message *agentv1.AgentClientMessage) []HistoryEntry {
	if message == nil || message.GetConversationAction() == nil {
		return nil
	}
	item, ok := message.GetConversationAction().GetAction().(*agentv1.ConversationAction_BackgroundTaskCompletionAction)
	if !ok || item.BackgroundTaskCompletionAction == nil {
		return nil
	}
	completions := item.BackgroundTaskCompletionAction.GetCompletions()
	if len(completions) == 0 {
		return nil
	}
	entries := make([]HistoryEntry, 0, len(completions))
	for _, completion := range completions {
		if completion == nil {
			continue
		}
		values := map[string]any{
			"task_id": strings.TrimSpace(completion.GetTaskId()),
			"kind":    completion.GetKind().String(),
			"status":  completion.GetStatus().String(),
			"reason":  completion.GetReason().String(),
		}
		if title := strings.TrimSpace(completion.GetTitle()); title != "" {
			values["title"] = title
		}
		if detail := strings.TrimSpace(completion.GetDetail()); detail != "" {
			values["detail"] = detail
		}
		if outputPath := strings.TrimSpace(completion.GetOutputPath()); outputPath != "" {
			values["output_path"] = outputPath
		}
		if threadID := strings.TrimSpace(completion.GetThreadId()); threadID != "" {
			values["thread_id"] = threadID
		}
		entries = append(entries, newMetadataEntry(turnSeq, requestID, "background_task_completion_action", values))
	}
	return entries
}

// nameBackgroundedShellOutput tells the result of a Shell call whose command
// was sent to the background where the command's output goes. Only this
// stream knows the client's terminals folder; the bridge built the result
// without it.
//
// The model reads the card's result, so the folder goes there (the output is
// <folder>/<shell id>.txt, as the client's own result has it). The result
// text, which the model is sent when the card cannot be used, names the file.
func nameBackgroundedShellOutput(stream *ActiveStream, pending runtimecore.PendingExec, message *agentv1.ExecClientMessage, result *execbridge.ExecApplyResult) {
	backgrounded := message.GetShellStream().GetBackgrounded()
	if stream == nil || result == nil || backgrounded == nil || strings.TrimSpace(pending.ExecKind) != "shell" {
		return
	}
	stream.mu.Lock()
	terminalsFolder := strings.TrimSpace(stream.TerminalsFolder)
	stream.mu.Unlock()
	terminalFile, ok := terminalFilePathForShell(terminalsFolder, strconv.FormatUint(uint64(backgrounded.GetShellId()), 10))
	if !ok {
		return
	}
	result.ToolResultPayload = execbridge.ShellBackgroundedText(backgrounded, pending, terminalFile)
	if card := result.ToolCall.GetShellToolCall().GetResult(); card != nil && card.GetIsBackground() {
		card.TerminalsFolder = &terminalsFolder
	}
}

func shellToolCallIsBackgrounded(toolCall *agentv1.ToolCall) bool {
	if toolCall == nil {
		return false
	}
	shellToolCall := toolCall.GetShellToolCall()
	if shellToolCall == nil || shellToolCall.GetResult() == nil {
		return false
	}
	return shellToolCall.GetResult().GetIsBackground()
}

func appendBackgroundShellBuffer(current string, value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return current
	}
	if current == "" || strings.HasSuffix(current, "\n") {
		return current + trimmed
	}
	return current + "\n" + trimmed
}

func ensureBackgroundShellStateLocked(stream *ActiveStream, shellID string, pending runtimecore.PendingExec, now time.Time) *BackgroundShellState {
	trimmedShellID := strings.TrimSpace(shellID)
	if trimmedShellID == "" {
		return nil
	}
	if stream.BackgroundShells == nil {
		stream.BackgroundShells = make(map[string]*BackgroundShellState)
	}
	if stream.BackgroundShellsByMessageID == nil {
		stream.BackgroundShellsByMessageID = make(map[uint32]string)
	}
	if stream.BackgroundShellsByExecID == nil {
		stream.BackgroundShellsByExecID = make(map[string]string)
	}
	state := stream.BackgroundShells[trimmedShellID]
	if state == nil {
		state = &BackgroundShellState{ShellID: trimmedShellID, Status: backgroundShellStatusRunning, CreatedAt: now}
		stream.BackgroundShells[trimmedShellID] = state
	}
	if pending.MessageID != 0 {
		stream.BackgroundShellsByMessageID[pending.MessageID] = trimmedShellID
	}
	if strings.TrimSpace(pending.ExecID) != "" {
		stream.BackgroundShellsByExecID[strings.TrimSpace(pending.ExecID)] = trimmedShellID
	}
	if state.OriginalToolCallID == "" {
		state.OriginalToolCallID = strings.TrimSpace(pending.ToolCallID)
	}
	if state.OriginalExecID == "" {
		state.OriginalExecID = strings.TrimSpace(pending.ExecID)
	}
	if state.OriginalMessageID == 0 {
		state.OriginalMessageID = pending.MessageID
	}
	if len(state.ArgsJSON) == 0 && len(pending.ArgsJSON) > 0 {
		state.ArgsJSON = append([]byte(nil), pending.ArgsJSON...)
	}
	if state.ModelCallID == "" {
		state.ModelCallID = strings.TrimSpace(pending.ModelCallID)
	}
	return state
}

func backgroundShellIDForMessageLocked(stream *ActiveStream, messageID uint32, execID string) string {
	if stream == nil {
		return ""
	}
	if strings.TrimSpace(execID) != "" {
		if shellID := strings.TrimSpace(stream.BackgroundShellsByExecID[strings.TrimSpace(execID)]); shellID != "" {
			return shellID
		}
	}
	if messageID != 0 {
		return strings.TrimSpace(stream.BackgroundShellsByMessageID[messageID])
	}
	return ""
}

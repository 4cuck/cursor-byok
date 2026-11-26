// native_subagent_background.go handles the Cursor client moving a native
// subagent to the background.
//
// The client answers a subagent exec in one of two ways. A subagent that ran
// in the foreground is answered when it ends, with its final message. A
// subagent that was asked to run in the background, or was sent there while
// running, is answered at once with only an agent id, a background reason and
// a transcript path. The Task call is over in both cases. The subagent is
// over only in the first.
package forwarder

import (
	"context"
	"strings"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	"cursor/internal/backend/delegation"
	"cursor/internal/logger"
)

// noteNativeSubagentBackgrounded records a background handoff. From here the
// client alone knows how the subagent ends: it settles the Task card itself
// when the subagent finishes, fails or is stopped.
func (service *Service) noteNativeSubagentBackgrounded(stream *ActiveStream, pending runtimecore.PendingExec, success *agentv1.SubagentSuccess) {
	if service == nil || stream == nil {
		return
	}
	// No exec is awaited any more, so the delegation's slot is free and its
	// keep-alive and progress watchdog stop. The closing summary a finished
	// subagent leaves in its Task card is not written: this one is running.
	service.settleNativeDelegation(pending.ExecID, delegation.TaskCompleted, "Cursor 子代理已转入后台运行", "", false)
	service.recordDelegationRunBackgrounded(stream, pending)
	logger.Infof("forwarder native subagent moved to background request_id=%s conversation_id=%s exec_id=%s tool_call_id=%s agent_id=%s reason=%s",
		strings.TrimSpace(stream.RequestID), strings.TrimSpace(stream.ConversationID), strings.TrimSpace(pending.ExecID), strings.TrimSpace(pending.ToolCallID),
		strings.TrimSpace(success.GetAgentId()), success.GetBackgroundReason().String())
	if service.debug != nil {
		service.debug.LogRuntime(context.Background(), stream.RequestID, stream.ConversationID, "native_subagent_backgrounded", map[string]any{
			"exec_id":           strings.TrimSpace(pending.ExecID),
			"tool_call_id":      strings.TrimSpace(pending.ToolCallID),
			"agent_id":          strings.TrimSpace(success.GetAgentId()),
			"background_reason": success.GetBackgroundReason().String(),
		})
	}
}

// recordDelegationRunBackgrounded keeps the subagent's run in the checkpoint
// as BACKGROUNDED. The client reads the run's status before anything else it
// knows about a Task card, so a SUCCESS here shows the card as completed the
// moment the subagent is launched.
func (service *Service) recordDelegationRunBackgrounded(stream *ActiveStream, pending runtimecore.PendingExec) {
	toolCallID := strings.TrimSpace(pending.ToolCallID)
	if stream == nil || toolCallID == "" {
		return
	}
	run := &agentv1.SubagentRunState{
		ParentToolCallId: toolCallID,
		// The same id the Task card and the RUNNING run carried while the
		// exec was pending.
		SubagentId:  stringPtr(delegationSubagentID(toolCallID)),
		Environment: agentv1.SubagentExecutionEnvironment_SUBAGENT_EXECUTION_ENVIRONMENT_LOCAL,
		Status:      agentv1.SubagentRunStatus_SUBAGENT_RUN_STATUS_BACKGROUNDED,
		Title:       stringPtr("Cursor 子代理"),
	}
	stream.mu.Lock()
	if stream.DelegationRunBackgrounded == nil {
		stream.DelegationRunBackgrounded = make(map[string]*agentv1.SubagentRunState)
	}
	stream.DelegationRunBackgrounded[toolCallID] = run
	delete(stream.DelegationRunProgress, toolCallID)
	stream.mu.Unlock()
}

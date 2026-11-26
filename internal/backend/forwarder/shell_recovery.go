package forwarder

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	"cursor/internal/logger"
)

const shellTerminalRecoveryGrace = 1500 * time.Millisecond

const (
	// shellClientLivenessWindow is how long the client may stay silent about
	// an exec it is still handling. It sends an exec heartbeat every three
	// seconds for as long as its handler runs, approval prompt included.
	shellClientLivenessWindow = 20 * time.Second
	// shellOverdueLimit bounds how far past its foreground window a started
	// shell may go without a terminal event while the client still heartbeats.
	shellOverdueLimit = 2 * time.Minute
	// shellRecoveryMinRecheck keeps a re-armed stall check from spinning.
	shellRecoveryMinRecheck = 250 * time.Millisecond
)

const (
	shellRecoveryReasonForegroundDeadline = "foreground_deadline_exceeded"
	shellRecoveryReasonTransportClosed    = "transport_closed_without_terminal"
)

func initializePendingExecForTracking(pending runtimecore.PendingExec) runtimecore.PendingExec {
	now := time.Now().UTC()
	if pending.OpenedAt.IsZero() {
		pending.OpenedAt = now
	}
	if strings.TrimSpace(pending.ExecKind) != "shell" {
		return pending
	}
	if pending.LastShellActivityAt.IsZero() {
		pending.LastShellActivityAt = pending.OpenedAt
	}
	if pending.ShellForegroundDeadline.IsZero() {
		pending.ShellForegroundDeadline = pending.OpenedAt.Add(shellForegroundTimeoutDuration(pending.ArgsJSON) + shellTerminalRecoveryGrace)
	}
	return pending
}

// shellForegroundTimeoutDuration is the foreground window the client was given
// for this shell: after it, the client backgrounds (or aborts) the command and
// reports a terminal event.
//
// Tool calls made by a BYOK model carry block_until_ms. A Cursor-account exec
// carries the client's own ShellArgs instead, where the window is `timeout`;
// reading only block_until_ms treated every such shell as a 30 second one.
func shellForegroundTimeoutDuration(argsJSON []byte) time.Duration {
	const fallback = 30 * time.Second
	args, err := runtimecore.DecodeArgsMap(argsJSON)
	if err != nil {
		return fallback
	}
	if blockUntilMS, found, err := runtimecore.ReadFloat64Arg(args, "block_until_ms", "blockUntilMS"); err == nil && found {
		if blockUntilMS <= 0 {
			return 0
		}
		return time.Duration(blockUntilMS) * time.Millisecond
	}
	if timeoutMS, found, err := runtimecore.ReadFloat64Arg(args, "timeout"); err == nil && found && timeoutMS > 0 {
		return time.Duration(timeoutMS) * time.Millisecond
	}
	if nativeShellStartsInBackground(args) {
		return 0
	}
	return fallback
}

// nativeShellStartsInBackground mirrors the client: with no timeout, a shell
// that is flagged background, backgrounds on timeout, or has a hard timeout is
// backgrounded as soon as it starts.
func nativeShellStartsInBackground(args map[string]any) bool {
	if runtimecore.ReadBoolArg(args, "isBackground", "is_background") {
		return true
	}
	switch behavior := args["timeoutBehavior"].(type) {
	case string:
		if behavior == agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND.String() {
			return true
		}
	case json.Number:
		if behavior.String() == strconv.Itoa(int(agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND)) {
			return true
		}
	}
	hardTimeout, found, err := runtimecore.ReadFloat64Arg(args, "hardTimeout", "hard_timeout")
	return err == nil && found && hardTimeout > 0
}

// shellClientLastHeardAt is the last time the client said anything about this
// shell: output, a lifecycle event or a heartbeat.
func shellClientLastHeardAt(pending runtimecore.PendingExec) time.Time {
	last := pending.OpenedAt
	if pending.LastShellActivityAt.After(last) {
		last = pending.LastShellActivityAt
	}
	if pending.LastShellHeartbeatAt.After(last) {
		last = pending.LastShellHeartbeatAt
	}
	return last
}

// shellForegroundRecoveryDelay reports how much longer to wait before a shell
// without a terminal event is closed with a synthetic result. Zero means now.
//
// The client's foreground window starts when the command starts running, not
// when it was requested: until the user approves a command the client holds it
// and only heartbeats. A synthetic result discards the real one, so a shell is
// only given up on once the window is over and the client has gone quiet, or
// once it is far past the window.
func shellForegroundRecoveryDelay(pending runtimecore.PendingExec, now time.Time) time.Duration {
	window := shellForegroundTimeoutDuration(pending.ArgsJSON) + shellTerminalRecoveryGrace
	quietAt := shellClientLastHeardAt(pending).Add(shellClientLivenessWindow)
	alive := now.Before(quietAt)

	if pending.FirstChunkAt.IsZero() {
		// Not started: waiting for approval, or the request never arrived.
		if alive {
			return quietAt.Sub(now)
		}
		if deadline := pending.OpenedAt.Add(window); now.Before(deadline) {
			return deadline.Sub(now)
		}
		return 0
	}
	deadline := pending.FirstChunkAt.Add(window)
	if now.Before(deadline) {
		return deadline.Sub(now)
	}
	if limit := deadline.Add(shellOverdueLimit); alive && now.Before(limit) {
		// The terminal event is due from a client that is still there.
		if untilLimit := limit.Sub(now); untilLimit < quietAt.Sub(now) {
			return untilLimit
		}
		return quietAt.Sub(now)
	}
	return 0
}

func shellForegroundTimeoutMS(argsJSON []byte) int64 {
	return shellForegroundTimeoutDuration(argsJSON).Milliseconds()
}

func (service *Service) scheduleShellForegroundRecovery(requestID string, pending runtimecore.PendingExec) {
	if service == nil || strings.TrimSpace(requestID) == "" || strings.TrimSpace(pending.ExecKind) != "shell" || strings.TrimSpace(pending.ExecID) == "" {
		return
	}
	stream, ok := service.broker.Get(requestID)
	if !ok || stream == nil {
		return
	}
	deadline := pending.ShellForegroundDeadline
	if deadline.IsZero() {
		deadline = time.Now().UTC().Add(shellForegroundTimeoutDuration(pending.ArgsJSON) + shellTerminalRecoveryGrace)
	}
	service.scheduleStreamTimer(
		stream,
		providerTimerKey(streamTimerShellForeground, pending.ExecID),
		time.Until(deadline),
		streamTimerShellForeground,
		pending.ExecID,
		pending.MessageID,
		shellRecoveryReasonForegroundDeadline,
	)
}

func (service *Service) scheduleShellTransportCloseRecovery(requestID string, pending runtimecore.PendingExec) {
	if service == nil || strings.TrimSpace(requestID) == "" || strings.TrimSpace(pending.ExecKind) != "shell" || strings.TrimSpace(pending.ExecID) == "" {
		return
	}
	stream, ok := service.broker.Get(requestID)
	if !ok || stream == nil {
		return
	}
	if _, scheduled := markShellRecoveryScheduled(stream, pending.ExecID); !scheduled {
		return
	}
	service.scheduleStreamTimer(
		stream,
		providerTimerKey(streamTimerShellTransportClose, pending.ExecID),
		shellTerminalRecoveryGrace,
		streamTimerShellTransportClose,
		pending.ExecID,
		pending.MessageID,
		shellRecoveryReasonTransportClosed,
	)
}

func snapshotPendingExecWithStatus(stream *ActiveStream, execID string) (runtimecore.PendingExec, StreamStatus, bool) {
	if stream == nil || strings.TrimSpace(execID) == "" {
		return runtimecore.PendingExec{}, "", false
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	item, ok := stream.PendingExecs[strings.TrimSpace(execID)]
	if !ok {
		return runtimecore.PendingExec{}, stream.Status, false
	}
	return item, stream.Status, true
}

func markShellRecoveryScheduled(stream *ActiveStream, execID string) (runtimecore.PendingExec, bool) {
	if stream == nil || strings.TrimSpace(execID) == "" {
		return runtimecore.PendingExec{}, false
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	current, ok := stream.PendingExecs[strings.TrimSpace(execID)]
	if !ok || strings.TrimSpace(current.ExecKind) != "shell" || current.ShellRecoveryScheduled {
		return current, false
	}
	current.ShellRecoveryScheduled = true
	stream.PendingExecs[strings.TrimSpace(execID)] = current
	stream.UpdatedAt = time.Now().UTC()
	return current, true
}

func (service *Service) recoverShellWithoutTerminalIfNeeded(stream *ActiveStream, execID string, messageID uint32, reason string) error {
	if stream == nil || strings.TrimSpace(execID) == "" {
		return nil
	}
	current, status, found := snapshotPendingExecWithStatus(stream, execID)
	if !found || current.MessageID != messageID || strings.TrimSpace(current.ExecKind) != "shell" || isTerminalStreamStatus(status) {
		return nil
	}
	switch strings.TrimSpace(current.StreamState) {
	case "exited", "backgrounded", "rejected", "permission_denied":
		return nil
	}
	if reason == shellRecoveryReasonForegroundDeadline {
		if wait := shellForegroundRecoveryDelay(current, time.Now().UTC()); wait > 0 {
			// Not stalled yet. Look again once it could be.
			if wait < shellRecoveryMinRecheck {
				wait = shellRecoveryMinRecheck
			}
			service.scheduleStreamTimer(
				stream,
				providerTimerKey(streamTimerShellForeground, current.ExecID),
				wait,
				streamTimerShellForeground,
				current.ExecID,
				current.MessageID,
				shellRecoveryReasonForegroundDeadline,
			)
			return nil
		}
	}
	return service.recoverShellWithoutTerminal(stream, current, reason)
}

func (service *Service) recoverShellWithoutTerminal(stream *ActiveStream, pending runtimecore.PendingExec, reason string) error {
	if stream == nil {
		return nil
	}
	pending.ShellRecoveryScheduled = true
	markExecCompleted(stream, pending)
	// 合成结果注入属于「工具假成功」：标记 degraded，由下一次 provider_call 记录 usage 时消费（token_usage.go）。
	stream.mu.Lock()
	stream.ShellSyntheticRecoveryInTurn = true
	stream.UpdatedAt = time.Now().UTC()
	stream.mu.Unlock()
	resultPayload := buildSyntheticShellResultPayload(pending, reason)
	logger.Infof(
		"forwarder synthetic shell recovery request_id=%s tool_call_id=%s message_id=%d exec_id=%s reason=%s stream_state=%s",
		strings.TrimSpace(stream.RequestID),
		strings.TrimSpace(pending.ToolCallID),
		pending.MessageID,
		strings.TrimSpace(pending.ExecID),
		strings.TrimSpace(reason),
		strings.TrimSpace(pending.StreamState),
	)
	if err := service.appendToolResult(stream, pending.ToolCallID, deriveToolNameFromPendingExec(pending), pending.ArgsJSON, resultPayload, pending.ReasoningContent, nil); err != nil {
		return err
	}
	if _, err := service.appendConversationEntries(stream, stream.ConversationID, []HistoryEntry{
		newMetadataEntry(stream.TurnSeq, stream.RequestID, "shell_stream_stalled", map[string]any{
			"tool_call_id":             pending.ToolCallID,
			"message_id":               pending.MessageID,
			"exec_id":                  pending.ExecID,
			"exec_kind":                pending.ExecKind,
			"reason":                   strings.TrimSpace(reason),
			"recent_stream_state":      pending.StreamState,
			"chunk_count":              pending.ChunkCount,
			"first_chunk_at":           pending.FirstChunkAt,
			"last_activity_at":         pending.LastShellActivityAt,
			"last_heartbeat_at":        pending.LastShellHeartbeatAt,
			"foreground_deadline":      pending.ShellForegroundDeadline,
			"timeout_ms":               shellForegroundTimeoutMS(pending.ArgsJSON),
			"stdout_buffer_bytes":      len(pending.StdoutBuffer),
			"stderr_buffer_bytes":      len(pending.StderrBuffer),
			"shell_recovery_scheduled": pending.ShellRecoveryScheduled,
		}),
	}); err != nil {
		return err
	}
	service.deferSummaryCarryForward(stream.ConversationID, stream.RequestID, pending.ModelCallID)
	if err := service.publishToolCallCompleted(stream.RequestID, pending.ToolCallID, pending.ModelCallID, nil); err != nil {
		return err
	}
	if err := service.publishCheckpoint(stream.RequestID, stream.ConversationID); err != nil {
		return err
	}
	service.deliverCapturedShellToCursorAccount(stream, pending, reason)
	return service.reconcileStream(stream)
}

// deliverCapturedShellToCursorAccount closes the upstream shell with the output
// already captured. reconcileStream does nothing while that provider stream is
// still open, so without this send the model never sees the text.
func (service *Service) deliverCapturedShellToCursorAccount(stream *ActiveStream, pending runtimecore.PendingExec, reason string) {
	if service == nil || stream == nil {
		return
	}
	accountPending := pending
	note := "shell ended without an exit event (" + strings.TrimSpace(reason) + "); this is the output captured so far"
	if strings.TrimSpace(accountPending.StdoutBuffer) == "" {
		accountPending.StdoutBuffer = note
	} else {
		accountPending.StdoutBuffer += "\n" + note
	}
	exitMsg := &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_ShellStream{
			ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: 0}},
			},
		},
	}
	_ = service.answerCursorAccountShellTerminal(stream, accountPending, exitMsg)
}

func buildSyntheticShellResultPayload(pending runtimecore.PendingExec, reason string) string {
	sections := make([]string, 0, 2)
	if captured := summarizeCapturedShellOutput(pending.StdoutBuffer, pending.StderrBuffer); captured != "" {
		sections = append(sections, captured)
	}
	noteLines := []string{
		"<shell-incomplete>",
		"Missing terminal shell stream event (expected exit or backgrounded).",
	}
	switch strings.TrimSpace(reason) {
	case shellRecoveryReasonTransportClosed:
		noteLines = append(noteLines, "The shell transport closed before a terminal event arrived.")
	case shellRecoveryReasonForegroundDeadline:
		noteLines = append(noteLines, fmt.Sprintf("The foreground wait window expired after %dms without a terminal event.", shellForegroundTimeoutMS(pending.ArgsJSON)))
	default:
		noteLines = append(noteLines, "The shell stream stopped progressing before a terminal event arrived.")
	}
	noteLines = append(noteLines,
		"The command may still be running in the Cursor app client.",
		"</shell-incomplete>",
	)
	sections = append(sections, strings.Join(noteLines, "\n"))
	return strings.Join(sections, "\n\n")
}

func summarizeCapturedShellOutput(stdout string, stderr string) string {
	trimmedStdout := strings.TrimSpace(stdout)
	trimmedStderr := strings.TrimSpace(stderr)
	sections := make([]string, 0, 2)
	if trimmedStdout != "" {
		sections = append(sections, trimmedStdout)
	}
	if trimmedStderr != "" {
		if trimmedStdout != "" {
			sections = append(sections, "<stderr>\n"+trimmedStderr+"\n</stderr>")
		} else {
			sections = append(sections, trimmedStderr)
		}
	}
	return strings.Join(sections, "\n\n")
}

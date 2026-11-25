package forwarder

import (
	"testing"
	"time"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
)

// A shell that was closed synthetically keeps running in the IDE. Its real
// events arrive whenever the command gets around to them -- seconds or minutes
// later -- and must be dropped quietly. Returning an error here fails the
// whole turn ("pending exec not found").

func newLateResultStream(t *testing.T) (*Service, *ActiveStream) {
	t.Helper()
	broker := NewStreamBroker()
	stream, err := broker.OpenStream("req-late-result", "conv-late-result", 1, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "build it")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	setupCheckpointConversation(stream)
	stream.mu.Lock()
	stream.TimerTokens = make(map[string]uint64)
	stream.StreamTimers = make(map[string]*time.Timer)
	stream.mu.Unlock()
	return newTestService(broker, func() time.Duration { return time.Second }, nil), stream
}

func addPendingShell(stream *ActiveStream, execID string, messageID uint32) runtimecore.PendingExec {
	pending := runtimecore.PendingExec{
		MessageID:   messageID,
		ExecID:      execID,
		ExecKind:    "shell",
		ToolCallID:  "call-" + execID,
		ModelCallID: "mc-late",
		ArgsJSON:    []byte(`{"command":"npm run build","block_until_ms":30000}`),
		StreamState: "streaming",
		OpenedAt:    time.Now().UTC().Add(-40 * time.Second),
	}
	stream.mu.Lock()
	stream.PendingExecs[execID] = pending
	stream.mu.Unlock()
	return pending
}

// ageCompletedExecs makes every completion record look old, as it does when
// the late event shows up well after the exec was closed.
func ageCompletedExecs(stream *ActiveStream, age time.Duration) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	for messageID := range stream.RecentCompletedExecs {
		stream.RecentCompletedExecs[messageID] = time.Now().UTC().Add(-age)
	}
}

func shellExitMessage(execID string, messageID uint32, code uint32) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{
		Id:     messageID,
		ExecId: execID,
		Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{
			Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: code}},
		}},
	}
}

func TestLateShellExitLongAfterSyntheticRecoveryIsIgnored(t *testing.T) {
	service, stream := newLateResultStream(t)
	pending := addPendingShell(stream, "exec-native-1", 58)

	if err := service.recoverShellWithoutTerminal(stream, pending, shellRecoveryReasonForegroundDeadline); err != nil {
		t.Fatalf("recoverShellWithoutTerminal() error = %v", err)
	}
	resultsAfterRecovery := countHistoryByKind(stream, "tool_result")
	ageCompletedExecs(stream, 26*time.Second)

	err := service.handleExecResult(InboundIntent{
		Kind:              "exec_result",
		RequestID:         "req-late-result",
		ExecClientMessage: shellExitMessage("exec-native-1", 58, 0),
	})

	if err != nil {
		t.Fatalf("late shell exit returned %q; that error fails the whole turn", err)
	}
	if got := countHistoryByKind(stream, "tool_result"); got != resultsAfterRecovery {
		t.Fatalf("tool_result entries = %d, want %d: the late exit must not add a second result", got, resultsAfterRecovery)
	}
}

func TestLateShellOutputLongAfterSyntheticRecoveryIsIgnored(t *testing.T) {
	service, stream := newLateResultStream(t)
	pending := addPendingShell(stream, "exec-native-2", 59)
	if err := service.recoverShellWithoutTerminal(stream, pending, shellRecoveryReasonForegroundDeadline); err != nil {
		t.Fatalf("recoverShellWithoutTerminal() error = %v", err)
	}
	ageCompletedExecs(stream, 3*time.Minute)

	err := service.handleExecResult(InboundIntent{
		Kind:      "exec_result",
		RequestID: "req-late-result",
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id:     59,
			ExecId: "exec-native-2",
			Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: "still building\n"}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("late shell output returned %q; that error fails the whole turn", err)
	}
}

func TestLateThrowLongAfterCompletionIsIgnored(t *testing.T) {
	service, stream := newLateResultStream(t)
	pending := addPendingShell(stream, "exec-shell-3", 60)
	markExecCompleted(stream, pending)
	ageCompletedExecs(stream, time.Minute)

	err := service.handleExecControl(InboundIntent{
		Kind:      "exec_control",
		RequestID: "req-late-result",
		ExecClientControlMessage: &agentv1.ExecClientControlMessage{
			Message: &agentv1.ExecClientControlMessage_Throw{Throw: &agentv1.ExecClientThrow{Id: 60, Error: "aborted"}},
		},
	})
	if err != nil {
		t.Fatalf("late throw for a finished exec returned %q; that error fails the whole turn", err)
	}
}

// Canceling a turn (or starting a new one on the same stream) drops every
// pending exec at once. The IDE still answers the aborts.
func TestLateResultAfterPendingExecsWereDroppedIsIgnored(t *testing.T) {
	service, stream := newLateResultStream(t)
	addPendingShell(stream, "exec-shell-4", 61)

	cleanupAllPendingExecs(stream)

	err := service.handleExecResult(InboundIntent{
		Kind:              "exec_result",
		RequestID:         "req-late-result",
		ExecClientMessage: shellExitMessage("exec-shell-4", 61, 130),
	})
	if err != nil {
		t.Fatalf("result for an exec dropped by cancel returned %q; that error fails the next turn", err)
	}
	if got := countHistoryByKind(stream, "tool_result"); got != 0 {
		t.Fatalf("tool_result entries = %d, want 0", got)
	}
}

// A result for an exec this stream never tracked cannot be acted on. It is a
// stray message, not a reason to end the user's turn.
func TestResultForUntrackedExecDoesNotFailTheTurn(t *testing.T) {
	service, stream := newLateResultStream(t)
	live := addPendingShell(stream, "exec-shell-live", 70)

	err := service.handleExecResult(InboundIntent{
		Kind:              "exec_result",
		RequestID:         "req-late-result",
		ExecClientMessage: shellExitMessage("exec-from-nowhere", 999, 0),
	})
	if err != nil {
		t.Fatalf("stray exec result returned %q; that error fails the whole turn", err)
	}
	if _, stillPending := snapshotPendingExec(stream, live.ExecID); !stillPending {
		t.Fatal("a stray result must not complete an unrelated pending exec")
	}
	if got := countHistoryByKind(stream, "tool_result"); got != 0 {
		t.Fatalf("tool_result entries = %d, want 0", got)
	}
}

// Retired exec ids are remembered by id, not only by the numeric message id:
// the client may resend with the id zeroed.
func TestLateResultMatchedByExecIDWhenMessageIDIsZero(t *testing.T) {
	service, stream := newLateResultStream(t)
	pending := addPendingShell(stream, "exec-native-5", 62)
	markExecCompleted(stream, pending)
	ageCompletedExecs(stream, time.Minute)

	err := service.handleExecResult(InboundIntent{
		Kind:              "exec_result",
		RequestID:         "req-late-result",
		ExecClientMessage: shellExitMessage("exec-native-5", 0, 0),
	})
	if err != nil {
		t.Fatalf("late result with a zero message id returned %q", err)
	}
}

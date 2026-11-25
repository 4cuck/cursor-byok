package forwarder

import (
	"testing"
	"time"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/encoding/protojson"
)

// nativeShellArgsJSON renders ShellArgs the way a Cursor-account exec reaches
// the forwarder: the client's own message, as protojson.
func nativeShellArgsJSON(t *testing.T, args *agentv1.ShellArgs) []byte {
	t.Helper()
	raw, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(args)
	if err != nil {
		t.Fatalf("marshal ShellArgs: %v", err)
	}
	return raw
}

func TestShellForegroundTimeoutReadsTheWindowTheClientWasGiven(t *testing.T) {
	hardTimeout := int32(86_400_000)
	cases := []struct {
		name string
		args []byte
		want time.Duration
	}{
		{"tool call with block_until_ms", []byte(`{"command":"npm test","block_until_ms":45000}`), 45 * time.Second},
		{"tool call without block_until_ms", []byte(`{"command":"npm test"}`), 30 * time.Second},
		{"tool call started in the background", []byte(`{"command":"npm run dev","block_until_ms":0}`), 0},
		{"cursor account shell with a two minute window", nativeShellArgsJSON(t, &agentv1.ShellArgs{
			Command:         "npm run build",
			Timeout:         120_000,
			ToolCallId:      "toolu_1",
			TimeoutBehavior: agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND,
			HardTimeout:     &hardTimeout,
		}), 2 * time.Minute},
		{"cursor account shell started in the background", nativeShellArgsJSON(t, &agentv1.ShellArgs{
			Command:         "npm run dev",
			ToolCallId:      "toolu_2",
			TimeoutBehavior: agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND,
			HardTimeout:     &hardTimeout,
		}), 0},
		{"cursor account shell flagged background", nativeShellArgsJSON(t, &agentv1.ShellArgs{
			Command:      "npm run dev",
			ToolCallId:   "toolu_3",
			IsBackground: true,
		}), 0},
		{"unreadable arguments fall back to the default", []byte(`not json`), 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellForegroundTimeoutDuration(tc.args); got != tc.want {
				t.Fatalf("shellForegroundTimeoutDuration(%s) = %s, want %s", tc.args, got, tc.want)
			}
		})
	}
}

func TestShellForegroundRecoveryDelay(t *testing.T) {
	now := time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)
	thirtySeconds := []byte(`{"command":"npm run build","block_until_ms":30000}`)

	t.Run("waiting for approval while the client heartbeats is not a stall", func(t *testing.T) {
		pending := runtimecore.PendingExec{
			ExecKind:             "shell",
			ArgsJSON:             thirtySeconds,
			OpenedAt:             now.Add(-5 * time.Minute),
			LastShellActivityAt:  now.Add(-2 * time.Second),
			LastShellHeartbeatAt: now.Add(-2 * time.Second),
		}
		if wait := shellForegroundRecoveryDelay(pending, now); wait <= 0 {
			t.Fatal("a shell the client is still holding for approval was declared stalled")
		}
	})

	t.Run("never started and the client went quiet", func(t *testing.T) {
		pending := runtimecore.PendingExec{
			ExecKind:            "shell",
			ArgsJSON:            thirtySeconds,
			OpenedAt:            now.Add(-40 * time.Second),
			LastShellActivityAt: now.Add(-40 * time.Second),
		}
		if wait := shellForegroundRecoveryDelay(pending, now); wait != 0 {
			t.Fatalf("delay = %s, want 0: nothing was heard for 40s and the window has passed", wait)
		}
	})

	t.Run("never started, quiet, but still inside the window", func(t *testing.T) {
		pending := runtimecore.PendingExec{
			ExecKind:            "shell",
			ArgsJSON:            []byte(`{"command":"npm run build","block_until_ms":120000}`),
			OpenedAt:            now.Add(-40 * time.Second),
			LastShellActivityAt: now.Add(-40 * time.Second),
		}
		wait := shellForegroundRecoveryDelay(pending, now)
		if wait < 80*time.Second || wait > 82*time.Second {
			t.Fatalf("delay = %s, want the rest of the 120s window (+grace)", wait)
		}
	})

	t.Run("the window starts when the command starts, not when it was requested", func(t *testing.T) {
		pending := runtimecore.PendingExec{
			ExecKind:            "shell",
			ArgsJSON:            thirtySeconds,
			OpenedAt:            now.Add(-2 * time.Minute), // approval took a while
			FirstChunkAt:        now.Add(-10 * time.Second),
			LastShellActivityAt: now.Add(-10 * time.Second),
			StreamState:         "started",
		}
		wait := shellForegroundRecoveryDelay(pending, now)
		if wait < 20*time.Second || wait > 22*time.Second {
			t.Fatalf("delay = %s, want about 21s: 30s window + grace, 10s already used", wait)
		}
	})

	t.Run("window over and the client is silent", func(t *testing.T) {
		pending := runtimecore.PendingExec{
			ExecKind:            "shell",
			ArgsJSON:            thirtySeconds,
			OpenedAt:            now.Add(-70 * time.Second),
			FirstChunkAt:        now.Add(-65 * time.Second),
			LastShellActivityAt: now.Add(-30 * time.Second),
			StreamState:         "streaming",
		}
		if wait := shellForegroundRecoveryDelay(pending, now); wait != 0 {
			t.Fatalf("delay = %s, want 0", wait)
		}
	})

	t.Run("window just over while the client still heartbeats", func(t *testing.T) {
		pending := runtimecore.PendingExec{
			ExecKind:             "shell",
			ArgsJSON:             thirtySeconds,
			OpenedAt:             now.Add(-36 * time.Second),
			FirstChunkAt:         now.Add(-35 * time.Second),
			LastShellActivityAt:  now.Add(-1 * time.Second),
			LastShellHeartbeatAt: now.Add(-1 * time.Second),
			StreamState:          "streaming",
		}
		if wait := shellForegroundRecoveryDelay(pending, now); wait <= 0 {
			t.Fatal("the terminal event is on its way from a client that is alive; closing now discards the real result")
		}
	})

	t.Run("a client that heartbeats forever does not hold the turn forever", func(t *testing.T) {
		pending := runtimecore.PendingExec{
			ExecKind:             "shell",
			ArgsJSON:             thirtySeconds,
			OpenedAt:             now.Add(-10 * time.Minute),
			FirstChunkAt:         now.Add(-10 * time.Minute),
			LastShellActivityAt:  now.Add(-1 * time.Second),
			LastShellHeartbeatAt: now.Add(-1 * time.Second),
			StreamState:          "streaming",
		}
		if wait := shellForegroundRecoveryDelay(pending, now); wait != 0 {
			t.Fatalf("delay = %s, want 0: far past the window with no terminal event", wait)
		}
	})
}

func shellHeartbeat(messageID uint32) *agentv1.ExecClientControlMessage {
	return &agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_Heartbeat{Heartbeat: &agentv1.ExecClientHeartbeat{Id: messageID}},
	}
}

// The recovery timer fires at the original estimate. A shell whose client is
// alive must be left pending, with the check re-armed.
func TestShellForegroundTimerLeavesAHeartbeatingShellPending(t *testing.T) {
	service, stream := newLateResultStream(t)
	pending := initializePendingExecForTracking(runtimecore.PendingExec{
		MessageID:   81,
		ExecID:      "exec-native-approval",
		ExecKind:    "shell",
		ToolCallID:  "call-approval",
		ModelCallID: "mc-approval",
		ArgsJSON:    []byte(`{"command":"rm -rf build","block_until_ms":30000}`),
		StreamState: "opened",
		OpenedAt:    time.Now().UTC().Add(-45 * time.Second),
	})
	stream.mu.Lock()
	stream.PendingExecs[pending.ExecID] = pending
	stream.mu.Unlock()

	// The approval prompt is open; the client keeps reporting in.
	if err := service.handleExecControl(InboundIntent{
		Kind:                     "exec_control",
		RequestID:                "req-late-result",
		ExecClientControlMessage: shellHeartbeat(81),
	}); err != nil {
		t.Fatalf("handleExecControl(heartbeat) error = %v", err)
	}

	if err := service.recoverShellWithoutTerminalIfNeeded(stream, pending.ExecID, pending.MessageID, shellRecoveryReasonForegroundDeadline); err != nil {
		t.Fatalf("recoverShellWithoutTerminalIfNeeded() error = %v", err)
	}

	if _, stillPending := snapshotPendingExec(stream, pending.ExecID); !stillPending {
		t.Fatal("a shell waiting for the user's approval was closed with a synthetic result")
	}
	if got := countHistoryByKind(stream, "tool_result"); got != 0 {
		t.Fatalf("tool_result entries = %d, want 0", got)
	}
	stream.mu.Lock()
	_, rearmed := stream.StreamTimers[providerTimerKey(streamTimerShellForeground, pending.ExecID)]
	stopAllStreamTimersLocked(stream)
	stream.mu.Unlock()
	if !rearmed {
		t.Fatal("the stall check was not re-armed; a client that later goes quiet would hang the turn")
	}
}

func TestShellForegroundTimerRecoversASilentShell(t *testing.T) {
	service, stream := newLateResultStream(t)
	opened := time.Now().UTC().Add(-45 * time.Second)
	pending := initializePendingExecForTracking(runtimecore.PendingExec{
		MessageID:   82,
		ExecID:      "exec-native-silent",
		ExecKind:    "shell",
		ToolCallID:  "call-silent",
		ModelCallID: "mc-silent",
		ArgsJSON:    []byte(`{"command":"npm run build","block_until_ms":30000}`),
		StreamState: "opened",
		OpenedAt:    opened,
	})
	stream.mu.Lock()
	stream.PendingExecs[pending.ExecID] = pending
	stream.mu.Unlock()

	if err := service.recoverShellWithoutTerminalIfNeeded(stream, pending.ExecID, pending.MessageID, shellRecoveryReasonForegroundDeadline); err != nil {
		t.Fatalf("recoverShellWithoutTerminalIfNeeded() error = %v", err)
	}

	if _, stillPending := snapshotPendingExec(stream, pending.ExecID); stillPending {
		t.Fatal("a shell the client never reported on is still pending 45s into a 30s window")
	}
	if got := countHistoryByKind(stream, "tool_result"); got != 1 {
		t.Fatalf("tool_result entries = %d, want the synthetic result", got)
	}
}

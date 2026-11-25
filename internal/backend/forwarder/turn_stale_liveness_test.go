package forwarder

import (
	"testing"
	"time"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
)

// newWaitingExternalStream is a turn whose provider pass is over and which is
// waiting for the client to finish its tools, already past the stale-turn
// threshold and inside the grace period: the next watchdog tick force-completes
// whatever is still pending.
func newWaitingExternalStream(t *testing.T, requestID string) (*Service, *ActiveStream) {
	t.Helper()
	broker := NewStreamBroker()
	stream, err := broker.OpenStream(requestID, "conv-"+requestID, 1, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "run the build")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	setupCheckpointConversation(stream)
	stream.mu.Lock()
	stream.Phase = TurnPhaseWaitingExternal
	stream.Status = StreamStatusStreaming
	stream.TurnStaleGraceStartedAt = time.Now().UTC().Add(-time.Minute)
	stream.TimerTokens = make(map[string]uint64)
	stream.StreamTimers = make(map[string]*time.Timer)
	stream.mu.Unlock()
	t.Cleanup(func() {
		stream.mu.Lock()
		stopAllStreamTimersLocked(stream)
		stream.mu.Unlock()
	})
	return newTestService(broker, func() time.Duration { return time.Second }, nil), stream
}

func turnStaleTick(t *testing.T, service *Service, stream *ActiveStream) {
	t.Helper()
	if err := service.handleTurnStaleTimeout(stream, &streamTimerEvent{Kind: streamTimerTurnStale}); err != nil {
		t.Fatalf("handleTurnStaleTimeout() error = %v", err)
	}
}

// A ten minute build in the foreground is not a stale turn. The client reports
// in every few seconds for as long as it is running the command; closing the
// shell with a fake timeout after three minutes tells the model the build
// failed while it is still going.
func TestTurnStaleWatchdogLeavesAnExecTheClientIsStillRunning(t *testing.T) {
	service, stream := newWaitingExternalStream(t, "req-stale-alive")
	pending := initializePendingExecForTracking(runtimecore.PendingExec{
		MessageID:   91,
		ExecID:      "exec-shell-long-build",
		ExecKind:    "shell",
		ToolCallID:  "call-long-build",
		ModelCallID: "mc-stale",
		ArgsJSON:    []byte(`{"command":"npm run build","block_until_ms":600000}`),
		StreamState: "streaming",
		OpenedAt:    time.Now().UTC().Add(-3 * time.Minute),
	})
	pending.FirstChunkAt = pending.OpenedAt
	stream.mu.Lock()
	stream.PendingExecs[pending.ExecID] = pending
	stream.mu.Unlock()

	if err := service.handleExecControl(InboundIntent{
		Kind:                     "exec_control",
		RequestID:                "req-stale-alive",
		ExecClientControlMessage: shellHeartbeat(91),
	}); err != nil {
		t.Fatalf("handleExecControl(heartbeat) error = %v", err)
	}

	turnStaleTick(t, service, stream)

	if _, stillPending := snapshotPendingExec(stream, pending.ExecID); !stillPending {
		t.Fatal("a shell the client is still running was force-completed as a stale turn")
	}
	if got := countHistoryByKind(stream, "tool_result"); got != 0 {
		t.Fatalf("tool_result entries = %d, want 0", got)
	}
	stream.mu.Lock()
	grace := stream.TurnStaleGraceStartedAt
	_, rearmed := stream.StreamTimers[providerTimerKey(streamTimerTurnStale, "")]
	stream.mu.Unlock()
	if !grace.IsZero() {
		t.Fatal("the grace period was left running; the next tick would force-complete the shell")
	}
	if !rearmed {
		t.Fatal("the stale-turn watchdog was not re-armed")
	}
}

// The same holds for any tool the client executes, not only shells: a slow MCP
// call heartbeats too.
func TestTurnStaleWatchdogLeavesAHeartbeatingNonShellExec(t *testing.T) {
	service, stream := newWaitingExternalStream(t, "req-stale-mcp")
	pending := initializePendingExecForTracking(runtimecore.PendingExec{
		MessageID:   92,
		ExecID:      "exec-mcp-slow",
		ExecKind:    "mcp",
		ToolCallID:  "call-mcp-slow",
		ModelCallID: "mc-stale",
		ArgsJSON:    []byte(`{}`),
		StreamState: "opened",
		OpenedAt:    time.Now().UTC().Add(-3 * time.Minute),
	})
	stream.mu.Lock()
	stream.PendingExecs[pending.ExecID] = pending
	stream.mu.Unlock()

	if err := service.handleExecControl(InboundIntent{
		Kind:                     "exec_control",
		RequestID:                "req-stale-mcp",
		ExecClientControlMessage: shellHeartbeat(92),
	}); err != nil {
		t.Fatalf("handleExecControl(heartbeat) error = %v", err)
	}

	turnStaleTick(t, service, stream)

	if _, stillPending := snapshotPendingExec(stream, pending.ExecID); !stillPending {
		t.Fatal("an MCP call the client is still running was force-completed as a stale turn")
	}
}

// When the client has gone quiet the watchdog still does its job.
func TestTurnStaleWatchdogForceCompletesASilentExec(t *testing.T) {
	service, stream := newWaitingExternalStream(t, "req-stale-silent")
	pending := initializePendingExecForTracking(runtimecore.PendingExec{
		MessageID:   93,
		ExecID:      "exec-read-lost",
		ExecKind:    "read",
		ToolCallID:  "call-read-lost",
		ModelCallID: "mc-stale",
		ArgsJSON:    []byte(`{"path":"README.md"}`),
		StreamState: "opened",
		OpenedAt:    time.Now().UTC().Add(-3 * time.Minute),
	})
	stream.mu.Lock()
	stream.PendingExecs[pending.ExecID] = pending
	stream.mu.Unlock()

	turnStaleTick(t, service, stream)

	if _, stillPending := snapshotPendingExec(stream, pending.ExecID); stillPending {
		t.Fatal("an exec the client never answered is still pending after the grace period")
	}
	if got := countHistoryByKind(stream, "tool_result"); got != 1 {
		t.Fatalf("tool_result entries = %d, want the synthetic result", got)
	}
}

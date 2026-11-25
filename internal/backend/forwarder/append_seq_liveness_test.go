package forwarder

import (
	"context"
	"testing"
	"time"

	"cursor/gen/agentv1"
)

func ageAppendSequenceStates(tracker *appendSequenceTracker, age time.Duration) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for _, state := range tracker.states {
		state.mu.Lock()
		state.updatedAt = time.Now().UTC().Add(-age)
		state.mu.Unlock()
	}
}

func processAppend(t *testing.T, tracker *appendSequenceTracker, requestID string, seq int64) {
	t.Helper()
	ticket, stale, err := tracker.Acquire(context.Background(), requestID, seq)
	if err != nil || stale {
		t.Fatalf("Acquire(%s, %d) error=%v stale=%t, want accepted", requestID, seq, err, stale)
	}
	ticket.Release()
}

// The tracker remembers which frame of a request comes next. It used to
// forget that after ten quiet minutes, whether or not the request was still
// running: a turn waiting on the user while the machine slept, or a long
// detached task. The next frame then carried a number the fresh state was not
// expecting, and it -- and every frame after it -- waited for frame 1 until
// the proxy gave up five minutes later.
func TestAppendSeqKeepsTheStateOfARunningRequestHoweverLongItIsQuiet(t *testing.T) {
	tracker := newAppendSequenceTracker()
	tracker.live = func(requestID string) bool { return requestID == "req-running" }
	processAppend(t, tracker, "req-running", 1)
	processAppend(t, tracker, "req-running", 2)
	ageAppendSequenceStates(tracker, appendSequenceRetention+time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ticket, stale, err := tracker.Acquire(ctx, "req-running", 3)

	if err != nil {
		t.Fatalf("Acquire(seq=3) after a long quiet spell = %v; the frame is waiting for a frame 1 that was processed long ago", err)
	}
	if stale {
		t.Fatal("Acquire(seq=3) returned stale, want it accepted as the next frame")
	}
	ticket.Release()
}

// A frame that was already processed is still recognized as a repeat.
func TestAppendSeqStillDropsRepeatsOfARunningRequestAfterAQuietSpell(t *testing.T) {
	tracker := newAppendSequenceTracker()
	tracker.live = func(string) bool { return true }
	processAppend(t, tracker, "req-running", 1)
	ageAppendSequenceStates(tracker, appendSequenceRetention+time.Minute)

	_, stale, err := tracker.Acquire(context.Background(), "req-running", 1)

	if err != nil || !stale {
		t.Fatalf("repeat of frame 1: error=%v stale=%t, want it recognized as already processed", err, stale)
	}
}

// Requests that are over are still forgotten, so the tracker does not grow.
func TestAppendSeqForgetsFinishedRequests(t *testing.T) {
	tracker := newAppendSequenceTracker()
	tracker.live = func(requestID string) bool { return requestID == "req-running" }
	processAppend(t, tracker, "req-running", 1)
	processAppend(t, tracker, "req-finished", 1)
	ageAppendSequenceStates(tracker, appendSequenceRetention+time.Minute)

	processAppend(t, tracker, "req-new", 1)

	tracker.mu.Lock()
	_, keptRunning := tracker.states["req-running"]
	_, keptFinished := tracker.states["req-finished"]
	tracker.mu.Unlock()
	if !keptRunning {
		t.Fatal("the state of a running request was forgotten")
	}
	if keptFinished {
		t.Fatal("the state of a finished request was kept after the retention period")
	}
}

// The service tells the tracker which requests are running: those whose
// stream has not ended.
func TestServiceKeepsAppendSequenceOfAnOpenStream(t *testing.T) {
	broker := NewStreamBroker()
	service := newServiceWithDependencies(NewConversationFileStore(t.TempDir()), NewHistoryProjector(), nil, nil, broker)
	if _, err := broker.OpenStream("req-open", "conv-open", 1, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "hello"); err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	processAppend(t, service.appendSeq, "req-open", 1)
	processAppend(t, service.appendSeq, "req-unknown", 1)
	ageAppendSequenceStates(service.appendSeq, appendSequenceRetention+time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ticket, stale, err := service.appendSeq.Acquire(ctx, "req-open", 2)
	if err != nil || stale {
		t.Fatalf("Acquire(req-open, 2) error=%v stale=%t, want the open stream's next frame accepted", err, stale)
	}
	ticket.Release()

	service.appendSeq.mu.Lock()
	_, keptUnknown := service.appendSeq.states["req-unknown"]
	service.appendSeq.mu.Unlock()
	if keptUnknown {
		t.Fatal("the state of a request without a stream was kept after the retention period")
	}
}

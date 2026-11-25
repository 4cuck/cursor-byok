package forwarder

import (
	"fmt"
	"testing"
	"time"

	"cursor/gen/agentv1"
)

const checkpointRateTestRequestID = "req-checkpoint-rate"

// newCheckpointRateStream builds a stream whose conversation is long enough
// that walking it is clearly more work than a constant-time check.
func newCheckpointRateStream(t *testing.T, entryCount int) (*Service, *ActiveStream) {
	t.Helper()
	broker := NewStreamBroker()
	stream, err := broker.OpenStream(checkpointRateTestRequestID, "conv-checkpoint-rate", 1, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "long conversation")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	setupCheckpointConversation(stream)
	entries := make([]HistoryEntry, 0, entryCount)
	for i := 0; i < entryCount; i++ {
		entries = append(entries, newAssistantTextEntry(1, checkpointRateTestRequestID, fmt.Sprintf("assistant message number %d with some text in it", i), "", ""))
	}
	stream.mu.Lock()
	appendEntriesInPlace(stream.CheckpointConversation, entries)
	stream.mu.Unlock()
	t.Cleanup(func() {
		stream.mu.Lock()
		if stream.CheckpointPublishTimer != nil {
			stream.CheckpointPublishTimer.Stop()
		}
		stream.mu.Unlock()
	})
	return newTestService(broker, func() time.Duration { return time.Second }, nil), stream
}

func checkpointRevision(stream *ActiveStream) uint64 {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.NextCheckpointRevision
}

// Every tool dispatch and every tool result asks for a checkpoint. Inside the
// send interval the answer is always "not yet", and reaching that answer must
// not cost a full projection of the conversation: on real histories that is
// 50-150ms per call, on the goroutine that also forwards the model's output.
func TestRateLimitedCheckpointPublishDoesNotWalkTheHistory(t *testing.T) {
	service, stream := newCheckpointRateStream(t, 3000)

	if err := service.publishCheckpoint(checkpointRateTestRequestID, "conv-checkpoint-rate"); err != nil {
		t.Fatalf("first publishCheckpoint() error = %v", err)
	}
	sent := checkpointRevision(stream)
	if sent == 0 {
		t.Fatal("the first checkpoint was not queued")
	}

	allocs := testing.AllocsPerRun(20, func() {
		if err := service.publishCheckpoint(checkpointRateTestRequestID, "conv-checkpoint-rate"); err != nil {
			t.Fatalf("rate-limited publishCheckpoint() error = %v", err)
		}
	})

	if allocs > 100 {
		t.Fatalf("a rate-limited checkpoint publish made %.0f allocations for a 3000-entry conversation; it must decide without projecting the history", allocs)
	}
	if got := checkpointRevision(stream); got != sent {
		t.Fatalf("checkpoint revision moved from %d to %d inside the send interval", sent, got)
	}
}

// Deferring must not lose anything: what changed inside the interval goes out
// once the interval closes.
func TestRateLimitedCheckpointIsSentWhenTheIntervalCloses(t *testing.T) {
	service, stream := newCheckpointRateStream(t, 20)

	if err := service.publishCheckpoint(checkpointRateTestRequestID, "conv-checkpoint-rate"); err != nil {
		t.Fatalf("first publishCheckpoint() error = %v", err)
	}
	sent := checkpointRevision(stream)
	stream.mu.Lock()
	firstHash := stream.LastCheckpointWireHash
	appendEntriesInPlace(stream.CheckpointConversation, []HistoryEntry{
		newAssistantTextEntry(1, checkpointRateTestRequestID, "text that arrived inside the interval", "", ""),
	})
	stream.mu.Unlock()

	if err := service.publishCheckpoint(checkpointRateTestRequestID, "conv-checkpoint-rate"); err != nil {
		t.Fatalf("deferred publishCheckpoint() error = %v", err)
	}
	if got := checkpointRevision(stream); got != sent {
		t.Fatalf("checkpoint revision moved from %d to %d before the interval closed", sent, got)
	}

	deadline := time.Now().Add(checkpointMinSendInterval + 3*time.Second)
	for time.Now().Before(deadline) && checkpointRevision(stream) == sent {
		time.Sleep(10 * time.Millisecond)
	}
	if got := checkpointRevision(stream); got == sent {
		t.Fatal("the deferred checkpoint was never sent")
	}
	stream.mu.Lock()
	laterHash := stream.LastCheckpointWireHash
	stream.mu.Unlock()
	if laterHash == firstHash {
		t.Fatal("the deferred checkpoint does not contain the new entry")
	}
}

// A forced checkpoint (turn end, subagent status) is never deferred.
func TestForcedCheckpointIgnoresTheSendInterval(t *testing.T) {
	service, stream := newCheckpointRateStream(t, 20)

	if err := service.publishCheckpoint(checkpointRateTestRequestID, "conv-checkpoint-rate"); err != nil {
		t.Fatalf("first publishCheckpoint() error = %v", err)
	}
	sent := checkpointRevision(stream)

	if err := service.publishCheckpointForce(checkpointRateTestRequestID, "conv-checkpoint-rate"); err != nil {
		t.Fatalf("publishCheckpointForce() error = %v", err)
	}
	if got := checkpointRevision(stream); got == sent {
		t.Fatal("a forced checkpoint was held back by the send interval")
	}
}

package forwarder

import (
	"testing"
	"time"
)

// A checkpoint is a copy of the conversation state for the client. Its blobs
// are written to the client first, and the client acknowledges each one.
//
// When the turn is over, the turn's end waits for that checkpoint, so an
// acknowledgement that never comes has to end the turn explicitly. In the
// middle of a turn nothing waits for the checkpoint: a late acknowledgement
// only means this copy is not sent. It used to fail the whole turn
// ("checkpoint_sync_error") while the model and the tools were working.

func attachCheckpointClient(t *testing.T, service *Service, stream *ActiveStream) {
	t.Helper()
	if _, _, _, err := service.broker.Subscribe(stream.RequestID); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
}

func queueMidTurnCheckpoint(t *testing.T, service *Service, stream *ActiveStream, entries []HistoryEntry) {
	t.Helper()
	projection, err := service.projector.ProjectCheckpointProjection(testConversation(entries))
	if err != nil {
		t.Fatalf("ProjectCheckpointProjection() error = %v", err)
	}
	if err := service.queueCheckpointProjection(stream, projection, checkpointTerminalAction{kind: checkpointTerminalActionNone}); err != nil {
		t.Fatalf("queueCheckpointProjection() error = %v", err)
	}
}

type checkpointStreamOutcome struct {
	failed      bool
	ended       bool
	checkpoints int
	blobWrites  int
}

func readCheckpointStreamOutcome(t *testing.T, service *Service, stream *ActiveStream) checkpointStreamOutcome {
	t.Helper()
	events, err := service.broker.ReadFromCursor(stream.RequestID, 0)
	if err != nil {
		t.Fatalf("ReadFromCursor() error = %v", err)
	}
	outcome := checkpointStreamOutcome{}
	for _, event := range events {
		if event.End {
			outcome.ended = true
			if event.TerminalErrorCode == "checkpoint_sync_error" {
				outcome.failed = true
			}
		}
		if event.Message.GetConversationCheckpointUpdate() != nil {
			outcome.checkpoints++
		}
		if event.Message.GetKvServerMessage().GetSetBlobArgs() != nil {
			outcome.blobWrites++
		}
	}
	return outcome
}

func TestCheckpointBlobTimeoutMidTurnLeavesTheTurnRunning(t *testing.T) {
	service, stream := testCheckpointBlobService(t)
	attachCheckpointClient(t, service, stream)
	entries := []HistoryEntry{testUserMessageEntry(t, 1, "request-1", "hello")}
	queueMidTurnCheckpoint(t, service, stream, entries)
	firstWrites := readCheckpointStreamOutcome(t, service, stream).blobWrites
	if firstWrites == 0 {
		t.Fatal("test did not queue a blob write")
	}

	if err := service.handleCheckpointBlobTimeout(stream); err != nil {
		t.Fatalf("handleCheckpointBlobTimeout() error = %v", err)
	}

	outcome := readCheckpointStreamOutcome(t, service, stream)
	if outcome.failed || outcome.ended {
		t.Fatalf("a late blob acknowledgement in the middle of a turn ended the turn (failed=%t ended=%t)", outcome.failed, outcome.ended)
	}
	if outcome.checkpoints != 0 {
		t.Fatal("a checkpoint whose blobs were not acknowledged was published")
	}
	stream.mu.Lock()
	status := stream.Status
	stream.mu.Unlock()
	if isTerminalStreamStatus(status) {
		t.Fatalf("stream status = %s, want the turn still running", status)
	}

	// The next checkpoint writes the unacknowledged blobs again and, once the
	// client has them, is published.
	queueMidTurnCheckpoint(t, service, stream, entries)
	if writes := readCheckpointStreamOutcome(t, service, stream).blobWrites; writes <= firstWrites {
		t.Fatalf("blob writes = %d after the next checkpoint, want the unacknowledged blobs written again (was %d)", writes, firstWrites)
	}
	if err := acknowledgePendingCheckpointBlobs(service, stream); err != nil {
		t.Fatalf("acknowledge blob writes: %v", err)
	}
	if got := readCheckpointStreamOutcome(t, service, stream).checkpoints; got != 1 {
		t.Fatalf("published checkpoints = %d, want 1 once the blobs are acknowledged", got)
	}
}

// The dropped checkpoint was never sent, so an unchanged conversation must
// still be offered to the client again.
func TestCheckpointDroppedMidTurnIsNotRememberedAsSent(t *testing.T) {
	service, stream := testCheckpointBlobService(t)
	attachCheckpointClient(t, service, stream)
	stream.mu.Lock()
	stream.LastCheckpointWireHash = "hash-of-the-checkpoint-being-synced"
	stream.mu.Unlock()
	queueMidTurnCheckpoint(t, service, stream, []HistoryEntry{testUserMessageEntry(t, 1, "request-1", "hello")})

	if err := service.handleCheckpointBlobTimeout(stream); err != nil {
		t.Fatalf("handleCheckpointBlobTimeout() error = %v", err)
	}

	stream.mu.Lock()
	remembered := stream.LastCheckpointWireHash
	stream.mu.Unlock()
	if remembered != "" {
		t.Fatalf("LastCheckpointWireHash = %q; an identical checkpoint would be skipped as already sent", remembered)
	}
}

func TestCheckpointBlobRejectionMidTurnLeavesTheTurnRunning(t *testing.T) {
	service, stream := testCheckpointBlobService(t)
	attachCheckpointClient(t, service, stream)
	queueMidTurnCheckpoint(t, service, stream, []HistoryEntry{testUserMessageEntry(t, 1, "request-1", "hello")})
	stream.mu.Lock()
	var requestID uint32
	for pendingID := range stream.PendingCheckpointBlobWrites {
		requestID = pendingID
		break
	}
	stream.mu.Unlock()
	if requestID == 0 {
		t.Fatal("test did not queue a blob write")
	}

	if err := rejectCheckpointBlob(service, stream, requestID, "disk full"); err != nil {
		t.Fatalf("rejectCheckpointBlob() error = %v", err)
	}

	outcome := readCheckpointStreamOutcome(t, service, stream)
	if outcome.failed || outcome.ended {
		t.Fatalf("a rejected blob write in the middle of a turn ended the turn (failed=%t ended=%t)", outcome.failed, outcome.ended)
	}
	if outcome.checkpoints != 0 {
		t.Fatal("a checkpoint with a rejected blob was published")
	}
}

// At the end of a turn the checkpoint is what the turn's end waits for. With
// the client attached, a missing acknowledgement still has to end the turn,
// or it would stay in the checkpointing phase for good.
func TestCheckpointBlobTimeoutAtTurnEndStillEndsTheTurn(t *testing.T) {
	service, stream := testCheckpointBlobService(t)
	attachCheckpointClient(t, service, stream)
	projection, err := service.projector.ProjectCheckpointProjection(testConversation([]HistoryEntry{
		testUserMessageEntry(t, 1, stream.RequestID, "hello"),
	}))
	if err != nil {
		t.Fatalf("ProjectCheckpointProjection() error = %v", err)
	}
	completion := &pendingTurnCompletion{RequestID: stream.RequestID}
	if err := service.queueCheckpointProjection(stream, projection, checkpointCompletionAction(completion)); err != nil {
		t.Fatalf("queueCheckpointProjection() error = %v", err)
	}

	if err := service.handleCheckpointBlobTimeout(stream); err != nil {
		t.Fatalf("handleCheckpointBlobTimeout() error = %v", err)
	}

	if outcome := readCheckpointStreamOutcome(t, service, stream); !outcome.failed {
		t.Fatal("the turn was left waiting for a checkpoint that will not be acknowledged")
	}
}

// A newer message of the same conversation is waiting for this turn to end,
// and the client stopped acknowledging this turn's blobs: the client has moved
// on. Ending the turn is what lets the waiting message start.
func TestCheckpointBlobTimeoutMidTurnEndsATurnANewerRunIsWaitingBehind(t *testing.T) {
	service, stream := testCheckpointBlobService(t)
	attachCheckpointClient(t, service, stream)
	service.runQueue = newRunQueue()
	started := make(chan string, 1)
	service.startOwnedRunHook = func(intent InboundIntent) error {
		started <- intent.RequestID
		return nil
	}
	owner := InboundIntent{Kind: "run", RequestID: stream.RequestID, ConversationID: stream.ConversationID}
	if result, _, _ := service.runQueue.Submit(owner); result != runQueueStart {
		t.Fatalf("first run submit = %v, want it to start", result)
	}
	successor := InboundIntent{Kind: "run", RequestID: "request-2", ConversationID: stream.ConversationID}
	if result, _, _ := service.runQueue.Submit(successor); result != runQueueQueued {
		t.Fatalf("second run submit = %v, want it queued behind the first", result)
	}
	queueMidTurnCheckpoint(t, service, stream, []HistoryEntry{testUserMessageEntry(t, 1, "request-1", "hello")})

	if err := service.handleCheckpointBlobTimeout(stream); err != nil {
		t.Fatalf("handleCheckpointBlobTimeout() error = %v", err)
	}

	if outcome := readCheckpointStreamOutcome(t, service, stream); !outcome.failed {
		t.Fatal("the unattended turn was kept alive while a newer run of the conversation waits behind it")
	}
	select {
	case requestID := <-started:
		if requestID != "request-2" {
			t.Fatalf("started run = %q, want the waiting run request-2", requestID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiting run was not started after the turn ahead of it ended")
	}
}

// Nobody is attached to the stream at all: nothing changes, the turn ends.
func TestCheckpointBlobTimeoutMidTurnEndsATurnWithoutAClient(t *testing.T) {
	service, stream := testCheckpointBlobService(t)
	queueMidTurnCheckpoint(t, service, stream, []HistoryEntry{testUserMessageEntry(t, 1, "request-1", "hello")})

	if err := service.handleCheckpointBlobTimeout(stream); err != nil {
		t.Fatalf("handleCheckpointBlobTimeout() error = %v", err)
	}

	if outcome := readCheckpointStreamOutcome(t, service, stream); !outcome.failed {
		t.Fatal("a turn with no client attached was not ended on a blob timeout")
	}
}

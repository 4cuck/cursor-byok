package modeladapter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"cursor/gen/agentv1"
)

// cursorAccountAppendWindow is how many frames may be on their way beyond the
// oldest one the server has not acknowledged. The IDE's BidiTransport uses the
// same window: it numbers frames in order, sends them without waiting for one
// another, and lets the server put them back in order by sequence number.
const cursorAccountAppendWindow = 32

// cursorAccountAppendDrain bounds how long a stream that ended cleanly waits
// for frames that are still on their way before they are abandoned.
const cursorAccountAppendDrain = 3 * time.Second

// errCursorAccountFrameNotSent reports a frame that never went on the wire
// because an earlier frame of its stream was lost. The loss itself is reported
// once, by the frame that was lost; a frame that was merely not sent after it
// is not a second failure.
var errCursorAccountFrameNotSent = errors.New("cursor account frame was not sent after an earlier frame was lost")

// cursorAccountFrameSender delivers the client half of one RunSSE stream.
//
// Each frame is one BidiAppend round trip. Sending them one at a time made
// every reply wait for all the replies before it: the results of a batch of
// tool calls went out one round trip after another, and the stream reader
// stood still while a blob acknowledgement was in flight.
//
// Cursor's server acknowledges a frame as soon as it arrives and processes
// frames by sequence number, whatever order they arrive in, so frames can
// travel side by side.
type cursorAccountFrameSender struct {
	appendFrame cursorAccountAppendFunc

	mu       sync.Mutex
	nextSeq  int64              // next sequence number to hand out
	oldest   int64              // oldest sequence number not yet acknowledged
	ahead    map[int64]struct{} // acknowledged while an older frame was outstanding
	moved    chan struct{}      // closed whenever the window moves or the sender fails
	failed   error              // first lost frame; nothing is sent after it
	inFlight int                // batches handed over and not finished
	idle     chan struct{}      // closed while nothing is in flight
}

func newCursorAccountFrameSender(appendFrame cursorAccountAppendFunc) *cursorAccountFrameSender {
	idle := make(chan struct{})
	close(idle)
	return &cursorAccountFrameSender{
		appendFrame: appendFrame,
		// Sequence number 0 is the run request, which is sent on its own.
		nextSeq: 1,
		oldest:  1,
		ahead:   make(map[int64]struct{}),
		moved:   make(chan struct{}),
		idle:    idle,
	}
}

// send numbers the payloads in the order given and delivers them without
// waiting for frames handed over earlier. It never blocks. done, if not nil,
// is called exactly once: with nil after every payload was acknowledged, or
// with the first failure as soon as it happens. That failure is
// errCursorAccountFrameNotSent when the stream had already lost a frame.
func (sender *cursorAccountFrameSender) send(ctx context.Context, done func(error), payloads ...[]byte) {
	if len(payloads) == 0 {
		if done != nil {
			done(nil)
		}
		return
	}
	sender.mu.Lock()
	first := sender.nextSeq
	sender.nextSeq += int64(len(payloads))
	if sender.inFlight == 0 {
		sender.idle = make(chan struct{})
	}
	sender.inFlight++
	sender.mu.Unlock()

	go func() {
		outcomes := make(chan error, len(payloads))
		for index, payload := range payloads {
			go func(seq int64, payload []byte) {
				outcomes <- sender.deliver(ctx, seq, payload)
			}(first+int64(index), payload)
		}
		var firstErr error
		for range payloads {
			if err := <-outcomes; err != nil && firstErr == nil {
				firstErr = err
				// Report at once. The server can no longer process the rest
				// of the batch, so nobody should wait for it.
				if done != nil {
					done(err)
				}
			}
		}
		// Report before going idle: a stream waiting for its last frames must
		// see their outcome.
		if firstErr == nil && done != nil {
			done(nil)
		}
		sender.mu.Lock()
		sender.inFlight--
		if sender.inFlight == 0 {
			close(sender.idle)
		}
		sender.mu.Unlock()
	}()
}

func (sender *cursorAccountFrameSender) deliver(ctx context.Context, seq int64, payload []byte) error {
	if err := sender.waitUntilSendable(ctx, seq); err != nil {
		return err
	}
	if err := sender.appendFrame(ctx, seq, payload); err != nil {
		sender.fail(err)
		return err
	}
	sender.acknowledge(seq)
	return nil
}

// waitUntilSendable holds a frame back while it is a full window ahead of the
// oldest unacknowledged one, so the server never has to buffer more than a
// window of out-of-order frames.
func (sender *cursorAccountFrameSender) waitUntilSendable(ctx context.Context, seq int64) error {
	for {
		sender.mu.Lock()
		if sender.failed != nil {
			err := fmt.Errorf("%w: %w", errCursorAccountFrameNotSent, sender.failed)
			sender.mu.Unlock()
			return err
		}
		if seq < sender.oldest+cursorAccountAppendWindow {
			sender.mu.Unlock()
			return nil
		}
		moved := sender.moved
		sender.mu.Unlock()
		select {
		case <-moved:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (sender *cursorAccountFrameSender) acknowledge(seq int64) {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if seq != sender.oldest {
		sender.ahead[seq] = struct{}{}
		return
	}
	sender.oldest++
	for {
		if _, ok := sender.ahead[sender.oldest]; !ok {
			break
		}
		delete(sender.ahead, sender.oldest)
		sender.oldest++
	}
	close(sender.moved)
	sender.moved = make(chan struct{})
}

// fail records the first lost frame. The server processes frames in sequence,
// so everything numbered after a lost frame can never be processed; frames
// still waiting for the window are released with the same error.
func (sender *cursorAccountFrameSender) fail(err error) {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.failed != nil {
		return
	}
	sender.failed = err
	close(sender.moved)
	sender.moved = make(chan struct{})
}

// drain waits, up to limit, for the frames handed over so far.
func (sender *cursorAccountFrameSender) drain(limit time.Duration) {
	sender.mu.Lock()
	idle := sender.idle
	sender.mu.Unlock()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
	}
}

// cursorAccountStreamClosePayload is the control frame that closes a finished
// exec, as the IDE sends after every exec result.
func cursorAccountStreamClosePayload(id uint32) []byte {
	return mustCursorProto(&agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientControlMessage{
			ExecClientControlMessage: &agentv1.ExecClientControlMessage{
				Message: &agentv1.ExecClientControlMessage_StreamClose{
					StreamClose: &agentv1.ExecClientStreamClose{Id: id},
				},
			},
		},
	})
}

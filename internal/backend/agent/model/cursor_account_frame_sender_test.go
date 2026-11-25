package modeladapter

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// A stand-in for Cursor's append endpoint
// ---------------------------------------------------------------------------

type appendedFrame struct {
	seq int64
	msg *agentv1.AgentClientMessage
}

// appendRecorder is safe for concurrent appends. Frames may arrive in any
// order; the sequence number is what orders them.
type appendRecorder struct {
	mu          sync.Mutex
	frames      []appendedFrame
	entered     []int64
	inFlight    int
	maxInFlight int

	// delay is how long each append takes, as a round trip would.
	delay time.Duration
	// hold, when set, returns a channel that must close before the append for
	// this frame completes.
	hold func(seq int64, msg *agentv1.AgentClientMessage) <-chan struct{}
	// fail, when set, decides whether the append for this frame fails.
	fail func(seq int64, msg *agentv1.AgentClientMessage) error
}

func (recorder *appendRecorder) append(ctx context.Context, seq int64, payload []byte) error {
	msg := &agentv1.AgentClientMessage{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return err
	}
	recorder.mu.Lock()
	recorder.entered = append(recorder.entered, seq)
	recorder.inFlight++
	if recorder.inFlight > recorder.maxInFlight {
		recorder.maxInFlight = recorder.inFlight
	}
	hold := recorder.hold
	fail := recorder.fail
	delay := recorder.delay
	recorder.mu.Unlock()

	var release <-chan struct{}
	if hold != nil {
		release = hold(seq, msg)
	}
	var timer <-chan time.Time
	if delay > 0 {
		timer = time.After(delay)
	}
	var err error
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if timer != nil && err == nil {
		select {
		case <-timer:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err == nil && fail != nil {
		err = fail(seq, msg)
	}

	recorder.mu.Lock()
	recorder.inFlight--
	if err == nil {
		recorder.frames = append(recorder.frames, appendedFrame{seq: seq, msg: msg})
	}
	recorder.mu.Unlock()
	return err
}

func (recorder *appendRecorder) delivered() []appendedFrame {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	frames := append([]appendedFrame(nil), recorder.frames...)
	sort.Slice(frames, func(i, j int) bool { return frames[i].seq < frames[j].seq })
	return frames
}

func (recorder *appendRecorder) enteredCount() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return len(recorder.entered)
}

func (recorder *appendRecorder) enteredSeqs() []int64 {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	seqs := append([]int64(nil), recorder.entered...)
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs
}

func (recorder *appendRecorder) peak() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.maxInFlight
}

func waitFor(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// ---------------------------------------------------------------------------
// Server frames
// ---------------------------------------------------------------------------

func serverFrame(msg *agentv1.AgentServerMessage) []byte {
	return cursorAccountFrame(mustCursorProto(msg))
}

func serverReadExec(id uint32) []byte {
	return serverFrame(&agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_ExecServerMessage{
		ExecServerMessage: &agentv1.ExecServerMessage{
			Id:     id,
			ExecId: "exec-read-" + string(rune('a'+id)),
			Message: &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{
				Path:       "/workspace/file.go",
				ToolCallId: "call-read-" + string(rune('a'+id)),
			}},
		},
	}})
}

func serverSetBlob(id uint32) []byte {
	return serverFrame(&agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_KvServerMessage{
		KvServerMessage: &agentv1.KvServerMessage{
			Id: id,
			Message: &agentv1.KvServerMessage_SetBlobArgs{SetBlobArgs: &agentv1.SetBlobArgs{
				BlobId:   []byte{byte(id), 1, 2, 3},
				BlobData: []byte("blob"),
			}},
		},
	}})
}

func serverTextDelta(text string) []byte {
	return serverFrame(&agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_InteractionUpdate{
		InteractionUpdate: &agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_TextDelta{
			TextDelta: &agentv1.TextDeltaUpdate{Text: text},
		}},
	}})
}

func readResultFor(exec *agentv1.ExecServerMessage) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{
		Id:     exec.GetId(),
		ExecId: exec.GetExecId(),
		Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: &agentv1.ReadResult{
			Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
				Path:   "/workspace/file.go",
				Output: &agentv1.ReadSuccess_Content{Content: "package main\n"},
			}},
		}},
	}
}

// ---------------------------------------------------------------------------
// The stream reader
// ---------------------------------------------------------------------------

// The model asks for several files at once and cannot continue until every
// result is in. Sent one at a time, each result and its stream_close wait for
// the previous round trip; the IDE sends them together and lets the sequence
// number order them.
func TestCursorAccountToolResultsOfOneBatchAreDeliveredTogether(t *testing.T) {
	const batch = 6
	const roundTrip = 100 * time.Millisecond
	reader, writer := io.Pipe()
	recorder := &appendRecorder{delay: roundTrip}
	results := make(chan *agentv1.ExecClientMessage, batch)
	dispatched := make(chan *agentv1.ExecServerMessage, batch)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- readCursorAccountStream(ctx, recorder.append, "req", "conv", "model", "cursor", 0, reader, nil, results, nil, nil, func(event ModelEvent) error {
			if event.Kind == ModelEventKindToolLikeCompleted && event.ToolInvocation != nil {
				exec := &agentv1.ExecServerMessage{}
				if err := proto.Unmarshal(event.ToolInvocation.NativeExec, exec); err != nil {
					return err
				}
				dispatched <- exec
			}
			return nil
		})
	}()

	for id := uint32(1); id <= batch; id++ {
		if _, err := writer.Write(serverReadExec(id)); err != nil {
			t.Fatal(err)
		}
	}
	execs := make([]*agentv1.ExecServerMessage, 0, batch)
	for len(execs) < batch {
		select {
		case exec := <-dispatched:
			execs = append(execs, exec)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d tool requests were dispatched", len(execs), batch)
		}
	}

	started := time.Now()
	for _, exec := range execs {
		results <- readResultFor(exec)
	}
	waitFor(t, 10*time.Second, "all results and stream closes to be delivered", func() bool {
		return len(recorder.delivered()) == 2*batch
	})
	elapsed := time.Since(started)

	_ = writer.Close()
	if err := <-done; err != nil {
		t.Fatalf("readCursorAccountStream() error = %v", err)
	}

	if sequential := time.Duration(2*batch) * roundTrip; elapsed > sequential/2 {
		t.Fatalf("delivering %d results took %s; one frame at a time takes %s, together about %s", batch, elapsed.Round(time.Millisecond), sequential, roundTrip)
	}
	if recorder.peak() < 2 {
		t.Fatalf("never more than %d append in flight", recorder.peak())
	}

	frames := recorder.delivered()
	resultSeq := map[uint32]int64{}
	closeSeq := map[uint32]int64{}
	for index, frame := range frames {
		if want := int64(index + 1); frame.seq != want {
			t.Fatalf("frame %d has sequence number %d, want %d: numbers must be consecutive", index, frame.seq, want)
		}
		if result := frame.msg.GetExecClientMessage(); result != nil {
			resultSeq[result.GetId()] = frame.seq
		}
		if closing := frame.msg.GetExecClientControlMessage().GetStreamClose(); closing != nil {
			closeSeq[closing.GetId()] = frame.seq
		}
	}
	for id := uint32(1); id <= batch; id++ {
		if resultSeq[id] == 0 || closeSeq[id] == 0 {
			t.Fatalf("exec %d: result seq=%d close seq=%d, want both delivered", id, resultSeq[id], closeSeq[id])
		}
		if resultSeq[id] >= closeSeq[id] {
			t.Fatalf("exec %d: stream_close (seq %d) is numbered before its result (seq %d)", id, closeSeq[id], resultSeq[id])
		}
	}
}

// Cursor's server stores blobs through the client and waits for no reply
// before it keeps streaming. Acknowledging one must not stop the reader from
// forwarding the text that follows.
func TestCursorAccountSlowBlobAckDoesNotHoldBackModelOutput(t *testing.T) {
	reader, writer := io.Pipe()
	release := make(chan struct{})
	recorder := &appendRecorder{hold: func(_ int64, msg *agentv1.AgentClientMessage) <-chan struct{} {
		if msg.GetKvClientMessage() != nil {
			return release
		}
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	text := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- readCursorAccountStream(ctx, recorder.append, "req", "conv", "model", "cursor", 0, reader, nil, nil, nil, nil, func(event ModelEvent) error {
			if event.Kind == ModelEventKindTextDelta {
				text <- event.Text
			}
			return nil
		})
	}()

	// A pipe write only returns once the reader has taken the bytes, so a
	// reader that stands still must not be able to hang the test itself.
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	go func() {
		_, _ = writer.Write(serverSetBlob(1))
		_, _ = writer.Write(serverTextDelta("hello"))
	}()

	select {
	case got := <-text:
		if got != "hello" {
			t.Fatalf("text = %q, want hello", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the model's text waited behind an unfinished blob acknowledgement")
	}

	close(release)
	_ = writer.Close()
	if err := <-done; err != nil {
		t.Fatalf("readCursorAccountStream() error = %v", err)
	}
	frames := recorder.delivered()
	if len(frames) != 1 || frames[0].msg.GetKvClientMessage().GetId() != 1 {
		t.Fatalf("delivered frames = %v, want the blob acknowledgement", frames)
	}
}

// A blob acknowledgement that cannot be delivered still ends the stream with
// the error, as it did when it was sent inline.
func TestCursorAccountFailedBlobAckEndsTheStream(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	boom := errors.New("append refused")
	recorder := &appendRecorder{fail: func(_ int64, msg *agentv1.AgentClientMessage) error {
		if msg.GetKvClientMessage() != nil {
			return boom
		}
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- readCursorAccountStream(ctx, recorder.append, "req", "conv", "model", "cursor", 0, reader, nil, nil, nil, nil, func(ModelEvent) error { return nil })
	}()
	if _, err := writer.Write(serverSetBlob(1)); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("readCursorAccountStream() error = %v, want the append failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream kept going after a blob acknowledgement could not be delivered")
	}
}

// lost reports the frame the sender gave up on, if any.
func (sender *cursorAccountFrameSender) lost() error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return sender.failed
}

// A tool result that cannot be delivered ends the pass, so the turn resumes in
// a fresh one. Frames the reader answers with afterwards are never sent; that
// must not turn the lost result into a failed turn.
func TestCursorAccountLostToolResultEndsThePassNotTheTurn(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	boom := errors.New("append refused")
	recorder := &appendRecorder{
		fail: func(_ int64, msg *agentv1.AgentClientMessage) error {
			if msg.GetExecClientMessage() != nil {
				return boom
			}
			return nil
		},
		// The stream_close numbered after the lost result is still on its way.
		hold: func(_ int64, msg *agentv1.AgentClientMessage) <-chan struct{} {
			if msg.GetExecClientControlMessage().GetStreamClose() != nil {
				return make(chan struct{})
			}
			return nil
		},
	}
	sender := newCursorAccountFrameSender(recorder.append)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	results := make(chan *agentv1.ExecClientMessage, 1)
	dispatched := make(chan *agentv1.ExecServerMessage, 1)
	textSeen := make(chan struct{})
	releaseText := make(chan struct{})
	var finish string
	done := make(chan error, 1)
	go func() {
		done <- readCursorAccountFrames(ctx, streamCtx, sender, "req", "conv", "model", "cursor", 0, reader, nil, results, nil, nil, func(event ModelEvent) error {
			switch event.Kind {
			case ModelEventKindToolLikeCompleted:
				exec := &agentv1.ExecServerMessage{}
				if err := proto.Unmarshal(event.ToolInvocation.NativeExec, exec); err != nil {
					return err
				}
				dispatched <- exec
			case ModelEventKindTextDelta:
				close(textSeen)
				<-releaseText
			case ModelEventKindTurnFinished:
				finish = event.FinishReason
			}
			return nil
		})
	}()

	go func() { _, _ = writer.Write(serverReadExec(1)) }()
	var exec *agentv1.ExecServerMessage
	select {
	case exec = <-dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool request was not dispatched")
	}

	// The reader is busy forwarding text when the result is lost, and a blob
	// to acknowledge is already waiting behind that text.
	go func() { _, _ = writer.Write(append(serverTextDelta("working"), serverSetBlob(1)...)) }()
	select {
	case <-textSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the text was not forwarded")
	}
	results <- readResultFor(exec)
	waitFor(t, 5*time.Second, "the tool result to be lost", func() bool { return sender.lost() != nil })
	close(releaseText)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the turn failed with %q; a lost tool result ends the pass so the turn can resume in a fresh one", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pass did not end after a tool result was lost")
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", finish)
	}
}

// ---------------------------------------------------------------------------
// The sender
// ---------------------------------------------------------------------------

// The frames of a batch travel together. A lost one is reported at once, not
// when the others have finished: whoever waits for the batch must not keep
// waiting for a frame the server can no longer process.
func TestCursorAccountFrameSenderReportsALostFrameAtOnce(t *testing.T) {
	boom := errors.New("append refused")
	held := make(chan struct{})
	defer close(held)
	recorder := &appendRecorder{
		fail: func(seq int64, _ *agentv1.AgentClientMessage) error {
			if seq == 1 {
				return boom
			}
			return nil
		},
		hold: func(seq int64, _ *agentv1.AgentClientMessage) <-chan struct{} {
			if seq == 2 {
				return held
			}
			return nil
		},
	}
	sender := newCursorAccountFrameSender(recorder.append)
	outcome := make(chan error, 1)

	sender.send(context.Background(), func(err error) { outcome <- err }, kvAckPayload(1), kvAckPayload(2))

	select {
	case err := <-outcome:
		if !errors.Is(err, boom) {
			t.Fatalf("batch outcome = %v, want the append failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the lost frame was not reported while another frame of its batch was still on its way")
	}
}

func kvAckPayload(id uint32) []byte {
	return mustCursorProto(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_KvClientMessage{
		KvClientMessage: &agentv1.KvClientMessage{Id: id},
	}})
}

// The IDE never sends more than 32 frames past the oldest one the server has
// not acknowledged, so the server's reordering buffer stays bounded.
func TestCursorAccountFrameSenderStaysWithinTheAppendWindow(t *testing.T) {
	gates := map[int64]chan struct{}{}
	var gatesMu sync.Mutex
	gate := func(seq int64) chan struct{} {
		gatesMu.Lock()
		defer gatesMu.Unlock()
		if gates[seq] == nil {
			gates[seq] = make(chan struct{})
		}
		return gates[seq]
	}
	recorder := &appendRecorder{hold: func(seq int64, _ *agentv1.AgentClientMessage) <-chan struct{} { return gate(seq) }}
	sender := newCursorAccountFrameSender(recorder.append)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const total = 40
	for i := 0; i < total; i++ {
		sender.send(ctx, nil, kvAckPayload(uint32(i)))
	}

	waitFor(t, 5*time.Second, "the first window of frames to be in flight", func() bool {
		return recorder.enteredCount() == cursorAccountAppendWindow
	})
	time.Sleep(50 * time.Millisecond)
	if got := recorder.enteredCount(); got != cursorAccountAppendWindow {
		t.Fatalf("%d frames in flight, want exactly the window of %d", got, cursorAccountAppendWindow)
	}
	seqs := recorder.enteredSeqs()
	if seqs[0] != 1 || seqs[len(seqs)-1] != cursorAccountAppendWindow {
		t.Fatalf("in-flight sequence numbers run %d..%d, want 1..%d", seqs[0], seqs[len(seqs)-1], cursorAccountAppendWindow)
	}

	// Acknowledging a frame in the middle does not move the window: the oldest
	// one is still outstanding.
	close(gate(5))
	time.Sleep(50 * time.Millisecond)
	if got := recorder.enteredCount(); got != cursorAccountAppendWindow {
		t.Fatalf("%d frames sent after a mid-window acknowledgement, want still %d", got, cursorAccountAppendWindow)
	}

	// Acknowledging the oldest one does.
	close(gate(1))
	waitFor(t, 5*time.Second, "the window to slide", func() bool {
		return recorder.enteredCount() == cursorAccountAppendWindow+1
	})

	for seq := int64(1); seq <= total; seq++ {
		if seq != 1 && seq != 5 {
			close(gate(seq))
		}
	}
	waitFor(t, 5*time.Second, "every frame to be delivered", func() bool {
		return len(recorder.delivered()) == total
	})
	for index, frame := range recorder.delivered() {
		if want := int64(index + 1); frame.seq != want {
			t.Fatalf("frame %d has sequence number %d, want %d", index, frame.seq, want)
		}
		if got := frame.msg.GetKvClientMessage().GetId(); got != uint32(index) {
			t.Fatalf("sequence number %d carries payload %d, want payload %d: numbers follow the order frames were handed in", frame.seq, got, index)
		}
	}
}

// Once a frame is lost the server can never process the ones numbered after
// it. The failure is reported, and later frames are not sent into the void.
func TestCursorAccountFrameSenderStopsAfterAFailedFrame(t *testing.T) {
	boom := errors.New("append refused")
	recorder := &appendRecorder{fail: func(seq int64, _ *agentv1.AgentClientMessage) error {
		if seq == 2 {
			return boom
		}
		return nil
	}}
	sender := newCursorAccountFrameSender(recorder.append)
	ctx := context.Background()

	outcomes := make(chan error, 3)
	sender.send(ctx, func(err error) { outcomes <- err }, kvAckPayload(1))
	if err := <-outcomes; err != nil {
		t.Fatalf("first frame: %v", err)
	}
	sender.send(ctx, func(err error) { outcomes <- err }, kvAckPayload(2))
	if err := <-outcomes; !errors.Is(err, boom) {
		t.Fatalf("second frame outcome = %v, want the append failure", err)
	}

	sender.send(ctx, func(err error) { outcomes <- err }, kvAckPayload(3))
	select {
	case err := <-outcomes:
		if err == nil {
			t.Fatal("a frame sent after a lost one was reported as delivered")
		}
		// It is told apart from the loss itself, which was already reported.
		if !errors.Is(err, errCursorAccountFrameNotSent) || !errors.Is(err, boom) {
			t.Fatalf("outcome = %v, want a frame that was not sent because of the earlier loss", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a frame sent after a lost one never reported back")
	}
	if got := recorder.enteredCount(); got != 2 {
		t.Fatalf("%d appends were attempted, want 2: nothing is sent after a lost frame", got)
	}
}

// Frames handed over together keep their order: a result is always numbered
// before the stream_close that follows it.
func TestCursorAccountFrameSenderNumbersABatchInOrder(t *testing.T) {
	recorder := &appendRecorder{delay: 10 * time.Millisecond}
	sender := newCursorAccountFrameSender(recorder.append)
	delivered := make(chan error, 1)

	sender.send(context.Background(), func(err error) { delivered <- err }, kvAckPayload(10), kvAckPayload(11), kvAckPayload(12))

	if err := <-delivered; err != nil {
		t.Fatalf("batch outcome = %v", err)
	}
	frames := recorder.delivered()
	if len(frames) != 3 {
		t.Fatalf("delivered %d frames, want 3 before the batch reports back", len(frames))
	}
	for index, frame := range frames {
		if frame.seq != int64(index+1) || frame.msg.GetKvClientMessage().GetId() != uint32(10+index) {
			t.Fatalf("frame %d: seq=%d payload=%d, want seq=%d payload=%d", index, frame.seq, frame.msg.GetKvClientMessage().GetId(), index+1, 10+index)
		}
	}
}

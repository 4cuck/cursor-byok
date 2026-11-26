package modeladapter

import (
	"context"
	"strings"
	"sync"
	"time"

	"cursor/gen/agentv1"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// A RunSSE stream to Cursor's server can be lost in the middle of a turn: the
// server sends GOAWAY when it is replaced, a proxy resets the connection. The
// turn is not lost with it. While a turn runs the server streams a checkpoint
// after every completed step, and it takes any of them back with a
// ResumeAction to continue the turn from that step. A tool call that was
// pending in the checkpoint is requested again under the same tool call id.
//
// This is how the IDE's own agent client survives a dropped stream
// (turn-runner.ts): a transport failure is retried from the newest checkpoint
// the failed attempt received or, when it received none, with the state and
// the action it had sent.
//
// The gateway ends an interrupted pass with FinishReasonCursorStreamInterrupted
// instead of an error. The caller starts another pass of the same run, and
// that pass's run message is built from what is recorded here.

// FinishReasonCursorStreamInterrupted is the finish reason of a pass whose
// stream to Cursor's server was lost before the turn ended. The turn is not
// over: the next pass of the same run resumes it.
const FinishReasonCursorStreamInterrupted = "cursor_stream_interrupted"

const (
	// cursorAccountMaxStreamRecoveries is how many lost streams one run
	// survives. It is the IDE's cap on transport retries.
	cursorAccountMaxStreamRecoveries = 10
	// cursorAccountMaxBarrenRecoveries is how many attempts in a row may be
	// lost before bringing a checkpoint. Past it, resuming would only repeat
	// the same work.
	cursorAccountMaxBarrenRecoveries = 2
	// cursorAccountInterruptionRetention bounds how long a run's record is
	// kept. A run that is still being resumed is touched far more often.
	cursorAccountInterruptionRetention = 3 * time.Hour
)

// cursorAccountRecoveryBackoff is the pause before a recovery attempt. The
// first one is immediate, as in the IDE; later ones double from one second.
// Tests shorten it.
var cursorAccountRecoveryBackoff = func(recoveries int) time.Duration {
	if recoveries <= 1 {
		return 0
	}
	return min(time.Second<<(recoveries-2), 30*time.Second)
}

// cursorAccountStreamLostError is a read failure of an open RunSSE stream
// before the turn ended. checkpointed reports whether the attempt received a
// checkpoint, so that the saved state holds this turn up to its last
// completed step.
type cursorAccountStreamLostError struct {
	cause        error
	checkpointed bool
}

func (e *cursorAccountStreamLostError) Error() string { return e.cause.Error() }

func (e *cursorAccountStreamLostError) Unwrap() error { return e.cause }

// cursorAccountInterruption is what a run remembers about its lost streams.
type cursorAccountInterruption struct {
	// pending is set from a lost stream until the pass that recovers it.
	pending      bool
	checkpointed bool
	// state, action and blobs are what the lost attempt sent. They are sent
	// again when it received no checkpoint: nothing of it was kept upstream.
	state  *agentv1.ConversationStateStructure
	action *agentv1.ConversationAction
	blobs  []*agentv1.PreFetchedBlob
	// recoveries counts the lost streams of this run, barren those in a row
	// that brought no checkpoint.
	recoveries int
	barren     int
	updatedAt  time.Time
}

var cursorAccountInterruptions = struct {
	mu   sync.Mutex
	runs map[string]*cursorAccountInterruption
}{runs: map[string]*cursorAccountInterruption{}}

// cursorAccountNoteStreamLost records a lost stream of runID. It reports the
// number of streams the run has lost and whether the run may resume.
func cursorAccountNoteStreamLost(runID string, checkpointed bool, run *agentv1.AgentClientMessage) (int, bool) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return 0, false
	}
	now := time.Now()
	cursorAccountInterruptions.mu.Lock()
	defer cursorAccountInterruptions.mu.Unlock()
	for id, item := range cursorAccountInterruptions.runs {
		if now.Sub(item.updatedAt) > cursorAccountInterruptionRetention {
			delete(cursorAccountInterruptions.runs, id)
		}
	}
	item := cursorAccountInterruptions.runs[runID]
	if item == nil {
		item = &cursorAccountInterruption{}
		cursorAccountInterruptions.runs[runID] = item
	}
	item.updatedAt = now
	item.recoveries++
	if checkpointed {
		item.barren = 0
	} else {
		item.barren++
	}
	if item.recoveries > cursorAccountMaxStreamRecoveries || item.barren > cursorAccountMaxBarrenRecoveries {
		delete(cursorAccountInterruptions.runs, runID)
		return item.recoveries, false
	}
	item.pending = true
	item.checkpointed = checkpointed
	item.state, item.action, item.blobs = nil, nil, nil
	if request := run.GetRunRequest(); !checkpointed && request != nil {
		item.state = proto.CloneOf(request.GetConversationState())
		item.action = proto.CloneOf(request.GetAction())
		for _, blob := range request.GetPreFetchedBlobs() {
			item.blobs = append(item.blobs, proto.CloneOf(blob))
		}
	}
	return item.recoveries, true
}

// cursorAccountTakeRecovery returns the lost stream runID has to recover
// from, if any, and marks it as taken.
func cursorAccountTakeRecovery(runID string) (cursorAccountInterruption, bool) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return cursorAccountInterruption{}, false
	}
	cursorAccountInterruptions.mu.Lock()
	defer cursorAccountInterruptions.mu.Unlock()
	item := cursorAccountInterruptions.runs[runID]
	if item == nil || !item.pending {
		return cursorAccountInterruption{}, false
	}
	item.pending = false
	item.updatedAt = time.Now()
	return *item, true
}

// cursorAccountRestoreRecovery hands a taken recovery back: the call that
// took it failed, so the turn is still to be resumed.
func cursorAccountRestoreRecovery(runID string) {
	cursorAccountInterruptions.mu.Lock()
	defer cursorAccountInterruptions.mu.Unlock()
	if item := cursorAccountInterruptions.runs[strings.TrimSpace(runID)]; item != nil {
		item.pending = true
		item.updatedAt = time.Now()
	}
}

// cursorAccountForgetInterruptions drops what is recorded for runID. Tests
// use it; a record otherwise ages out.
func cursorAccountForgetInterruptions(runID string) {
	cursorAccountInterruptions.mu.Lock()
	delete(cursorAccountInterruptions.runs, strings.TrimSpace(runID))
	cursorAccountInterruptions.mu.Unlock()
}

// cursorAccountApplyRecovery turns run into the message that resumes a run
// whose last stream was lost (req.cursorRecovery). Without one, run is left
// as built.
func cursorAccountApplyRecovery(req StreamRequest, run *agentv1.AgentClientMessage, stateConversationID string) {
	request := run.GetRunRequest()
	recovery := req.cursorRecovery
	if request == nil || recovery == nil {
		return
	}
	switch {
	case recovery.checkpointed:
		state := cursorAccountLoadState(stateConversationID)
		if !cursorAccountStateHasConversation(state) {
			// The checkpoint is gone (cleared by a rewind or a refusal). The
			// message built from the transcript is the best there is.
			return
		}
		// The checkpoint already holds this turn's request and every step
		// completed since. Whatever the transcript ends with, nothing is
		// sent on top of it.
		request.ConversationState = state
		request.Action = &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_ResumeAction{
				ResumeAction: &agentv1.ResumeAction{RequestContext: cursorAccountRequestContext(req)},
			},
		}
		request.PreFetchedBlobs = nil
	case recovery.action != nil:
		// Nothing of the lost attempt was kept upstream: send it again.
		request.ConversationState = proto.CloneOf(recovery.state)
		request.Action = proto.CloneOf(recovery.action)
		request.PreFetchedBlobs = nil
		for _, blob := range recovery.blobs {
			request.PreFetchedBlobs = append(request.PreFetchedBlobs, proto.CloneOf(blob))
		}
	default:
		return
	}
	// The lost stream may still own its conversation id upstream, with an
	// exec it is waiting on. A fresh id never waits on it.
	request.ConversationId = proto.String(uuid.NewString())
}

// CursorAccountExecToolCallID returns the id of the tool call a Cursor exec
// belongs to, as the gateway names it before making ids unique within a
// pass. An exec requested again after a resume carries the same one. It is ""
// for an exec that cannot be decoded.
func CursorAccountExecToolCallID(nativeExec []byte) string {
	if len(nativeExec) == 0 {
		return ""
	}
	exec := &agentv1.ExecServerMessage{}
	if err := proto.Unmarshal(nativeExec, exec); err != nil {
		return ""
	}
	_, callID, _, ok := cursorAccountExecTool(exec)
	if !ok {
		return ""
	}
	return strings.TrimSpace(callID)
}

// cursorAccountWaitBeforeRecovery pauses before a recovery attempt.
func cursorAccountWaitBeforeRecovery(ctx context.Context, recoveries int) error {
	delay := cursorAccountRecoveryBackoff(recoveries)
	if delay <= 0 {
		return nil
	}
	return sleepWithContext(ctx, delay)
}

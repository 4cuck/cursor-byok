// cursor_exec_replay.go keeps a Cursor-account turn from running a tool twice
// after its stream to Cursor was lost.
//
// A lost stream is resumed from Cursor's last checkpoint
// (modeladapter.FinishReasonCursorStreamInterrupted). A tool call that was
// pending in that checkpoint is requested again, under the same tool call id:
// the server never saw its result. The client has usually finished the tool
// by then, or finished it while the stream was down. The request is answered
// with the result this turn already has; the tool does not run again.
package forwarder

import (
	"strings"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	modeladapter "cursor/internal/backend/agent/model"
	"cursor/internal/logger"

	"google.golang.org/protobuf/proto"
)

const (
	// Only the tool calls of the step that was running when the stream was
	// lost can be requested again; earlier ones are in the checkpoint. A step
	// has a handful of calls, so a short memory is enough.
	cursorExecAnswerLimit = 64
	// cursorExecAnswerByteLimit bounds what the memory holds in total.
	cursorExecAnswerByteLimit = 16 << 20
)

// cursorExecAnswer is what a tool call's exec was answered with.
type cursorExecAnswer struct {
	toolName string
	messages []*agentv1.ExecClientMessage
	bytes    int
}

// noteCursorExecCall opens the record of a tool call that is dispatched to
// the client as one of Cursor's execs. Internal execs (the reads of a wait,
// the reads and the write of an edit) are requested many times under one id
// and are not tool calls of the model: they have no record.
func noteCursorExecCall(stream *ActiveStream, callID string, toolName string) {
	callID = strings.TrimSpace(callID)
	if stream == nil || callID == "" {
		return
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.cursorExecAnswers == nil {
		stream.cursorExecAnswers = make(map[string]cursorExecAnswer)
	}
	if _, known := stream.cursorExecAnswers[callID]; known {
		return
	}
	stream.cursorExecAnswers[callID] = cursorExecAnswer{toolName: strings.TrimSpace(toolName)}
	stream.cursorExecAnswerOrder = append(stream.cursorExecAnswerOrder, callID)
	trimCursorExecAnswersLocked(stream)
}

// rememberCursorExecAnswer records the messages a finished exec is answered
// with, whether or not they could be delivered.
func (service *Service) rememberCursorExecAnswer(stream *ActiveStream, pending runtimecore.PendingExec, messages ...*agentv1.ExecClientMessage) {
	toolCallID := strings.TrimSpace(pending.ToolCallID)
	if service == nil || stream == nil || pending.SilentUI || toolCallID == "" {
		return
	}
	var kept []*agentv1.ExecClientMessage
	size := 0
	for _, message := range messages {
		if message == nil {
			continue
		}
		kept = append(kept, proto.CloneOf(message))
		size += proto.Size(message)
	}
	if len(kept) == 0 || size > cursorExecAnswerByteLimit {
		return
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	answer, known := stream.cursorExecAnswers[toolCallID]
	if !known {
		// Not one of Cursor's execs, or its record has aged out.
		return
	}
	answer.messages, answer.bytes = kept, size
	stream.cursorExecAnswers[toolCallID] = answer
	trimCursorExecAnswersLocked(stream)
}

// trimCursorExecAnswersLocked drops the oldest records past the limits.
// Caller holds stream.mu.
func trimCursorExecAnswersLocked(stream *ActiveStream) {
	total := 0
	for _, item := range stream.cursorExecAnswers {
		total += item.bytes
	}
	for len(stream.cursorExecAnswerOrder) > 1 && (len(stream.cursorExecAnswerOrder) > cursorExecAnswerLimit || total > cursorExecAnswerByteLimit) {
		oldest := stream.cursorExecAnswerOrder[0]
		stream.cursorExecAnswerOrder = stream.cursorExecAnswerOrder[1:]
		total -= stream.cursorExecAnswers[oldest].bytes
		delete(stream.cursorExecAnswers, oldest)
	}
}

// answerReissuedCursorExec answers an exec Cursor requests again for a tool
// call this turn has already run. It reports whether it did; if not, the exec
// is dispatched as any other.
func (service *Service) answerReissuedCursorExec(stream *ActiveStream, invocation runtimecore.ToolInvocation) bool {
	callID := strings.TrimSpace(invocation.CallID)
	if service == nil || stream == nil || invocation.SilentUI || len(invocation.NativeExec) == 0 || callID == "" {
		return false
	}
	// Ids are made unique within a pass. Only the exec dispatched under the
	// tool call's own id is the same exec in every pass.
	if modeladapter.CursorAccountExecToolCallID(invocation.NativeExec) != callID {
		return false
	}
	stream.mu.Lock()
	answer, known := stream.cursorExecAnswers[callID]
	stream.mu.Unlock()
	if !known || len(answer.messages) == 0 || answer.toolName != strings.TrimSpace(invocation.ToolName) {
		return false
	}
	original := &agentv1.ExecServerMessage{}
	if err := proto.Unmarshal(invocation.NativeExec, original); err != nil {
		return false
	}
	// The answer goes out under the ids of this request, not the first one's.
	rememberNativeExecSlot(stream, callID, original)
	for index, message := range answer.messages {
		if deliverCursorToolResultWithin(stream, callID, message, cursorShellTerminalDeliverWait) {
			continue
		}
		if index == 0 {
			// Nothing was sent: run the tool as Cursor asks.
			return false
		}
		// Part of the answer is out and the rest is not. The exec stays open
		// upstream; the stall watchdog of the stream ends the pass.
		logger.Warnf("forwarder cursor exec answer was only partly replayed request_id=%s tool=%s call_id=%s sent=%d/%d",
			strings.TrimSpace(stream.RequestID), answer.toolName, callID, index, len(answer.messages))
		return true
	}
	logger.Infof("forwarder cursor exec requested again after a resumed stream; answered with the recorded result request_id=%s tool=%s call_id=%s messages=%d",
		strings.TrimSpace(stream.RequestID), answer.toolName, callID, len(answer.messages))
	return true
}

// forgetCursorExecAnswersLocked drops the turn's recorded answers. Caller
// holds stream.mu.
func forgetCursorExecAnswersLocked(stream *ActiveStream) {
	stream.cursorExecAnswers = nil
	stream.cursorExecAnswerOrder = nil
}

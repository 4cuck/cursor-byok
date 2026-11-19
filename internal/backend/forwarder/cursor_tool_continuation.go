package forwarder

import (
	"strings"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/proto"
)

// armCursorToolContinuation is kept so a new tool can still be answered on the
// open Cursor account stream. Results are queued, not limited to one at a time,
// because the model often emits Read, Write, and Shell in one burst.
func armCursorToolContinuation(stream *ActiveStream) {
	if stream == nil {
		return
	}
	stream.mu.Lock()
	stream.cursorToolWaiting = stream.CursorToolResults != nil
	stream.mu.Unlock()
}

// deliverCursorToolResult hands one tool result to the open Cursor account stream.
// A nil message ends that stream so the normal follow-up pass can resume.
func deliverCursorToolResult(stream *ActiveStream, toolCallID string, msg *agentv1.ExecClientMessage) bool {
	if stream == nil {
		return false
	}
	if msg != nil {
		cloned, _ := proto.Clone(msg).(*agentv1.ExecClientMessage)
		if cloned == nil {
			cloned = msg
		}
		stream.mu.Lock()
		if slot, ok := stream.nativeExecSlots[strings.TrimSpace(toolCallID)]; ok && (slot.id != 0 || slot.execID != "") {
			cloned.Id = slot.id
			cloned.ExecId = slot.execID
		}
		stream.mu.Unlock()
		msg = cloned
	}
	stream.mu.Lock()
	ch := stream.CursorToolResults
	if ch == nil {
		stream.mu.Unlock()
		return false
	}
	if msg != nil {
		stream.CursorInlineToolTurn = true
	}
	stream.mu.Unlock()
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}

func deliverCursorInteractionResult(stream *ActiveStream, msg *agentv1.InteractionResponse) bool {
	if stream == nil || msg == nil {
		return false
	}
	stream.mu.Lock()
	ch := stream.CursorInteractionReplies
	stream.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- msg:
		// SwitchMode and the other interaction cards are answered on the same
		// open Cursor stream. Mark that turn inline so a later stop does not
		// start another pass after the model has already finished.
		stream.mu.Lock()
		stream.CursorInlineToolTurn = true
		stream.mu.Unlock()
		return true
	default:
		return false
	}
}

func (service *Service) continueCursorToolOrReconcile(stream *ActiveStream, toolCallID string, msg *agentv1.ExecClientMessage) error {
	if deliverCursorToolResult(stream, toolCallID, msg) {
		return nil
	}
	return service.reconcileStream(stream)
}

// shellStreamOutputPreface is the one stdout event the Cursor account stream
// needs before a shell exit. Live chunks stay on the local terminal. The
// upstream model gets Cursor's head-and-tail view, then the exit, then the
// gateway closes the exec. Sending the raw listing leaves that exec open.
func shellStreamOutputPreface(pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) *agentv1.ExecClientMessage {
	if message.GetShellStream().GetExit() == nil {
		return nil
	}
	combined := pending.StdoutBuffer
	if pending.StderrBuffer != "" {
		if combined != "" && !strings.HasSuffix(combined, "\n") {
			combined += "\n"
		}
		combined += pending.StderrBuffer
	}
	if strings.TrimSpace(combined) == "" {
		return nil
	}
	return &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_ShellStream{
			ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Stdout{
					Stdout: &agentv1.ShellStreamStdout{Data: modeladapter.CursorAccountShellOutput(combined)},
				},
			},
		},
	}
}

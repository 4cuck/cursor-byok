package forwarder

import (
	"io"
	"os"
	"strings"
	"time"

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
	return deliverCursorToolResultWithin(stream, toolCallID, msg, 0)
}

func deliverCursorToolResultWithin(stream *ActiveStream, toolCallID string, msg *agentv1.ExecClientMessage, wait time.Duration) bool {
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
	if wait <= 0 {
		select {
		case ch <- msg:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case ch <- msg:
		return true
	case <-timer.C:
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

const (
	cursorShellTerminalDeliverWait = 2 * time.Second
	shellSpillReadLimit            = 512 * 1024
)

// deliverCursorAccountShellTerminal sends the text the model can read, then the
// finished shell, on the open Cursor account stream. A spill file path is not a
// substitute for that text: the model then tries to Read it and the turn stays
// on "Planning next moves".
func deliverCursorAccountShellTerminal(stream *ActiveStream, toolCallID string, pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) bool {
	accountPending := pendingWithShellSpill(pending, message)
	terminal := cursorAccountShellTerminalMessage(message, shellHasInlineOutput(accountPending, message))
	if preface := shellStreamOutputPreface(accountPending, terminal); preface != nil {
		if !deliverCursorToolResultWithin(stream, toolCallID, preface, cursorShellTerminalDeliverWait) {
			return false
		}
	}
	return deliverCursorToolResultWithin(stream, toolCallID, terminal, cursorShellTerminalDeliverWait)
}

func isCursorAccountShellTerminal(message *agentv1.ExecClientMessage) bool {
	return message.GetShellStream().GetExit() != nil || message.GetShellResult() != nil
}

func pendingWithShellSpill(pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) runtimecore.PendingExec {
	located := readShellSpillFile(shellOutputLocationPath(message))
	if len(located) > len(pending.StdoutBuffer) {
		pending.StdoutBuffer = located
	}
	return pending
}

func shellHasInlineOutput(pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) bool {
	if strings.TrimSpace(pending.StdoutBuffer) != "" || strings.TrimSpace(pending.StderrBuffer) != "" {
		return true
	}
	if success := message.GetShellResult().GetSuccess(); success != nil {
		if strings.TrimSpace(success.GetStdout()) != "" || strings.TrimSpace(success.GetStderr()) != "" || strings.TrimSpace(success.GetInterleavedOutput()) != "" {
			return true
		}
	}
	if failure := message.GetShellResult().GetFailure(); failure != nil {
		if strings.TrimSpace(failure.GetStdout()) != "" || strings.TrimSpace(failure.GetStderr()) != "" || strings.TrimSpace(failure.GetInterleavedOutput()) != "" {
			return true
		}
	}
	return false
}

func shellOutputLocationPath(message *agentv1.ExecClientMessage) string {
	if message == nil {
		return ""
	}
	if exit := message.GetShellStream().GetExit(); exit != nil && exit.GetOutputLocation() != nil {
		return strings.TrimSpace(exit.GetOutputLocation().GetFilePath())
	}
	if success := message.GetShellResult().GetSuccess(); success != nil && success.GetOutputLocation() != nil {
		return strings.TrimSpace(success.GetOutputLocation().GetFilePath())
	}
	if failure := message.GetShellResult().GetFailure(); failure != nil && failure.GetOutputLocation() != nil {
		return strings.TrimSpace(failure.GetOutputLocation().GetFilePath())
	}
	return ""
}

func readShellSpillFile(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 0 || !info.Mode().IsRegular() {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	if info.Size() <= shellSpillReadLimit {
		buf, err := io.ReadAll(io.LimitReader(file, shellSpillReadLimit))
		if err != nil && len(buf) == 0 {
			return ""
		}
		return string(buf)
	}
	headLen := shellSpillReadLimit / 2
	head := make([]byte, headLen)
	n, _ := io.ReadFull(file, head)
	head = head[:n]
	tail := make([]byte, headLen)
	if _, err := file.Seek(-int64(len(tail)), io.SeekEnd); err != nil {
		return string(head)
	}
	n, _ = io.ReadFull(file, tail)
	return string(head) + "\n\n[Output too long. middle of spill file elided]\n\n" + string(tail[:n])
}

// cursorAccountShellTerminalMessage is the shell completion sent upstream.
// When the output text is already inline, the spill-file path is removed so
// the model does not open a second Read for the same bytes.
func cursorAccountShellTerminalMessage(message *agentv1.ExecClientMessage, haveInlineOutput bool) *agentv1.ExecClientMessage {
	if message == nil || (message.GetShellStream().GetExit() == nil && message.GetShellResult() == nil) {
		return message
	}
	cloned, _ := proto.Clone(message).(*agentv1.ExecClientMessage)
	if cloned == nil {
		return message
	}
	if exit := cloned.GetShellStream().GetExit(); exit != nil && haveInlineOutput {
		exit.OutputLocation = nil
	}
	if result := cloned.GetShellResult(); result != nil {
		trimCursorAccountShellResult(result, haveInlineOutput)
	}
	return cloned
}

func trimCursorAccountShellResult(result *agentv1.ShellResult, dropLocation bool) {
	if result == nil {
		return
	}
	if success := result.GetSuccess(); success != nil {
		success.Stdout = modeladapter.CursorAccountShellOutput(success.GetStdout())
		success.Stderr = modeladapter.CursorAccountShellOutput(success.GetStderr())
		if success.InterleavedOutput != nil {
			trimmed := modeladapter.CursorAccountShellOutput(success.GetInterleavedOutput())
			success.InterleavedOutput = &trimmed
		}
		if dropLocation {
			success.OutputLocation = nil
		}
	}
	if failure := result.GetFailure(); failure != nil {
		failure.Stdout = modeladapter.CursorAccountShellOutput(failure.GetStdout())
		failure.Stderr = modeladapter.CursorAccountShellOutput(failure.GetStderr())
		if failure.InterleavedOutput != nil {
			trimmed := modeladapter.CursorAccountShellOutput(failure.GetInterleavedOutput())
			failure.InterleavedOutput = &trimmed
		}
		if dropLocation {
			failure.OutputLocation = nil
		}
	}
}

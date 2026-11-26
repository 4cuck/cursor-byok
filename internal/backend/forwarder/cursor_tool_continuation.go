package forwarder

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cursor/gen/agentv1"
	execbridge "cursor/internal/backend/agent/bridge/exec"
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
		if slot, ok := stream.nativeExecSlots[strings.TrimSpace(toolCallID)]; ok {
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
// finished shell, on the open Cursor account stream. For local Cursor spill
// files (agent-tools/*.txt or the stream's terminals folder), inline bounded
// text so the model doesn't stall on "Planning next moves"; never read
// arbitrary paths, and preserve OutputLocation when no inline text is available
// (such as remote SSH spill files).
func deliverCursorAccountShellTerminal(stream *ActiveStream, toolCallID string, pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) bool {
	preface, terminal := cursorAccountShellTerminalMessages(stream, pending, message)
	return sendCursorAccountShellTerminal(stream, toolCallID, preface, terminal)
}

// answerCursorAccountShellTerminal is deliverCursorAccountShellTerminal for a
// tool call of the model: what the shell is answered with is also kept, so a
// turn resumed after a lost stream can answer the same call again.
func (service *Service) answerCursorAccountShellTerminal(stream *ActiveStream, pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) bool {
	preface, terminal := cursorAccountShellTerminalMessages(stream, pending, message)
	service.rememberCursorExecAnswer(stream, pending, preface, terminal)
	return sendCursorAccountShellTerminal(stream, pending.ToolCallID, preface, terminal)
}

func sendCursorAccountShellTerminal(stream *ActiveStream, toolCallID string, preface, terminal *agentv1.ExecClientMessage) bool {
	if terminal == nil {
		return false
	}
	if preface != nil && !deliverCursorToolResultWithin(stream, toolCallID, preface, cursorShellTerminalDeliverWait) {
		return false
	}
	return deliverCursorToolResultWithin(stream, toolCallID, terminal, cursorShellTerminalDeliverWait)
}

// cursorAccountShellTerminalMessages builds what a finished shell is answered
// with on the Cursor account stream: the text the model can read, if any,
// then the end of the shell.
func cursorAccountShellTerminalMessages(stream *ActiveStream, pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) (*agentv1.ExecClientMessage, *agentv1.ExecClientMessage) {
	if message == nil {
		return nil, nil
	}
	accountPending := pendingWithLocalCursorShellSpill(stream, pending, message)
	terminal := proto.CloneOf(message)
	if result := terminal.GetShellResult(); result != nil {
		execbridge.FillMissingShellOutput(result, accountPending)
		trim := func(stdout, stderr *string, interleaved, head, tail *string, location **agentv1.OutputLocation) {
			*stdout = modeladapter.CursorAccountShellOutput(*stdout)
			*stderr = modeladapter.CursorAccountShellOutput(*stderr)
			hasText := strings.TrimSpace(*stdout) != "" || strings.TrimSpace(*stderr) != ""
			for _, field := range []*string{interleaved, head, tail} {
				if field != nil {
					*field = modeladapter.CursorAccountShellOutput(*field)
					hasText = hasText || strings.TrimSpace(*field) != ""
				}
			}
			if hasText {
				*location = nil
			}
		}
		if s := result.GetSuccess(); s != nil {
			trim(&s.Stdout, &s.Stderr, s.InterleavedOutput, s.OutputHead, s.OutputTail, &s.OutputLocation)
		} else if f := result.GetFailure(); f != nil {
			trim(&f.Stdout, &f.Stderr, f.InterleavedOutput, f.OutputHead, f.OutputTail, &f.OutputLocation)
		}
	}
	preface := shellStreamOutputPreface(accountPending, terminal)
	if preface != nil {
		// The text travels in the preface; the exit does not point at a file.
		terminal.GetShellStream().GetExit().OutputLocation = nil
	}
	return preface, terminal
}

func isCursorAccountShellTerminal(message *agentv1.ExecClientMessage) bool {
	return message.GetShellStream().GetExit() != nil || message.GetShellResult() != nil
}

func pendingWithLocalCursorShellSpill(stream *ActiveStream, pending runtimecore.PendingExec, message *agentv1.ExecClientMessage) runtimecore.PendingExec {
	path := shellOutputLocationPath(message)
	if !isAllowedLocalCursorShellSpillPath(stream, path) {
		return pending
	}
	if located := readLocalCursorShellSpillFile(path); len(located) > len(pending.StdoutBuffer) {
		pending.StdoutBuffer = located
	}
	return pending
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

func isAllowedLocalCursorShellSpillPath(stream *ActiveStream, path string) bool {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || trimmed != path {
		return false
	}
	if modeladapter.IsCursorInternalSpillWritePath(trimmed) {
		return true
	}
	if stream == nil {
		return false
	}
	stream.mu.Lock()
	terminalsFolder := strings.TrimSpace(stream.TerminalsFolder)
	stream.mu.Unlock()
	if terminalsFolder == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(terminalsFolder), filepath.Clean(trimmed))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.ContainsAny(rel, `/\`) {
		return false
	}
	return strings.HasSuffix(strings.ToLower(rel), ".txt") && len(rel) > len(".txt")
}

func readLocalCursorShellSpillFile(path string) string {
	info, err := os.Lstat(path)
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

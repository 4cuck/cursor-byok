package execbridge

import (
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/proto"
)

// A Shell call that ends with its command still running is the model's only
// introduction to that background job. The Cursor client's own Shell tool
// says that the command is still running, names the shell and the file its
// output goes to, and hands over the output collected so far. Without those
// the model cannot follow the job up (AwaitShell, reading the terminal file)
// and has lost everything the command printed while it was in the foreground.

const backgroundedTerminalFile = `C:\Users\dev\.cursor\projects\p\terminals/447245.txt`

func backgroundedEvent(msToWait int32, reason agentv1.ShellBackgroundReason) *agentv1.ShellStreamBackgrounded {
	return &agentv1.ShellStreamBackgrounded{
		ShellId:          447245,
		Command:          "npm run build",
		WorkingDirectory: `c:\Users\dev\project`,
		Pid:              proto.Uint32(3300),
		MsToWait:         proto.Int32(msToWait),
		Reason:           reason.Enum(),
	}
}

func pendingShell(argsJSON string, stdout string, stderr string) runtimecore.PendingExec {
	return runtimecore.PendingExec{
		MessageID:    7,
		ExecID:       "exec-shell-1",
		ExecKind:     "shell",
		ToolCallID:   "call-shell-1",
		ArgsJSON:     []byte(argsJSON),
		StdoutBuffer: stdout,
		StderrBuffer: stderr,
	}
}

func TestShellBackgroundedTextTellsTheModelHowToFollowTheJob(t *testing.T) {
	pending := pendingShell(`{"command":"npm run build","block_until_ms":30000}`, "compiling module a\ncompiling module b\n", "")

	text := ShellBackgroundedText(backgroundedEvent(30000, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT), pending, backgroundedTerminalFile)

	want := "The command did not complete in 30000ms and was sent to the background.\n" +
		"Shell ID: 447245\n" +
		"PID: 3300\n" +
		"The output is being written to " + backgroundedTerminalFile + ". Don't mention Shell ID to the user.\n" +
		"\n" +
		"Output collected before backgrounding:\n" +
		"\n" +
		"```\n" +
		"compiling module a\n" +
		"compiling module b\n" +
		"```"
	if text != want {
		t.Fatalf("backgrounded text:\n%s\n\nwant:\n%s", text, want)
	}
}

func TestShellBackgroundedTextKeepsStderrApart(t *testing.T) {
	pending := pendingShell(`{"command":"npm run build"}`, "compiling\n", "warning: deprecated flag\n")

	text := ShellBackgroundedText(backgroundedEvent(30000, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT), pending, backgroundedTerminalFile)

	if !strings.Contains(text, "compiling\n\n<stderr>\nwarning: deprecated flag\n</stderr>") {
		t.Fatalf("stderr must follow stdout in its own block, as in a finished Shell result:\n%s", text)
	}
}

func TestShellBackgroundedTextSaysWhenNothingWasPrinted(t *testing.T) {
	pending := pendingShell(`{"command":"sleep 600"}`, "", "")

	text := ShellBackgroundedText(backgroundedEvent(30000, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT), pending, backgroundedTerminalFile)

	if !strings.HasSuffix(text, "\n\nNo output was collected before backgrounding.") {
		t.Fatalf("an empty run must say so instead of showing an empty block:\n%s", text)
	}
	if strings.Contains(text, "```") {
		t.Fatalf("no output block expected when nothing was printed:\n%s", text)
	}
}

// The user moved the command to the background by hand: the model is told so
// and told not to sit waiting on it.
func TestShellBackgroundedTextForAUserRequest(t *testing.T) {
	pending := pendingShell(`{"command":"npm run dev","block_until_ms":120000}`, "ready on :3000\n", "")

	text := ShellBackgroundedText(backgroundedEvent(1200, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_USER_REQUEST), pending, backgroundedTerminalFile)

	first, _, _ := strings.Cut(text, "\n")
	want := "The user manually backgrounded the command after 1200ms. Do NOT await it; either continue with other work or end your turn."
	if first != want {
		t.Fatalf("first line = %q\nwant        %q", first, want)
	}
	if !strings.Contains(text, "Shell ID: 447245\n") || !strings.Contains(text, "ready on :3000") {
		t.Fatalf("the shell and its output are still reported:\n%s", text)
	}
}

// block_until_ms 0 asks for a background start. Nothing "did not complete".
func TestShellBackgroundedTextForACommandStartedInTheBackground(t *testing.T) {
	pending := pendingShell(`{"command":"npm run dev","block_until_ms":0}`, "", "")
	event := backgroundedEvent(0, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT)
	event.Command = "npm run dev"

	text := ShellBackgroundedText(event, pending, backgroundedTerminalFile)

	want := "Background command started successfully.\n" +
		"Shell ID: 447245\n" +
		"PID: 3300\n" +
		"Command: npm run dev\n" +
		"Output will be written to " + backgroundedTerminalFile + ". Don't mention Shell ID to the user."
	if text != want {
		t.Fatalf("background start text:\n%s\n\nwant:\n%s", text, want)
	}
}

func TestShellBackgroundedTextWithoutPidOrKnownTerminalFile(t *testing.T) {
	pending := pendingShell(`{"command":"npm run build"}`, "", "")
	event := backgroundedEvent(30000, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT)
	event.Pid = nil

	text := ShellBackgroundedText(event, pending, "")

	if strings.Contains(text, "PID:") {
		t.Fatalf("no PID line when the client reported none:\n%s", text)
	}
	if !strings.Contains(text, "The output is being written to <terminals_folder>/447245.txt.") {
		t.Fatalf("an unknown folder is named the way the client names it:\n%s", text)
	}
}

// What the command printed in the foreground can be large; the result that is
// replayed to the model on every later pass must stay bounded.
func TestShellBackgroundedTextBoundsCollectedOutput(t *testing.T) {
	huge := strings.Repeat("0123456789abcdef\n", 20_000)
	pending := pendingShell(`{"command":"yes"}`, huge, "")

	text := ShellBackgroundedText(backgroundedEvent(30000, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT), pending, backgroundedTerminalFile)

	if len(text) > shellReplayStreamLimit+2048 {
		t.Fatalf("backgrounded text is %d bytes for %d bytes of output; want it bounded near %d", len(text), len(huge), shellReplayStreamLimit)
	}
	if !strings.Contains(text, "Output collected before backgrounding (truncated):") {
		t.Fatalf("truncated output must be labelled as such:\n%.400s", text)
	}
}

// The bridge answers with this text for the stream event itself, so every
// caller gets it; the folder is filled in by whoever knows it.
func TestShellStreamBackgroundedResultUsesTheBackgroundedText(t *testing.T) {
	pending := pendingShell(`{"command":"npm run build","block_until_ms":30000}`, "compiling module a\n", "")
	event := backgroundedEvent(30000, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT)

	result, err := NewBridge().ApplyExecClientMessage(&agentv1.ExecClientMessage{
		Id: pending.MessageID, ExecId: pending.ExecID,
		Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{
			Event: &agentv1.ShellStream_Backgrounded{Backgrounded: event},
		}},
	}, pending)
	if err != nil {
		t.Fatal(err)
	}

	if !result.IsTerminal {
		t.Fatal("a backgrounded event ends the Shell call")
	}
	if want := ShellBackgroundedText(event, pending, ""); result.ToolResultPayload != want {
		t.Fatalf("tool result:\n%s\n\nwant:\n%s", result.ToolResultPayload, want)
	}
	if !strings.Contains(result.ToolResultPayload, "compiling module a") || !strings.Contains(result.ToolResultPayload, "Shell ID: 447245") {
		t.Fatalf("tool result lost the shell or its output:\n%s", result.ToolResultPayload)
	}
	shell := result.ToolCall.GetShellToolCall()
	if !shell.GetResult().GetIsBackground() || shell.GetResult().GetSuccess().GetShellId() != 447245 {
		t.Fatalf("the Shell card must still show a running background shell: %+v", shell.GetResult())
	}
}

// The model is normally sent the card's result, not the text. The card has to
// carry what the command printed before it was backgrounded, as the client's
// own result does; it used to carry an empty output.
func TestShellBackgroundedCardCarriesTheOutputCollectedSoFar(t *testing.T) {
	pending := pendingShell(`{"command":"npm run build","block_until_ms":30000}`, "compiling module a\n", "warning: deprecated flag\n")
	event := backgroundedEvent(30000, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT)

	result, err := NewBridge().ApplyExecClientMessage(&agentv1.ExecClientMessage{
		Id: pending.MessageID, ExecId: pending.ExecID,
		Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{
			Event: &agentv1.ShellStream_Backgrounded{Backgrounded: event},
		}},
	}, pending)
	if err != nil {
		t.Fatal(err)
	}

	success := result.ToolCall.GetShellToolCall().GetResult().GetSuccess()
	if output := success.GetInterleavedOutput(); !strings.Contains(output, "compiling module a") || !strings.Contains(output, "warning: deprecated flag") {
		t.Fatalf("card output = %q, want what the command printed before it was backgrounded", output)
	}
	if success.GetBackgroundReason() != agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT {
		t.Fatalf("card background reason = %v, want the client's reason", success.GetBackgroundReason())
	}
	if success.GetMsToWait() != 30000 || success.GetPid() != 3300 {
		t.Fatalf("card lost the wait or the pid: %+v", success)
	}
}

func TestShellBackgroundedCardBoundsTheCollectedOutput(t *testing.T) {
	huge := strings.Repeat("0123456789abcdef\n", 20_000)
	pending := pendingShell(`{"command":"yes"}`, huge, "")

	result, err := NewBridge().ApplyExecClientMessage(&agentv1.ExecClientMessage{
		Id: pending.MessageID, ExecId: pending.ExecID,
		Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{
			Event: &agentv1.ShellStream_Backgrounded{Backgrounded: backgroundedEvent(30000, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT)},
		}},
	}, pending)
	if err != nil {
		t.Fatal(err)
	}

	output := result.ToolCall.GetShellToolCall().GetResult().GetSuccess().GetInterleavedOutput()
	if len(output) == 0 || len(output) > 2*shellReplayStreamLimit {
		t.Fatalf("card output is %d bytes for %d bytes printed; want it present and bounded by %d", len(output), len(huge), 2*shellReplayStreamLimit)
	}
}

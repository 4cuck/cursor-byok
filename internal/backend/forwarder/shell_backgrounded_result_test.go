package forwarder

import (
	"encoding/json"
	"strings"
	"testing"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/proto"
)

// When a Shell command is sent to the background, its result is all the model
// learns about the job. The result is stored twice: as the Shell card, which
// is what the model is normally sent, and as text, which it is sent when the
// card cannot be used. Both have to say where the output goes -- the
// forwarder knows the terminals folder -- and carry what the command printed
// so far, or the model's next step is a guess.

func shellStreamMessage(execID string, messageID uint32, event *agentv1.ShellStream) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{
		Id:      messageID,
		ExecId:  execID,
		Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: event},
	}
}

func toolResultText(t *testing.T, stream *ActiveStream, callID string) (string, bool) {
	t.Helper()
	for _, entry := range snapshotHistoryEntries(stream) {
		if entry.Kind != "tool_result" || entry.ToolCallID != callID {
			continue
		}
		var payload toolResultEntryPayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("decode tool_result payload: %v", err)
		}
		return payload.ResultText, true
	}
	return "", false
}

// The text form.
func TestBackgroundedShellResultNamesTheTerminalFileAndKeepsTheOutput(t *testing.T) {
	const terminalsFolder = `C:\Users\dev\.cursor\projects\p\terminals`
	service, stream := newAwaitTestStream(t, terminalsFolder)
	pending := addPendingShell(stream, "exec-shell-bg", 71)

	for _, event := range []*agentv1.ShellStream{
		{Event: &agentv1.ShellStream_Start{Start: &agentv1.ShellStreamStart{}}},
		{Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: "compiling module a\n"}}},
		{Event: &agentv1.ShellStream_Backgrounded{Backgrounded: &agentv1.ShellStreamBackgrounded{
			ShellId:  447245,
			Command:  "npm run build",
			Pid:      proto.Uint32(3300),
			MsToWait: proto.Int32(30000),
			Reason:   agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.Enum(),
		}}},
	} {
		if err := service.handleExecResult(InboundIntent{
			Kind:              "exec_result",
			RequestID:         awaitTestRequestID,
			ExecClientMessage: shellStreamMessage(pending.ExecID, pending.MessageID, event),
		}); err != nil {
			t.Fatalf("handleExecResult(%T) error = %v", event.GetEvent(), err)
		}
	}

	text, ok := toolResultText(t, stream, pending.ToolCallID)
	if !ok {
		t.Fatal("the backgrounded Shell call has no tool result")
	}
	for _, want := range []string{
		"The command did not complete in 30000ms and was sent to the background.",
		"Shell ID: 447245",
		"PID: 3300",
		"The output is being written to " + terminalsFolder + "/447245.txt.",
		"compiling module a",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("tool result is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<terminals_folder>") {
		t.Fatalf("the terminals folder is known here and must be filled in:\n%s", text)
	}
}

// modelToolMessage returns the tool message the model is sent for a call: the
// prompt is projected from the history, and for a call with a stored card the
// message is that card's result, not the result text.
func modelToolMessage(t *testing.T, service *Service, stream *ActiveStream, callID string) string {
	t.Helper()
	conversation, _, _, err := service.snapshotCheckpointConversation(stream)
	if err != nil {
		t.Fatalf("snapshotCheckpointConversation() error = %v", err)
	}
	messages, err := NewHistoryProjector().ProjectPromptReplay(conversation)
	if err != nil {
		t.Fatalf("ProjectPromptReplay() error = %v", err)
	}
	for _, message := range messages {
		if message.Role == "tool" && message.ToolCallID == callID {
			return message.Content
		}
	}
	t.Fatalf("the prompt has no tool message for %s", callID)
	return ""
}

// What the model is actually sent for a backgrounded Shell call has to carry
// the output the command printed before it was backgrounded, and say where
// the rest of it goes. It used to carry neither: the shell id, and nothing the
// command had printed.
func TestModelIsSentTheOutputAndTerminalsFolderOfABackgroundedShell(t *testing.T) {
	const terminalsFolder = `C:\Users\dev\.cursor\projects\p\terminals`
	service, stream := newAwaitTestStream(t, terminalsFolder)
	pending := addPendingShell(stream, "exec-shell-bg3", 73)

	for _, event := range []*agentv1.ShellStream{
		{Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: "compiling module a\n"}}},
		{Event: &agentv1.ShellStream_Stderr{Stderr: &agentv1.ShellStreamStderr{Data: "warning: deprecated flag\n"}}},
		{Event: &agentv1.ShellStream_Backgrounded{Backgrounded: &agentv1.ShellStreamBackgrounded{
			ShellId:  447245,
			Command:  "npm run build",
			Pid:      proto.Uint32(3300),
			MsToWait: proto.Int32(30000),
			Reason:   agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.Enum(),
		}}},
	} {
		if err := service.handleExecResult(InboundIntent{
			Kind:              "exec_result",
			RequestID:         awaitTestRequestID,
			ExecClientMessage: shellStreamMessage(pending.ExecID, pending.MessageID, event),
		}); err != nil {
			t.Fatalf("handleExecResult(%T) error = %v", event.GetEvent(), err)
		}
	}

	content := modelToolMessage(t, service, stream, pending.ToolCallID)
	var sent struct {
		IsBackground    bool   `json:"isBackground"`
		TerminalsFolder string `json:"terminalsFolder"`
		Success         struct {
			ShellID           uint32 `json:"shellId"`
			MsToWait          int32  `json:"msToWait"`
			InterleavedOutput string `json:"interleavedOutput"`
			BackgroundReason  string `json:"backgroundReason"`
		} `json:"success"`
	}
	if err := json.Unmarshal([]byte(content), &sent); err != nil {
		t.Fatalf("the model's tool message is not the Shell result: %v\n%s", err, content)
	}
	if !sent.IsBackground || sent.Success.ShellID != 447245 || sent.Success.MsToWait != 30000 {
		t.Fatalf("the model is not told the command is still running as shell 447245:\n%s", content)
	}
	if !strings.Contains(sent.Success.InterleavedOutput, "compiling module a") || !strings.Contains(sent.Success.InterleavedOutput, "warning: deprecated flag") {
		t.Fatalf("the model is not sent what the command printed before it was backgrounded:\n%s", content)
	}
	if sent.TerminalsFolder != terminalsFolder {
		t.Fatalf("terminals folder sent to the model = %q, want %q (the output goes to <folder>/447245.txt)", sent.TerminalsFolder, terminalsFolder)
	}
	if sent.Success.BackgroundReason != agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.String() {
		t.Fatalf("background reason sent to the model = %q, want the client's reason", sent.Success.BackgroundReason)
	}
}

// The same holds for AwaitShell: what the model is sent is the card's result.
// It has to say whether the command ended, with which exit code, and where
// its output is.
func TestModelIsSentWhetherTheAwaitedShellEnded(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", runningTerminalFile)

	invokeAwaitShell(t, service, stream, "call-still-running", `{"shell_id":"447245","block_until_ms":0}`)
	var running struct {
		Success struct {
			StillRunning *struct {
				TaskID         string `json:"taskId"`
				OutputFilePath string `json:"outputFilePath"`
			} `json:"stillRunning"`
			Complete *struct{} `json:"complete"`
		} `json:"success"`
	}
	content := modelToolMessage(t, service, stream, "call-still-running")
	if err := json.Unmarshal([]byte(content), &running); err != nil {
		t.Fatalf("the model's AwaitShell message is not the await result: %v\n%s", err, content)
	}
	if running.Success.StillRunning == nil || running.Success.Complete != nil || running.Success.StillRunning.TaskID != "447245" {
		t.Fatalf("the model is not told shell 447245 is still running:\n%s", content)
	}

	writeTerminalFile(t, folder, "447245", strings.Replace(finishedTerminalFile, "exit_code: 0\n", "exit_code: 3\n", 1))
	invokeAwaitShell(t, service, stream, "call-ended", `{"shell_id":"447245","block_until_ms":60000}`)
	var ended struct {
		Success struct {
			Complete *struct {
				TaskID         string `json:"taskId"`
				ExitCode       *int32 `json:"exitCode"`
				OutputFilePath string `json:"outputFilePath"`
			} `json:"complete"`
		} `json:"success"`
	}
	content = modelToolMessage(t, service, stream, "call-ended")
	if err := json.Unmarshal([]byte(content), &ended); err != nil {
		t.Fatalf("the model's AwaitShell message is not the await result: %v\n%s", err, content)
	}
	complete := ended.Success.Complete
	if complete == nil || complete.ExitCode == nil || *complete.ExitCode != 3 {
		t.Fatalf("the model is not told the command ended with exit code 3:\n%s", content)
	}
	if want := strings.TrimSuffix(folder, "/") + "/447245.txt"; complete.OutputFilePath != want {
		t.Fatalf("output file sent to the model = %q, want %q", complete.OutputFilePath, want)
	}
}

// The shell id in that result is what AwaitShell is called with next.
func TestAwaitShellAcceptsTheShellAnnouncedByABackgroundedResult(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	pending := addPendingShell(stream, "exec-shell-bg2", 72)
	if err := service.handleExecResult(InboundIntent{
		Kind:      "exec_result",
		RequestID: awaitTestRequestID,
		ExecClientMessage: shellStreamMessage(pending.ExecID, pending.MessageID, &agentv1.ShellStream{
			Event: &agentv1.ShellStream_Backgrounded{Backgrounded: &agentv1.ShellStreamBackgrounded{
				ShellId:  447245,
				Command:  "npm run build",
				MsToWait: proto.Int32(30000),
			}},
		}),
	}); err != nil {
		t.Fatalf("handleExecResult(backgrounded) error = %v", err)
	}
	text, _ := toolResultText(t, stream, pending.ToolCallID)
	terminalFile := writeTerminalFile(t, folder, "447245", finishedTerminalFile)
	if !strings.Contains(text, strings.TrimSuffix(folder, "/")+"/447245.txt") {
		t.Fatalf("tool result does not name the terminal file %s:\n%s", terminalFile, text)
	}

	invokeAwaitShell(t, service, stream, "call-follow-up", `{"shell_id":"447245","block_until_ms":60000}`)

	result := requireAwaitResult(t, stream, "call-follow-up").Result
	if result.Status != backgroundShellStatusCompleted || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("AwaitShell on the announced shell = %+v, want completed with exit 0", result)
	}
	if result.OutputFilePath != strings.TrimSuffix(folder, "/")+"/447245.txt" {
		t.Fatalf("AwaitShell output_file_path = %q, want the file the Shell result announced", result.OutputFilePath)
	}
}

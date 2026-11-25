package forwarder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/encoding/protojson"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const awaitTestRequestID = "req-await"

// remoteTerminalsFolder is a folder this process cannot read: the shell lives
// on another host (WSL / SSH), so only the client can see its terminal file.
const remoteTerminalsFolder = "/home/dev/.cursor/projects/remote-project/terminals"

func newAwaitTestStream(t *testing.T, terminalsFolder string) (*Service, *ActiveStream) {
	t.Helper()
	broker := NewStreamBroker()
	stream, err := broker.OpenStream(awaitTestRequestID, "conv-await", 1, "m", "m", agentv1.AgentMode_AGENT_MODE_AGENT, "run the build")
	if err != nil {
		t.Fatalf("OpenStream() error = %v", err)
	}
	setupCheckpointConversation(stream)
	stream.mu.Lock()
	stream.TerminalsFolder = terminalsFolder
	stream.TimerTokens = make(map[string]uint64)
	stream.StreamTimers = make(map[string]*time.Timer)
	stream.mu.Unlock()
	service := newTestService(broker, func() time.Duration { return time.Second }, nil)
	t.Cleanup(func() {
		stream.mu.Lock()
		stopAllStreamTimersLocked(stream)
		stream.mu.Unlock()
	})
	return service, stream
}

func invokeAwaitShell(t *testing.T, service *Service, stream *ActiveStream, callID string, argsJSON string) {
	t.Helper()
	err := service.handleAwaitShellToolInvocation(stream, runtimecore.ToolInvocation{
		CallID:      callID,
		ToolName:    "AwaitShell",
		ArgsJSON:    []byte(argsJSON),
		ModelCallID: "mc-await",
	})
	if err != nil {
		t.Fatalf("handleAwaitShellToolInvocation() error = %v", err)
	}
}

// tickAwaitShell runs one poll for a deferred wait, as the stream timer would.
func tickAwaitShell(t *testing.T, service *Service, stream *ActiveStream, callID string) {
	t.Helper()
	if err := service.handleAwaitShellTimer(stream, &streamTimerEvent{
		Key:  awaitShellTimerKey(callID),
		Kind: streamTimerAwaitShell,
	}); err != nil {
		t.Fatalf("handleAwaitShellTimer() error = %v", err)
	}
}

type awaitToolResultEntry struct {
	Result   awaitShellResult
	ToolCall *agentv1.ToolCall
}

func findAwaitToolResult(t *testing.T, stream *ActiveStream, callID string) (awaitToolResultEntry, bool) {
	t.Helper()
	for _, entry := range snapshotHistoryEntries(stream) {
		if entry.Kind != "tool_result" || entry.ToolCallID != callID {
			continue
		}
		var payload toolResultEntryPayload
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("decode tool_result payload: %v", err)
		}
		if payload.ToolName != "AwaitShell" {
			t.Fatalf("tool_result for %s has tool_name %q, want AwaitShell", callID, payload.ToolName)
		}
		found := awaitToolResultEntry{}
		if err := json.Unmarshal([]byte(payload.ResultText), &found.Result); err != nil {
			t.Fatalf("AwaitShell result is not JSON: %v\n%s", err, payload.ResultText)
		}
		if len(payload.ToolCall) > 0 {
			found.ToolCall = &agentv1.ToolCall{}
			if err := protojson.Unmarshal(payload.ToolCall, found.ToolCall); err != nil {
				t.Fatalf("decode AwaitShell tool call card: %v", err)
			}
		}
		return found, true
	}
	return awaitToolResultEntry{}, false
}

func requireNoAwaitResult(t *testing.T, stream *ActiveStream, callID string, why string) {
	t.Helper()
	if found, ok := findAwaitToolResult(t, stream, callID); ok {
		t.Fatalf("%s, but AwaitShell already returned: %+v", why, found.Result)
	}
}

func requireAwaitResult(t *testing.T, stream *ActiveStream, callID string) awaitToolResultEntry {
	t.Helper()
	found, ok := findAwaitToolResult(t, stream, callID)
	if !ok {
		t.Fatalf("AwaitShell %s has not returned a result", callID)
	}
	return found
}

func waitForAwaitResult(t *testing.T, stream *ActiveStream, callID string, within time.Duration) awaitToolResultEntry {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if found, ok := findAwaitToolResult(t, stream, callID); ok {
			return found
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("AwaitShell %s did not return within %s", callID, within)
	return awaitToolResultEntry{}
}

func writeTerminalFile(t *testing.T, folder string, shellID string, content string) string {
	t.Helper()
	path := filepath.Join(folder, shellID+".txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write terminal file: %v", err)
	}
	return path
}

// publishedAwaitReads returns the read requests sent to the client so far.
func publishedAwaitReads(t *testing.T, service *Service) []*agentv1.ExecServerMessage {
	t.Helper()
	events, err := service.broker.ReadFromCursor(awaitTestRequestID, 0)
	if err != nil {
		t.Fatalf("ReadFromCursor() error = %v", err)
	}
	reads := make([]*agentv1.ExecServerMessage, 0, 4)
	for _, event := range events {
		if exec := event.Message.GetExecServerMessage(); exec != nil && exec.GetReadArgs() != nil {
			reads = append(reads, exec)
		}
	}
	return reads
}

func answerAwaitRead(t *testing.T, service *Service, read *agentv1.ExecServerMessage, result *agentv1.ReadResult) {
	t.Helper()
	if err := service.handleExecResult(InboundIntent{
		Kind:      "exec_result",
		RequestID: awaitTestRequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id:      read.GetId(),
			ExecId:  read.GetExecId(),
			Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: result},
		},
	}); err != nil {
		t.Fatalf("handleExecResult(read) error = %v", err)
	}
}

func readSuccess(content string) *agentv1.ReadResult {
	return &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
		TotalLines: int32(strings.Count(content, "\n") + 1),
		FileSize:   int64(len(content)),
		Output:     &agentv1.ReadSuccess_Content{Content: content},
	}}}
}

// ---------------------------------------------------------------------------
// Arguments
// ---------------------------------------------------------------------------

func TestDecodeAwaitShellArgsNormalizesLikeTheClient(t *testing.T) {
	cases := []struct {
		name      string
		args      string
		wantID    string
		wantBlock int64
	}{
		{"string id", `{"shell_id":"447245","block_until_ms":5000}`, "447245", 5000},
		{"numeric id is the same shell", `{"shell_id":447245,"block_until_ms":5000}`, "447245", 5000},
		{"task_id alias", `{"task_id":"447245"}`, "447245", 30000},
		{"camelCase alias", `{"shellId":447245}`, "447245", 30000},
		{"none means sleep only", `{"shell_id":"none","block_until_ms":1000}`, "", 1000},
		{"blank id means sleep only", `{"shell_id":"  ","block_until_ms":1000}`, "", 1000},
		{"block defaults to 30s", `{"shell_id":"1"}`, "1", 30000},
		{"float literal block", `{"shell_id":"1","block_until_ms":120000.0}`, "1", 120000},
		{"fractional block is floored", `{"shell_id":"1","block_until_ms":1500.9}`, "1", 1500},
		{"negative block falls back to the default", `{"shell_id":"1","block_until_ms":-5}`, "1", 30000},
		{"non numeric block falls back to the default", `{"shell_id":"1","block_until_ms":"soon"}`, "1", 30000},
		{"numeric string block", `{"shell_id":"1","block_until_ms":"2500"}`, "1", 2500},
		{"block is capped at the per-call limit", `{"shell_id":"1","block_until_ms":99999999}`, "1", 7140000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := decodeAwaitShellArgs([]byte(tc.args))
			if err != nil {
				t.Fatalf("decodeAwaitShellArgs(%s) error = %v", tc.args, err)
			}
			if args.ShellID != tc.wantID {
				t.Errorf("ShellID = %q, want %q", args.ShellID, tc.wantID)
			}
			if args.BlockUntilMS != tc.wantBlock {
				t.Errorf("BlockUntilMS = %d, want %d", args.BlockUntilMS, tc.wantBlock)
			}
		})
	}
}

func TestDecodeAwaitShellArgsRejectsNonBlockingCheckWithoutShell(t *testing.T) {
	_, err := decodeAwaitShellArgs([]byte(`{"block_until_ms":0}`))
	if err == nil {
		t.Fatal("block_until_ms 0 without a shell id has nothing to check and must be rejected")
	}
	if !strings.Contains(err.Error(), "shell_id") || !strings.Contains(err.Error(), "block_until_ms") {
		t.Fatalf("error should tell the model which argument to fix, got: %v", err)
	}
}

func TestDecodeAwaitShellArgsRejectsInvalidPattern(t *testing.T) {
	_, err := decodeAwaitShellArgs([]byte(`{"shell_id":"1","pattern":"(?=lookahead)"}`))
	if err == nil {
		t.Fatal("a pattern RE2 cannot compile must be rejected")
	}
	if !strings.Contains(err.Error(), "RE2") {
		t.Fatalf("error should name the regex dialect, got: %v", err)
	}
}

func TestAwaitShellStartedCardCarriesNumericShellID(t *testing.T) {
	card := buildStartedToolCall(runtimecore.ToolInvocation{
		CallID:   "call-card",
		ToolName: "AwaitShell",
		ArgsJSON: []byte(`{"shell_id":447245,"block_until_ms":5000,"pattern":"ready"}`),
	})
	args := card.GetAwaitToolCall().GetArgs()
	if args.GetTaskId() != "447245" {
		t.Fatalf("card task id = %q, want 447245", args.GetTaskId())
	}
	if args.GetBlockUntilMs() != 5000 {
		t.Fatalf("card block_until_ms = %d, want 5000", args.GetBlockUntilMs())
	}
	if args.GetRegex() != "ready" {
		t.Fatalf("card regex = %q, want ready", args.GetRegex())
	}
}

// ---------------------------------------------------------------------------
// Local workspace: this process can read the terminal file directly
// ---------------------------------------------------------------------------

func TestAwaitShellWaitsForBackgroundedShellUntilItExits(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", runningTerminalFile)

	invokeAwaitShell(t, service, stream, "call-wait", `{"shell_id":"447245","block_until_ms":60000}`)

	requireNoAwaitResult(t, stream, "call-wait", "the command is still running and 60s were requested")
	if got := pendingBridgeCount(stream); got != 1 {
		t.Fatalf("pendingBridgeCount = %d, want 1: an unanswered AwaitShell must hold the provider back", got)
	}

	tickAwaitShell(t, service, stream, "call-wait")
	requireNoAwaitResult(t, stream, "call-wait", "nothing changed between polls")

	path := writeTerminalFile(t, folder, "447245", finishedTerminalFile)
	tickAwaitShell(t, service, stream, "call-wait")

	got := requireAwaitResult(t, stream, "call-wait")
	if got.Result.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Result.Status)
	}
	if got.Result.ExitCode == nil || *got.Result.ExitCode != 0 {
		t.Fatalf("exit_code = %v, want 0", got.Result.ExitCode)
	}
	if got.Result.RuntimeMS != 18566 {
		t.Fatalf("runtime_ms = %d, want the footer's elapsed_ms 18566", got.Result.RuntimeMS)
	}
	if got.Result.TimedOut {
		t.Fatal("timed_out must be false when the command finished")
	}
	if !strings.HasSuffix(got.Result.OutputFilePath, "/447245.txt") {
		t.Fatalf("output_file_path = %q, want the terminal file", got.Result.OutputFilePath)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result.OutputLength != uint64(info.Size()) {
		t.Fatalf("output_length = %d, want file size %d", got.Result.OutputLength, info.Size())
	}
	if !strings.Contains(got.Result.Message, "Task completed in 18566ms with exit code: 0.") {
		t.Fatalf("message = %q, want the client's completion sentence", got.Result.Message)
	}
	if !strings.Contains(got.Result.OutputTail, "pkg/b") {
		t.Fatalf("output_tail = %q, want the end of the command output", got.Result.OutputTail)
	}
	if got := pendingBridgeCount(stream); got != 0 {
		t.Fatalf("pendingBridgeCount = %d after completion, want 0", got)
	}
}

func TestAwaitShellCompletedCardUsesTheClientResultShape(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", finishedTerminalFile)

	invokeAwaitShell(t, service, stream, "call-card", `{"shell_id":"447245","block_until_ms":60000}`)

	got := requireAwaitResult(t, stream, "call-card")
	if got.ToolCall == nil {
		t.Fatal("completed AwaitShell must carry a tool call card")
	}
	complete := got.ToolCall.GetAwaitToolCall().GetResult().GetSuccess().GetComplete()
	if complete == nil {
		t.Fatalf("card result = %v, want success.complete", got.ToolCall.GetAwaitToolCall().GetResult())
	}
	if complete.GetTaskId() != "447245" || complete.GetExitCode() != 0 || complete.ExitCode == nil {
		t.Fatalf("card complete = %v, want task 447245 with exit code 0", complete)
	}
	if !strings.HasSuffix(complete.GetOutputFilePath(), "/447245.txt") {
		t.Fatalf("card output_file_path = %q", complete.GetOutputFilePath())
	}
	if complete.GetRuntimeMs() != 18566 {
		t.Fatalf("card runtime_ms = %d, want 18566", complete.GetRuntimeMs())
	}
}

func TestAwaitShellZeroBlockReturnsStatusImmediately(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", runningTerminalFile)

	invokeAwaitShell(t, service, stream, "call-check", `{"shell_id":"447245","block_until_ms":0}`)

	got := requireAwaitResult(t, stream, "call-check")
	if got.Result.Status != "running" {
		t.Fatalf("status = %q, want running", got.Result.Status)
	}
	if got.Result.TimedOut {
		t.Fatal("a non-blocking status check did not wait, so it cannot have timed out")
	}
	if got.ToolCall.GetAwaitToolCall().GetResult().GetSuccess().GetStillRunning() == nil {
		t.Fatalf("card result = %v, want success.still_running", got.ToolCall.GetAwaitToolCall().GetResult())
	}
	if pendingBridgeCount(stream) != 0 {
		t.Fatal("a non-blocking check must not leave pending work behind")
	}
}

func TestAwaitShellReturnsWhenPatternAppearsInNewOutput(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", runningTerminalFile)

	invokeAwaitShell(t, service, stream, "call-pattern", `{"shell_id":"447245","block_until_ms":60000,"pattern":"^listening on :\\d+$"}`)
	requireNoAwaitResult(t, stream, "call-pattern", "the pattern is not in the output yet")

	writeTerminalFile(t, folder, "447245", runningTerminalFile+"listening on :3000\n")
	tickAwaitShell(t, service, stream, "call-pattern")

	got := requireAwaitResult(t, stream, "call-pattern")
	if got.Result.Status != "running" {
		t.Fatalf("status = %q, want running: a match does not mean the command ended", got.Result.Status)
	}
	if !got.Result.Matched || got.Result.RegexMatch == nil || *got.Result.RegexMatch != "listening on :3000" {
		t.Fatalf("matched=%t regex_match=%v, want the matched line", got.Result.Matched, got.Result.RegexMatch)
	}
	if got.Result.TimedOut {
		t.Fatal("timed_out must be false when the pattern matched")
	}
	if !strings.Contains(got.Result.Message, "Pattern matched: listening on :3000") {
		t.Fatalf("message = %q, want the match reported", got.Result.Message)
	}
}

func TestAwaitShellPatternNeverMatchesTerminalMetadata(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", finishedTerminalFile)

	invokeAwaitShell(t, service, stream, "call-meta", `{"shell_id":"447245","block_until_ms":60000,"pattern":"exit_code|running_for_ms|elapsed_ms"}`)

	got := requireAwaitResult(t, stream, "call-meta")
	if got.Result.Matched {
		t.Fatalf("pattern matched header/footer metadata: %v", got.Result.RegexMatch)
	}
	if got.Result.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Result.Status)
	}
	if !strings.Contains(got.Result.Message, "Pattern did NOT match.") {
		t.Fatalf("message = %q, want the missed pattern reported", got.Result.Message)
	}
}

func TestAwaitShellStillRunningAfterDeadlineReportsTimeout(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", runningTerminalFile)

	started := time.Now()
	invokeAwaitShell(t, service, stream, "call-timeout", `{"shell_id":"447245","block_until_ms":150}`)
	requireNoAwaitResult(t, stream, "call-timeout", "150ms have not passed yet")

	got := waitForAwaitResult(t, stream, "call-timeout", 5*time.Second)
	if waited := time.Since(started); waited < 140*time.Millisecond {
		t.Fatalf("AwaitShell returned after %s, want it to block for the requested 150ms", waited)
	}
	if got.Result.Status != "running" || !got.Result.TimedOut {
		t.Fatalf("status=%q timed_out=%t, want running/true", got.Result.Status, got.Result.TimedOut)
	}
	if !strings.Contains(got.Result.Message, "Task still running after") {
		t.Fatalf("message = %q, want the client's still-running sentence", got.Result.Message)
	}
}

func TestAwaitShellSleepOnlyWaitsForTheRequestedTime(t *testing.T) {
	service, stream := newAwaitTestStream(t, t.TempDir())

	started := time.Now()
	invokeAwaitShell(t, service, stream, "call-sleep", `{"block_until_ms":150}`)

	requireNoAwaitResult(t, stream, "call-sleep", "a sleep must not return before it slept")
	if got := pendingBridgeCount(stream); got != 1 {
		t.Fatalf("pendingBridgeCount = %d, want 1 while sleeping", got)
	}

	got := waitForAwaitResult(t, stream, "call-sleep", 5*time.Second)
	if waited := time.Since(started); waited < 140*time.Millisecond {
		t.Fatalf("sleep returned after %s, want at least the requested 150ms", waited)
	}
	if got.Result.Status != "waited" {
		t.Fatalf("status = %q, want waited", got.Result.Status)
	}
	if got.Result.Message != "Slept for 1s." {
		t.Fatalf("message = %q, want the client's sleep sentence", got.Result.Message)
	}
	complete := got.ToolCall.GetAwaitToolCall().GetResult().GetSuccess().GetComplete()
	if complete == nil || complete.GetTaskId() != "" {
		t.Fatalf("sleep card = %v, want success.complete with an empty task id", got.ToolCall.GetAwaitToolCall().GetResult())
	}
}

func TestAwaitShellUnknownLocalShellFailsFast(t *testing.T) {
	service, stream := newAwaitTestStream(t, t.TempDir())

	invokeAwaitShell(t, service, stream, "call-unknown", `{"shell_id":"999","block_until_ms":60000}`)

	got := requireAwaitResult(t, stream, "call-unknown")
	if got.Result.Status != "error" {
		t.Fatalf("status = %q, want error", got.Result.Status)
	}
	if !strings.Contains(got.Result.Message, "No shell found for id 999") {
		t.Fatalf("message = %q, want the client's unknown-shell error", got.Result.Message)
	}
	if got.ToolCall.GetAwaitToolCall().GetResult().GetError() == nil {
		t.Fatalf("card result = %v, want error", got.ToolCall.GetAwaitToolCall().GetResult())
	}
	if pendingBridgeCount(stream) != 0 {
		t.Fatal("a failed AwaitShell must not leave pending work behind")
	}
}

func TestAwaitShellNumericShellIDIsNotASleep(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", finishedTerminalFile)

	invokeAwaitShell(t, service, stream, "call-numeric", `{"shell_id":447245,"block_until_ms":60000.0}`)

	got := requireAwaitResult(t, stream, "call-numeric")
	if got.Result.Status != "completed" || got.Result.ShellID != "447245" {
		t.Fatalf("status=%q shell_id=%q, want the finished shell 447245", got.Result.Status, got.Result.ShellID)
	}
}

// ---------------------------------------------------------------------------
// Remote workspace: only the client can read the terminal file
// ---------------------------------------------------------------------------

func TestAwaitShellPollsRemoteTerminalFileThroughTheClient(t *testing.T) {
	service, stream := newAwaitTestStream(t, remoteTerminalsFolder)

	invokeAwaitShell(t, service, stream, "call-remote", `{"shell_id":"447245","block_until_ms":60000}`)

	reads := publishedAwaitReads(t, service)
	if len(reads) != 1 {
		t.Fatalf("published %d read requests, want 1", len(reads))
	}
	if got, want := reads[0].GetReadArgs().GetPath(), remoteTerminalsFolder+"/447245.txt"; got != want {
		t.Fatalf("read path = %q, want %q", got, want)
	}
	requireNoAwaitResult(t, stream, "call-remote", "the client has not answered yet")
	if got := pendingBridgeCount(stream); got == 0 {
		t.Fatal("an unanswered AwaitShell must hold the provider back")
	}

	answerAwaitRead(t, service, reads[0], readSuccess(runningTerminalFile))
	requireNoAwaitResult(t, stream, "call-remote", "the client reported the command still running")

	tickAwaitShell(t, service, stream, "call-remote")
	reads = publishedAwaitReads(t, service)
	if len(reads) != 2 {
		t.Fatalf("published %d read requests after the next poll, want 2", len(reads))
	}

	answerAwaitRead(t, service, reads[1], readSuccess(finishedTerminalFile))

	got := requireAwaitResult(t, stream, "call-remote")
	if got.Result.Status != "completed" || got.Result.ExitCode == nil || *got.Result.ExitCode != 0 {
		t.Fatalf("status=%q exit=%v, want completed/0", got.Result.Status, got.Result.ExitCode)
	}
	if got.Result.OutputFilePath != remoteTerminalsFolder+"/447245.txt" {
		t.Fatalf("output_file_path = %q", got.Result.OutputFilePath)
	}
	if got.Result.OutputLength != uint64(len(finishedTerminalFile)) {
		t.Fatalf("output_length = %d, want the reported file size %d", got.Result.OutputLength, len(finishedTerminalFile))
	}
	if pendingBridgeCount(stream) != 0 {
		t.Fatalf("pendingBridgeCount = %d after completion, want 0", pendingBridgeCount(stream))
	}
}

func TestAwaitShellRemoteFollowUpReadsOnlyRequestTheTail(t *testing.T) {
	service, stream := newAwaitTestStream(t, remoteTerminalsFolder)

	invokeAwaitShell(t, service, stream, "call-tail", `{"shell_id":"447245","block_until_ms":60000}`)
	reads := publishedAwaitReads(t, service)
	if reads[0].GetReadArgs().Offset != nil {
		t.Fatalf("first read offset = %d, want the whole file so old output and the header are seen once", reads[0].GetReadArgs().GetOffset())
	}
	answerAwaitRead(t, service, reads[0], readSuccess(runningTerminalFile))

	tickAwaitShell(t, service, stream, "call-tail")
	reads = publishedAwaitReads(t, service)
	if len(reads) != 2 {
		t.Fatalf("published %d read requests, want 2", len(reads))
	}
	if reads[1].GetReadArgs().Offset == nil || reads[1].GetReadArgs().GetOffset() >= 0 {
		t.Fatalf("follow-up read offset = %v, want a negative (tail) offset so long logs are not re-sent every poll", reads[1].GetReadArgs().Offset)
	}
}

func TestAwaitShellRemotePatternMatchesAcrossPolls(t *testing.T) {
	service, stream := newAwaitTestStream(t, remoteTerminalsFolder)

	invokeAwaitShell(t, service, stream, "call-rp", `{"shell_id":"447245","block_until_ms":60000,"pattern":"server ready"}`)
	reads := publishedAwaitReads(t, service)
	answerAwaitRead(t, service, reads[0], readSuccess(runningTerminalFile))
	requireNoAwaitResult(t, stream, "call-rp", "the pattern is not in the output yet")

	tickAwaitShell(t, service, stream, "call-rp")
	reads = publishedAwaitReads(t, service)
	answerAwaitRead(t, service, reads[1], readSuccess("compiling module b\nserver ready\n"))

	got := requireAwaitResult(t, stream, "call-rp")
	if !got.Result.Matched || got.Result.Status != "running" {
		t.Fatalf("matched=%t status=%q, want a match on a still-running command", got.Result.Matched, got.Result.Status)
	}
}

// Follow-up polls only see the last lines of the file. That window can begin
// on a dash rule the command printed; the footer behind it still ends the wait.
func TestAwaitShellRemoteCompletesWhenTheTailWindowStartsOnADashRule(t *testing.T) {
	service, stream := newAwaitTestStream(t, remoteTerminalsFolder)

	invokeAwaitShell(t, service, stream, "call-yaml", `{"shell_id":"447245","block_until_ms":60000}`)
	reads := publishedAwaitReads(t, service)
	answerAwaitRead(t, service, reads[0], readSuccess(runningTerminalFile))
	tickAwaitShell(t, service, stream, "call-yaml")
	reads = publishedAwaitReads(t, service)

	answerAwaitRead(t, service, reads[1], readSuccess(
		"---\n"+
			"kind: Deployment\n"+
			"metadata:\n"+
			"  name: web\n"+
			"\n"+
			"---\n"+
			"exit_code: 0\n"+
			"elapsed_ms: 61000\n"+
			"ended_at: 2026-11-25T08:49:50.511Z\n"+
			"---\n"))

	got := requireAwaitResult(t, stream, "call-yaml")
	if got.Result.Status != "completed" || got.Result.ExitCode == nil || *got.Result.ExitCode != 0 {
		t.Fatalf("status=%q exit=%v, want completed/0", got.Result.Status, got.Result.ExitCode)
	}
	if !strings.Contains(got.Result.OutputTail, "kind: Deployment") {
		t.Fatalf("output_tail = %q, want the command output", got.Result.OutputTail)
	}
}

// A window that begins inside the header carries metadata lines such as
// "status: running". A pattern is only ever matched against the output.
func TestAwaitShellRemotePatternIgnoresHeaderLinesInATailWindow(t *testing.T) {
	service, stream := newAwaitTestStream(t, remoteTerminalsFolder)

	invokeAwaitShell(t, service, stream, "call-hdr", `{"shell_id":"447245","block_until_ms":60000,"pattern":"running|ready"}`)
	reads := publishedAwaitReads(t, service)
	answerAwaitRead(t, service, reads[0], readSuccess(runningTerminalFile))
	tickAwaitShell(t, service, stream, "call-hdr")
	reads = publishedAwaitReads(t, service)

	answerAwaitRead(t, service, reads[1], readSuccess(
		"status: running  \n"+
			"started_at: 2026-11-25T08:38:22.780Z\n"+
			"running_for_ms: 20031    \n"+
			"---\n"+
			"compiling module a\n"+
			"compiling module b\n"))

	requireNoAwaitResult(t, stream, "call-hdr", "the pattern only appears in the file's header lines")
}

func TestAwaitShellRemoteMissingFileReportsNoShellFound(t *testing.T) {
	service, stream := newAwaitTestStream(t, remoteTerminalsFolder)

	invokeAwaitShell(t, service, stream, "call-missing", `{"shell_id":"447245","block_until_ms":60000}`)
	reads := publishedAwaitReads(t, service)
	answerAwaitRead(t, service, reads[0], &agentv1.ReadResult{Result: &agentv1.ReadResult_FileNotFound{
		FileNotFound: &agentv1.ReadFileNotFound{Path: remoteTerminalsFolder + "/447245.txt"},
	}})

	got := requireAwaitResult(t, stream, "call-missing")
	if got.Result.Status != "error" || !strings.Contains(got.Result.Message, "No shell found for id 447245") {
		t.Fatalf("status=%q message=%q, want the unknown-shell error", got.Result.Status, got.Result.Message)
	}
	if pendingBridgeCount(stream) != 0 {
		t.Fatal("a failed AwaitShell must not leave pending work behind")
	}
}

func TestAwaitShellRemoteReadsStayOutOfHistoryAndCards(t *testing.T) {
	service, stream := newAwaitTestStream(t, remoteTerminalsFolder)

	invokeAwaitShell(t, service, stream, "call-hidden", `{"shell_id":"447245","block_until_ms":60000}`)
	reads := publishedAwaitReads(t, service)
	answerAwaitRead(t, service, reads[0], readSuccess(runningTerminalFile))
	tickAwaitShell(t, service, stream, "call-hidden")
	reads = publishedAwaitReads(t, service)
	answerAwaitRead(t, service, reads[1], readSuccess(finishedTerminalFile))

	toolResults := 0
	for _, entry := range snapshotHistoryEntries(stream) {
		if entry.Kind == "tool_result" {
			toolResults++
		}
	}
	if toolResults != 1 {
		t.Fatalf("history has %d tool_result entries, want only the AwaitShell result (polls are internal)", toolResults)
	}
	events, err := service.broker.ReadFromCursor(awaitTestRequestID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		update := event.Message.GetInteractionUpdate()
		if update == nil {
			continue
		}
		if started := update.GetToolCallStarted(); started != nil && started.GetToolCall().GetReadToolCall() != nil {
			t.Fatal("a poll surfaced as a Read card")
		}
		if completed := update.GetToolCallCompleted(); completed != nil && completed.GetToolCall().GetReadToolCall() != nil {
			t.Fatal("a poll surfaced as a completed Read card")
		}
	}
}

func TestAwaitShellRemoteDeadlineWinsOverUnansweredRead(t *testing.T) {
	previousGrace := awaitRemoteAnswerGrace
	awaitRemoteAnswerGrace = 60 * time.Millisecond
	t.Cleanup(func() { awaitRemoteAnswerGrace = previousGrace })
	service, stream := newAwaitTestStream(t, remoteTerminalsFolder)

	invokeAwaitShell(t, service, stream, "call-lost", `{"shell_id":"447245","block_until_ms":120}`)
	reads := publishedAwaitReads(t, service)
	if len(reads) != 1 {
		t.Fatalf("published %d read requests, want 1", len(reads))
	}

	got := waitForAwaitResult(t, stream, "call-lost", 5*time.Second)
	if got.Result.Status != "running" || !got.Result.TimedOut {
		t.Fatalf("status=%q timed_out=%t, want running/true when the client never answered", got.Result.Status, got.Result.TimedOut)
	}
	if pendingBridgeCount(stream) != 0 {
		t.Fatalf("pendingBridgeCount = %d, want 0: the unanswered read must not outlive the wait", pendingBridgeCount(stream))
	}

	// The answer finally shows up. It must be dropped quietly.
	if err := service.handleExecResult(InboundIntent{
		Kind:      "exec_result",
		RequestID: awaitTestRequestID,
		ExecClientMessage: &agentv1.ExecClientMessage{
			Id:      reads[0].GetId(),
			ExecId:  reads[0].GetExecId(),
			Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: readSuccess(finishedTerminalFile)},
		},
	}); err != nil {
		t.Fatalf("late read answer returned an error (this fails the whole turn): %v", err)
	}
	results := 0
	for _, entry := range snapshotHistoryEntries(stream) {
		if entry.Kind == "tool_result" {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("history has %d tool_result entries after the late answer, want 1", results)
	}
}

// ---------------------------------------------------------------------------
// No terminal file at all: fall back to what the stream reported
// ---------------------------------------------------------------------------

func TestAwaitShellWithoutTerminalsFolderUsesStreamState(t *testing.T) {
	service, stream := newAwaitTestStream(t, "")
	stream.mu.Lock()
	stream.BackgroundShells["447245"] = &BackgroundShellState{
		ShellID:   "447245",
		Status:    backgroundShellStatusBackgrounded,
		CreatedAt: time.Now().UTC().Add(-5 * time.Second),
	}
	stream.mu.Unlock()

	invokeAwaitShell(t, service, stream, "call-stream", `{"shell_id":"447245","block_until_ms":60000}`)
	requireNoAwaitResult(t, stream, "call-stream", "a backgrounded shell is still running")

	exitCode := int32(3)
	stream.mu.Lock()
	state := stream.BackgroundShells["447245"]
	state.Status = backgroundShellStatusCompleted
	state.ExitCode = &exitCode
	state.StdoutBuffer = "boom\n"
	state.CompletedAt = time.Now().UTC()
	stream.mu.Unlock()
	tickAwaitShell(t, service, stream, "call-stream")

	got := requireAwaitResult(t, stream, "call-stream")
	if got.Result.Status != "completed" || got.Result.ExitCode == nil || *got.Result.ExitCode != 3 {
		t.Fatalf("status=%q exit=%v, want completed/3", got.Result.Status, got.Result.ExitCode)
	}
	if !strings.Contains(got.Result.OutputTail, "boom") {
		t.Fatalf("output_tail = %q, want the captured output", got.Result.OutputTail)
	}
}

func TestAwaitShellWithoutAnySourceReportsNoShellFound(t *testing.T) {
	service, stream := newAwaitTestStream(t, "")

	invokeAwaitShell(t, service, stream, "call-nowhere", `{"shell_id":"447245","block_until_ms":60000}`)

	got := requireAwaitResult(t, stream, "call-nowhere")
	if got.Result.Status != "error" || !strings.Contains(got.Result.Message, "No shell found for id 447245") {
		t.Fatalf("status=%q message=%q, want the unknown-shell error", got.Result.Status, got.Result.Message)
	}
}

// ---------------------------------------------------------------------------
// Turn lifecycle
// ---------------------------------------------------------------------------

func TestTurnStaleWatchdogLeavesDeferredAwaitAlone(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", runningTerminalFile)
	invokeAwaitShell(t, service, stream, "call-stale", `{"shell_id":"447245","block_until_ms":600000}`)

	if err := service.forceCompleteTurnStale(stream, awaitTestRequestID, "conv-await", pendingBridgeCount(stream)); err != nil {
		t.Fatalf("forceCompleteTurnStale() error = %v", err)
	}

	requireNoAwaitResult(t, stream, "call-stale", "the model asked to wait ten minutes; the stale-turn watchdog must not answer for it")
	if pendingBridgeCount(stream) != 1 {
		t.Fatalf("pendingBridgeCount = %d, want the wait still pending", pendingBridgeCount(stream))
	}

	writeTerminalFile(t, folder, "447245", finishedTerminalFile)
	tickAwaitShell(t, service, stream, "call-stale")
	if got := requireAwaitResult(t, stream, "call-stale"); got.Result.Status != "completed" {
		t.Fatalf("status = %q, want the real result", got.Result.Status)
	}
}

func TestCancelDropsDeferredAwaitWithoutALateResult(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", runningTerminalFile)
	invokeAwaitShell(t, service, stream, "call-cancel", `{"shell_id":"447245","block_until_ms":600000}`)

	if err := service.handleCancelIntent(InboundIntent{Kind: "cancel", RequestID: awaitTestRequestID, CancelReason: "user aborted"}); err != nil {
		t.Fatalf("handleCancelIntent() error = %v", err)
	}

	writeTerminalFile(t, folder, "447245", finishedTerminalFile)
	tickAwaitShell(t, service, stream, "call-cancel")

	requireNoAwaitResult(t, stream, "call-cancel", "the turn was canceled")
	stream.mu.Lock()
	leftover := len(stream.PendingAwaits)
	stream.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("%d deferred waits survived the cancel", leftover)
	}
}

func TestTwoAwaitShellCallsOnTheSameShellBothReturn(t *testing.T) {
	folder := t.TempDir()
	service, stream := newAwaitTestStream(t, folder)
	writeTerminalFile(t, folder, "447245", runningTerminalFile)

	invokeAwaitShell(t, service, stream, "call-a", `{"shell_id":"447245","block_until_ms":60000}`)
	invokeAwaitShell(t, service, stream, "call-b", `{"shell_id":"447245","block_until_ms":60000}`)
	if got := pendingBridgeCount(stream); got != 2 {
		t.Fatalf("pendingBridgeCount = %d, want 2", got)
	}

	writeTerminalFile(t, folder, "447245", finishedTerminalFile)
	tickAwaitShell(t, service, stream, "call-a")
	tickAwaitShell(t, service, stream, "call-b")

	for _, callID := range []string{"call-a", "call-b"} {
		if got := requireAwaitResult(t, stream, callID); got.Result.Status != "completed" {
			t.Fatalf("%s status = %q, want completed", callID, got.Result.Status)
		}
	}
}

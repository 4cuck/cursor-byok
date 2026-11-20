package execbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Check the completion card and the provider replay payload from the same
// synthetic client message. The original message may still be forwarded.
func applyTestShellResult(t *testing.T, pending runtimecore.PendingExec, completed *agentv1.ShellResult) *agentv1.ShellToolCall {
	t.Helper()
	original := proto.CloneOf(completed)
	result, err := NewBridge().ApplyExecClientMessage(&agentv1.ExecClientMessage{
		Id: pending.MessageID, ExecId: pending.ExecID,
		Message: &agentv1.ExecClientMessage_ShellResult{ShellResult: completed},
	}, pending)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsTerminal || result.ShellOutputDelta != nil || result.ExecID != pending.ExecID || result.ToolCallID != pending.ToolCallID || result.ToolCall.GetToolCallId() != pending.ToolCallID {
		t.Fatalf("incorrect completion: terminal=%t exec=%q call=%q card id=%q", result.IsTerminal, result.ExecID, result.ToolCallID, result.ToolCall.GetToolCallId())
	}
	card := result.ToolCall.GetShellToolCall()
	if card.GetArgs().GetToolCallId() != pending.ToolCallID || card.GetResult() == nil {
		t.Fatalf("missing Shell card args or result: %v", card)
	}
	var replay agentv1.ShellResult
	if err := protojson.Unmarshal([]byte(result.ToolResultPayload), &replay); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(card.Result, &replay) || !proto.Equal(completed, original) {
		t.Fatal("card and replay differ, or bridge mutated the incoming result")
	}
	return card
}

func TestNonStreamingShellResultOutcomes(t *testing.T) {
	bridge := NewBridge()
	nativeArgs := &agentv1.ShellArgs{Command: "pending command", WorkingDirectory: "/requested", Description: proto.String("description"), HardTimeout: proto.Int32(12000), SkipApproval: true}
	raw, err := protojson.Marshal(nativeArgs)
	if err != nil {
		t.Fatal(err)
	}
	_, native, err := bridge.OpenNativeExec(&agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_ShellArgs{ShellArgs: nativeArgs}}, runtimecore.ToolInvocation{CallID: "native-call", ToolName: "Shell", ArgsJSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	_, provider, err := bridge.OpenExec(OpenExecContext{}, runtimecoreToolInvocation("Bash", `{"command":"pending command","working_directory":"/requested","description":"description","block_until_ms":125}`))
	if err != nil {
		t.Fatal(err)
	}
	nativeArgs.ToolCallId = native.ToolCallID
	location := &agentv1.OutputLocation{FilePath: "/client/spill", LineCount: 100, SizeBytes: 9999}
	for name, result := range map[string]*agentv1.ShellResult{
		"success": {Result: &agentv1.ShellResult_Success{Success: &agentv1.ShellSuccess{
			Command: "executed command", WorkingDirectory: "/actual", ExitCode: 7, Signal: "signal", ExecutionTime: 13, LocalExecutionTimeMs: proto.Int32(0),
			ShellId: proto.Uint32(123), Pid: proto.Uint32(44), MsToWait: proto.Int32(20), BackgroundReason: agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.Enum(),
			OutputLocation: location, InterleavedOutput: proto.String("reported output"), OutputHead: proto.String("head"), OutputTail: proto.String("tail"), ElidedChars: proto.Uint32(12),
		}}},
		"failure": {Result: &agentv1.ShellResult_Failure{Failure: &agentv1.ShellFailure{
			Command: "executed command", WorkingDirectory: "/actual", ExitCode: 0, Signal: "abort", Aborted: true, AbortReason: agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT.Enum(),
			ExecutionTime: 42, LocalExecutionTimeMs: proto.Int32(3), OutputLocation: location, Stdout: "out", Stderr: "err", InterleavedOutput: proto.String("err\nout"),
		}}},
		"timeout":           {Result: &agentv1.ShellResult_Timeout{Timeout: &agentv1.ShellTimeout{WorkingDirectory: "/actual", TimeoutMs: 500}}},
		"rejected":          {Result: &agentv1.ShellResult_Rejected{Rejected: &agentv1.ShellRejected{Reason: "not approved", IsReadonly: true}}},
		"permission denied": {Result: &agentv1.ShellResult_PermissionDenied{PermissionDenied: &agentv1.ShellPermissionDenied{Error: "not permitted", IsReadonly: true}}},
		"spawn error":       {Result: &agentv1.ShellResult_SpawnError{SpawnError: &agentv1.ShellSpawnError{Error: "not started"}}},
	} {
		result.IsBackground, result.Pid, result.TerminalsFolder = proto.Bool(false), proto.Uint32(0), proto.String("/client/terminals")
		result.SandboxPolicy = &agentv1.SandboxPolicy{NetworkAccess: proto.Bool(false)}
		for _, pending := range []runtimecore.PendingExec{native, provider} {
			t.Run(pending.ToolCallID+"/"+name, func(t *testing.T) {
				pending.StdoutBuffer, pending.StderrBuffer = "stale stdout", "stale stderr"
				card := applyTestShellResult(t, pending, result)
				if !proto.Equal(card.GetResult(), result) {
					t.Fatalf("original outcome changed: %v", card.GetResult())
				}
				args := card.GetArgs()
				if card.GetDescription() != "description" || args.GetCommand() != "pending command" || args.GetWorkingDirectory() != "/requested" {
					t.Fatalf("incorrect card args or description: %v", card)
				}
				if pending.ToolCallID == native.ToolCallID && !proto.Equal(args, nativeArgs) || pending.ToolCallID == provider.ToolCallID && args.GetTimeout() != 125 {
					t.Fatalf("incorrect pending shell args: %v", args)
				}
			})
		}
	}
}

func TestNonStreamingShellResultOutputFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-spill.txt")
	if err := os.WriteFile(path, []byte("must never become output"), 0600); err != nil {
		t.Fatal(err)
	}
	pending := runtimecore.PendingExec{ExecKind: "shell", ToolCallID: "provider-call", ExecID: "provider-exec", ArgsJSON: []byte(`{"command":"test"}`)}
	location := &agentv1.OutputLocation{FilePath: path, LineCount: 100, SizeBytes: 9999}
	for _, tc := range []struct {
		name                                   string
		reported                               *agentv1.ShellFailure
		bufferOut, bufferErr, wantOut, wantErr string
	}{
		{"both missing", &agentv1.ShellFailure{}, "out", "err", "out", "err"},
		{"stdout missing", &agentv1.ShellFailure{Stderr: "reported"}, "out", "stale", "out", "reported"},
		{"stderr missing", &agentv1.ShellFailure{Stdout: "reported"}, "stale", "err", "reported", "err"},
		{"interleaved only", &agentv1.ShellFailure{InterleavedOutput: proto.String("ordered")}, "stale", "stale", "", ""},
		{"preview only", &agentv1.ShellFailure{OutputHead: proto.String("head"), OutputTail: proto.String("tail")}, "stale", "stale", "", ""},
		{"empty optional text", &agentv1.ShellFailure{InterleavedOutput: proto.String("")}, "out", "err", "out", "err"},
		{"location only", &agentv1.ShellFailure{}, "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending.StdoutBuffer, pending.StderrBuffer = tc.bufferOut, tc.bufferErr
			tc.reported.OutputLocation = location
			want := proto.CloneOf(tc.reported)
			want.Stdout, want.Stderr = tc.wantOut, tc.wantErr
			card := applyTestShellResult(t, pending, &agentv1.ShellResult{Result: &agentv1.ShellResult_Failure{Failure: tc.reported}})
			if !proto.Equal(card.Result.GetFailure(), want) {
				t.Fatalf("incorrect fallback: %v", card.Result)
			}
		})
	}
}

func TestNonStreamingShellResultBounds(t *testing.T) {
	large := "BEGIN" + strings.Repeat("界", 2*shellReplayStreamLimit) + "END"
	pending := runtimecore.PendingExec{ExecKind: "shell", ToolCallID: "call", ExecID: "exec", StdoutBuffer: large, StderrBuffer: large}
	check := func(field, value string, limit int) {
		t.Helper()
		if len(value) > limit || !utf8.ValidString(value) || !strings.HasPrefix(value, "BEGIN") || !strings.HasSuffix(value, "END") || !strings.Contains(value, "omitted middle") {
			t.Errorf("%s: missing ends/truncation or exceeded budget (%d bytes)", field, len(value))
		}
	}
	for _, inline := range []bool{true, false} {
		s := &agentv1.ShellSuccess{}
		if inline {
			s.Stdout, s.Stderr, s.InterleavedOutput, s.OutputHead, s.OutputTail = large, large, proto.String(large), proto.String(large), proto.String(large)
		}
		card := applyTestShellResult(t, pending, &agentv1.ShellResult{Result: &agentv1.ShellResult_Success{Success: s}})
		got := card.Result.GetSuccess()
		check("stdout", got.Stdout, shellReplayStreamLimit)
		check("stderr", got.Stderr, shellReplayStreamLimit)
		if inline {
			check("interleaved", got.GetInterleavedOutput(), 2*shellReplayStreamLimit)
			check("head", got.GetOutputHead(), shellReplayStreamLimit)
			check("tail", got.GetOutputTail(), shellReplayStreamLimit)
		} else if got.InterleavedOutput != nil || got.OutputHead != nil || got.OutputTail != nil {
			t.Fatal("bridge duplicated buffered streams into optional fields")
		}
	}
}

package forwarder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/proto"
)

func TestShellStreamOutputPrefaceKeepsBothEnds(t *testing.T) {
	pending := runtimecore.PendingExec{
		StdoutBuffer: strings.Repeat("A", 6000) + "MIDDLE" + strings.Repeat("Z", 6000),
		StderrBuffer: "boom",
	}
	message := &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_ShellStream{
			ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: 0}},
			},
		},
	}
	got := shellStreamOutputPreface(pending, message)
	data := got.GetShellStream().GetStdout().GetData()
	if !strings.HasPrefix(data, "AAAA") || !strings.Contains(data, "ZZZZ") || !strings.HasSuffix(strings.TrimSpace(data), "boom") {
		t.Fatalf("edges missing: %s", data)
	}
	if strings.Contains(data, "MIDDLE") || !strings.Contains(data, "Output too long.") {
		t.Fatalf("preface = %s", data)
	}
	if len(data) > 12000 {
		t.Fatalf("preface length = %d", len(data))
	}
}

func TestShellStreamOutputPrefaceLeavesShortOutput(t *testing.T) {
	message := &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_ShellStream{
			ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{}},
			},
		},
	}
	got := shellStreamOutputPreface(runtimecore.PendingExec{StdoutBuffer: "total 20\n"}, message)
	if got.GetShellStream().GetStdout().GetData() != "total 20\n" {
		t.Fatalf("short output = %q", got.GetShellStream().GetStdout().GetData())
	}
	if shellStreamOutputPreface(runtimecore.PendingExec{StdoutBuffer: "total 20\n"}, &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_ShellStream{
			ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: "total 20\n"}},
			},
		},
	}) != nil {
		t.Fatal("a live stdout chunk was turned into another preface")
	}
}

func TestCursorAccountShellTerminalUsesCapturedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	pending := runtimecore.PendingExec{StdoutBuffer: "hello from shell\n"}
	message := &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_ShellStream{
			ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{
					Code:           0,
					OutputLocation: &agentv1.OutputLocation{FilePath: path},
				}},
			},
		},
	}
	ch := make(chan *agentv1.ExecClientMessage, 4)
	stream := &ActiveStream{
		CursorToolResults: ch,
		nativeExecSlots: map[string]nativeExecSlot{
			"call-1": {id: 7, execID: "exec-7"},
		},
	}
	if !deliverCursorAccountShellTerminal(stream, "call-1", pending, message) || len(ch) != 2 {
		t.Fatalf("expected output and exit; queued %d messages", len(ch))
	}
	preface := <-ch
	if got := preface.GetShellStream().GetStdout().GetData(); got != pending.StdoutBuffer {
		t.Fatalf("stdout = %q", got)
	}
	if preface.GetId() != 7 || preface.GetExecId() != "exec-7" {
		t.Fatalf("preface id=%d exec=%s", preface.GetId(), preface.GetExecId())
	}
	exitMsg := <-ch
	if exitMsg.GetShellStream().GetExit() == nil {
		t.Fatal("missing exit")
	}
	if exitMsg.GetShellStream().GetExit().GetOutputLocation() != nil {
		t.Fatal("spill path was forwarded with the inline output")
	}
	if exitMsg.GetId() != 7 {
		t.Fatalf("exit id = %d", exitMsg.GetId())
	}
}

func TestCursorAccountShellInlinesLocalAgentToolsSpillFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-tools")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "226f3f4f-f560-4513-a657-107666fbc3d7.txt")
	if err := os.WriteFile(path, []byte("hello from local agent-tools spill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg := &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_ShellStream{
			ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{
					Code:           0,
					OutputLocation: &agentv1.OutputLocation{FilePath: path},
				}},
			},
		},
	}
	ch := make(chan *agentv1.ExecClientMessage, 4)
	stream := &ActiveStream{
		CursorToolResults: ch,
		nativeExecSlots:   map[string]nativeExecSlot{"call-1": {id: 7, execID: "exec-7"}},
	}
	if !deliverCursorAccountShellTerminal(stream, "call-1", runtimecore.PendingExec{}, msg) {
		t.Fatal("deliver failed")
	}
	preface := <-ch
	if got := preface.GetShellStream().GetStdout().GetData(); !strings.Contains(got, "hello from local agent-tools spill") {
		t.Fatalf("stdout = %q", got)
	}
	exitMsg := <-ch
	if exitMsg.GetShellStream().GetExit().GetOutputLocation() != nil {
		t.Fatal("local spill path was forwarded after inlining")
	}
}

func TestCursorAccountShellNeverDereferencesClientFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-output.txt")
	if err := os.WriteFile(path, []byte("proxy-side sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	loc := &agentv1.OutputLocation{FilePath: path}
	for _, kind := range []string{"stream exit", "success", "failure"} {
		t.Run(kind, func(t *testing.T) {
			msg := &agentv1.ExecClientMessage{Id: 73, ExecId: "search-shell"}
			switch kind {
			case "stream exit":
				msg.Message = &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: 2, OutputLocation: loc}}}}
			case "success":
				msg.Message = &agentv1.ExecClientMessage_ShellResult{ShellResult: &agentv1.ShellResult{Result: &agentv1.ShellResult_Success{Success: &agentv1.ShellSuccess{OutputLocation: loc}}}}
			case "failure":
				msg.Message = &agentv1.ExecClientMessage_ShellResult{ShellResult: &agentv1.ShellResult{Result: &agentv1.ShellResult_Failure{Failure: &agentv1.ShellFailure{ExitCode: 2, OutputLocation: loc}}}}
			}
			ch := make(chan *agentv1.ExecClientMessage, 2)
			if !deliverCursorAccountShellTerminal(&ActiveStream{CursorToolResults: ch}, "call", runtimecore.PendingExec{}, msg) || len(ch) != 1 {
				t.Fatalf("expected one path-only terminal, got %d messages", len(ch))
			}
			if got := <-ch; !proto.Equal(got, msg) {
				t.Fatalf("path-only terminal was changed: %v", got)
			}
		})
	}
}

func TestCursorAccountNonStreamingShellDeliversCapturedText(t *testing.T) {
	loc := &agentv1.OutputLocation{FilePath: "client-only.txt"}
	for _, failed := range []bool{false, true} {
		result := &agentv1.ShellResult{Result: &agentv1.ShellResult_Success{Success: &agentv1.ShellSuccess{OutputLocation: loc}}}
		if failed {
			result.Result = &agentv1.ShellResult_Failure{Failure: &agentv1.ShellFailure{ExitCode: 2, OutputLocation: loc}}
		}
		msg := &agentv1.ExecClientMessage{Id: 11, ExecId: "grep-shell", Message: &agentv1.ExecClientMessage_ShellResult{ShellResult: result}}
		before := proto.CloneOf(msg)
		ch := make(chan *agentv1.ExecClientMessage, 2)
		pending := runtimecore.PendingExec{StdoutBuffer: "file.go:9: match", StderrBuffer: "warning"}
		if !deliverCursorAccountShellTerminal(&ActiveStream{CursorToolResults: ch}, "call", pending, msg) || len(ch) != 1 {
			t.Fatalf("expected one terminal, got %d", len(ch))
		}
		got := <-ch
		if got.GetId() != 11 || got.GetExecId() != "grep-shell" || !proto.Equal(msg, before) {
			t.Fatal("call identity lost or original result mutated")
		}
		if failed {
			f := got.GetShellResult().GetFailure()
			if f.GetExitCode() != 2 || f.GetStdout() != pending.StdoutBuffer || f.GetStderr() != pending.StderrBuffer || f.GetOutputLocation() != nil {
				t.Fatalf("incomplete failure: %v", f)
			}
		} else {
			s := got.GetShellResult().GetSuccess()
			if s.GetStdout() != pending.StdoutBuffer || s.GetStderr() != pending.StderrBuffer || s.GetOutputLocation() != nil {
				t.Fatalf("incomplete success: %v", s)
			}
		}
	}
}

func TestDeliverCursorToolResultRestoresZeroUpstreamWireSlot(t *testing.T) {
	ch := make(chan *agentv1.ExecClientMessage, 1)
	stream := &ActiveStream{
		CursorToolResults: ch,
		nativeExecSlots:   map[string]nativeExecSlot{"exec-0": {id: 0, execID: ""}},
	}
	localReply := &agentv1.ExecClientMessage{
		Id:      18,
		ExecId:  "local-exec-18",
		Message: &agentv1.ExecClientMessage_WriteResult{WriteResult: &agentv1.WriteResult{}},
	}
	if !deliverCursorToolResult(stream, "exec-0", localReply) {
		t.Fatal("deliverCursorToolResult failed")
	}
	got := <-ch
	if got.GetId() != 0 || got.GetExecId() != "" {
		t.Fatalf("expected restored upstream wire slot {0, \"\"}, got {%d, %q}", got.GetId(), got.GetExecId())
	}
}

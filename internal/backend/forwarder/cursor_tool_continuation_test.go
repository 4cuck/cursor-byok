package forwarder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
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

func TestCursorAccountShellTerminalInlinesSpillFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte("hello from spill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if !deliverCursorAccountShellTerminal(stream, "call-1", runtimecore.PendingExec{}, message) {
		t.Fatal("deliver failed")
	}
	preface := <-ch
	if got := preface.GetShellStream().GetStdout().GetData(); !strings.Contains(got, "hello from spill") {
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

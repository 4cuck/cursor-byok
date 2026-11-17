package forwarder

import (
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

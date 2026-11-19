package execbridge

import (
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
)

func TestBackgroundShellSpawnBecomesRunningShellCard(t *testing.T) {
	bridge := NewBridge()
	_, pending, err := bridge.OpenNativeExec(&agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_BackgroundShellSpawnArgs{
			BackgroundShellSpawnArgs: &agentv1.BackgroundShellSpawnArgs{
				Command:    "powershell -File build.ps1",
				ToolCallId: "call-1",
			},
		},
	}, runtimecore.ToolInvocation{
		CallID:   "call-1",
		ToolName: "Shell",
		ArgsJSON: []byte(`{"command":"powershell -File build.ps1","description":"Rebuild Windows"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending.ExecKind != "background_shell_spawn" {
		t.Fatalf("exec kind = %q", pending.ExecKind)
	}
	pid := uint32(12128)
	applied, err := bridge.ApplyExecClientMessage(&agentv1.ExecClientMessage{
		Id: pending.MessageID, ExecId: pending.ExecID,
		Message: &agentv1.ExecClientMessage_BackgroundShellSpawnResult{
			BackgroundShellSpawnResult: &agentv1.BackgroundShellSpawnResult{
				Result: &agentv1.BackgroundShellSpawnResult_Success{
					Success: &agentv1.BackgroundShellSpawnSuccess{
						ShellId: 913722,
						Command: "powershell -File build.ps1",
						Pid:     &pid,
					},
				},
			},
		},
	}, pending)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.IsTerminal {
		t.Fatal("spawn result must finish the exec")
	}
	shell := applied.ToolCall.GetShellToolCall()
	if shell.GetResult().GetIsBackground() != true || shell.GetResult().GetSuccess().GetShellId() != 913722 {
		t.Fatalf("shell result = %+v", shell.GetResult())
	}
}

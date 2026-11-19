package modeladapter

import (
	"strings"
	"testing"

	"cursor/gen/agentv1"
)

func TestCursorAccountCapUpstreamExecKeepsReadEnds(t *testing.T) {
	body := strings.Repeat("A", 6000) + "MIDDLE-OF-TERMINAL" + strings.Repeat("Z", 6000)
	msg := &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_ReadResult{
			ReadResult: &agentv1.ReadResult{
				Result: &agentv1.ReadResult_Success{
					Success: &agentv1.ReadSuccess{
						Path: "terminals/26464.txt",
						Output: &agentv1.ReadSuccess_Content{
							Content: body,
						},
					},
				},
			},
		},
	}
	cursorAccountCapUpstreamExec(msg)
	got := msg.GetReadResult().GetSuccess().GetContent()
	if !msg.GetReadResult().GetSuccess().GetTruncated() {
		t.Fatal("long read was not marked truncated")
	}
	if !strings.HasPrefix(got, "AAAA") || !strings.Contains(got, "ZZZZ") || strings.Contains(got, "MIDDLE-OF-TERMINAL") {
		t.Fatalf("capped read = %s", got)
	}
	if !strings.Contains(got, "Output too long.") || len(got) > 12000 {
		t.Fatalf("capped length = %d text=%s", len(got), got)
	}
}

func TestCursorAccountCapUpstreamExecLeavesShortRead(t *testing.T) {
	msg := &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_ReadResult{
			ReadResult: &agentv1.ReadResult{
				Result: &agentv1.ReadResult_Success{
					Success: &agentv1.ReadSuccess{
						Output: &agentv1.ReadSuccess_Content{Content: "exit_code: 0\n"},
					},
				},
			},
		},
	}
	cursorAccountCapUpstreamExec(msg)
	if msg.GetReadResult().GetSuccess().GetTruncated() || msg.GetReadResult().GetSuccess().GetContent() != "exit_code: 0\n" {
		t.Fatalf("short read changed: %+v", msg.GetReadResult().GetSuccess())
	}
}

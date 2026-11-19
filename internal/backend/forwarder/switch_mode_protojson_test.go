package forwarder

import (
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
)

func TestBuildStartedSwitchModeKeepsProtoJSONTarget(t *testing.T) {
	for _, argsJSON := range []string{
		`{"targetModeId":"plan","explanation":"design first"}`,
		`{"target_mode_id":"ask","explanation":"read only"}`,
	} {
		toolCall := buildStartedToolCall(runtimecore.ToolInvocation{
			CallID:   "call-1\nfc_1",
			ToolName: "SwitchMode",
			ArgsJSON: []byte(argsJSON),
		})
		args := toolCall.GetSwitchModeToolCall().GetArgs()
		mode, err := switchModeTarget(args)
		if err != nil {
			t.Fatalf("switchModeTarget(%s) error = %v", argsJSON, err)
		}
		want := agentv1.AgentMode_AGENT_MODE_PLAN
		if args.GetTargetModeId() == "ask" {
			want = agentv1.AgentMode_AGENT_MODE_ASK
		}
		if mode != want {
			t.Fatalf("switchModeTarget(%s) = %s, want %s", argsJSON, mode, want)
		}
	}
}

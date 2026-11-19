package interaction

import (
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
)

func TestSwitchModeApprovalKeepsProtoJSONTarget(t *testing.T) {
	bridge := NewBridge()
	for _, argsJSON := range []string{
		`{"targetModeId":"plan","explanation":"design first","toolCallId":"call-1\nfc_1"}`,
		`{"target_mode_id":"plan","explanation":"design first","tool_call_id":"call-1"}`,
	} {
		result, err := bridge.ApplyInteractionResponse(&agentv1.InteractionResponse{
			Id: 7,
			Result: &agentv1.InteractionResponse_SwitchModeRequestResponse{
				SwitchModeRequestResponse: &agentv1.SwitchModeRequestResponse{
					Result: &agentv1.SwitchModeRequestResponse_Approved_{
						Approved: &agentv1.SwitchModeRequestResponse_Approved{},
					},
				},
			},
		}, runtimecore.PendingInteraction{
			InteractionID:   "7",
			ToolCallID:      "call-1",
			InteractionKind: "switch_mode",
			ArgsJSON:        []byte(argsJSON),
		})
		if err != nil {
			t.Fatalf("ApplyInteractionResponse(%s) error = %v", argsJSON, err)
		}
		toolCall := result.ToolCall.GetSwitchModeToolCall()
		if toolCall.GetArgs().GetTargetModeId() != "plan" {
			t.Fatalf("argsJSON %s target = %q, want plan", argsJSON, toolCall.GetArgs().GetTargetModeId())
		}
		if toolCall.GetResult().GetSuccess().GetToModeId() != "plan" {
			t.Fatalf("argsJSON %s to = %q, want plan", argsJSON, toolCall.GetResult().GetSuccess().GetToModeId())
		}
		if result.ToolResultPayload != "switch mode success to=plan" {
			t.Fatalf("argsJSON %s payload = %q", argsJSON, result.ToolResultPayload)
		}
	}
}

func TestOpenSwitchModeAcceptsProtoJSONTarget(t *testing.T) {
	bridge := NewBridge()
	message, _, err := bridge.OpenQuery(runtimecore.ToolInvocation{
		CallID:   "call-1",
		ToolName: "SwitchMode",
		ArgsJSON: []byte(`{"targetModeId":"plan","explanation":"design first"}`),
	})
	if err != nil {
		t.Fatalf("OpenQuery(SwitchMode) error = %v", err)
	}
	if got := message.GetInteractionQuery().GetSwitchModeRequestQuery().GetArgs().GetTargetModeId(); got != "plan" {
		t.Fatalf("target mode = %q, want plan", got)
	}
}

func TestOpenAskQuestionKeepsAllowMultiple(t *testing.T) {
	bridge := NewBridge()
	message, _, err := bridge.OpenQuery(runtimecore.ToolInvocation{
		CallID:   "call-1",
		ToolName: "AskQuestion",
		ArgsJSON: []byte(`{"title":"Pick","questions":[{"id":"q","prompt":"Which?","allowMultiple":true}]}`),
	})
	if err != nil {
		t.Fatalf("OpenQuery(AskQuestion) error = %v", err)
	}
	questions := message.GetInteractionQuery().GetAskQuestionInteractionQuery().GetArgs().GetQuestions()
	if len(questions) != 1 || !questions[0].GetAllowMultiple() {
		t.Fatalf("questions = %#v, want allowMultiple", questions)
	}
}

func TestPrManagementProtoJSONKeepsCreateAction(t *testing.T) {
	args := unmarshalPrManagementArgs([]byte(`{"toolCallId":"call-1","createPr":{"title":"Fix plan mode"}}`))
	if args.GetCreatePr().GetTitle() != "Fix plan mode" {
		t.Fatalf("title = %q", args.GetCreatePr().GetTitle())
	}
}

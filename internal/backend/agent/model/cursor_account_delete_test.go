package modeladapter

import (
	"bytes"
	"context"
	"testing"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/proto"
)

// The IDE runs a delete only after it finds the tool card named by the
// request's tool_call_id; without one it answers "File deletion rejected:
// Failed to find tool call context: Timeout waiting for bubble creation".
// The card is created under the id the exec is dispatched with, so that id
// has to be the one the request names.

func upstreamDeleteExec() *agentv1.ExecServerMessage {
	return &agentv1.ExecServerMessage{
		Id:     7,
		ExecId: "891274cc-8b09-4468-b002-977fc3a62679",
		Message: &agentv1.ExecServerMessage_DeleteArgs{
			DeleteArgs: &agentv1.DeleteArgs{Path: `c:\repo\scratch.txt`, ToolCallId: "toolu_01UCQmL1Mnf3d4LiLcTYyAak"},
		},
	}
}

func TestCursorAccountDeleteIsDispatchedUnderItsToolCallID(t *testing.T) {
	name, callID, _, ok := cursorAccountExecTool(upstreamDeleteExec())

	if !ok || name != "Delete" {
		t.Fatalf("name=%q ok=%v, want the Delete tool", name, ok)
	}
	if callID != "toolu_01UCQmL1Mnf3d4LiLcTYyAak" {
		t.Fatalf("dispatch id = %q, want toolu_01UCQmL1Mnf3d4LiLcTYyAak: the IDE looks the delete's card up by the request's tool_call_id", callID)
	}
}

func TestCursorAccountDeleteWithoutToolCallIDKeepsItsExecID(t *testing.T) {
	exec := upstreamDeleteExec()
	exec.GetDeleteArgs().ToolCallId = ""

	if _, callID, _, _ := cursorAccountExecTool(exec); callID != "891274cc-8b09-4468-b002-977fc3a62679" {
		t.Fatalf("dispatch id = %q, want the exec id", callID)
	}
}

// End to end through the stream reader: the card id and the id inside the
// request that reaches the IDE are the same.
func TestCursorAccountDeleteCardAndRequestShareOneID(t *testing.T) {
	var input bytes.Buffer
	writeCursorAgentFrame(&input, &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ExecServerMessage{ExecServerMessage: upstreamDeleteExec()},
	})
	var cardID, requestID string
	recorder := &appendRecorder{}
	err := readCursorAccountStream(context.Background(), recorder.append, "req", "conv", "model", "cursor", 0, &input, nil, nil, nil, nil, func(event ModelEvent) error {
		if event.ToolInvocation == nil {
			return nil
		}
		cardID = event.ToolInvocation.CallID
		forwarded := &agentv1.ExecServerMessage{}
		if err := proto.Unmarshal(event.ToolInvocation.NativeExec, forwarded); err != nil {
			return err
		}
		requestID = forwarded.GetDeleteArgs().GetToolCallId()
		return nil
	})
	if err != nil {
		t.Fatalf("readCursorAccountStream() error = %v", err)
	}

	if cardID == "" || cardID != requestID {
		t.Fatalf("card id = %q, id in the forwarded request = %q: they must match or the IDE never finds the card", cardID, requestID)
	}
}

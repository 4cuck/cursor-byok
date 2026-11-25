package forwarder

import (
	"strings"
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/proto"
)

// The Cursor client keeps a shell's working directory and environment per
// conversation id. The conversation the user is in is the local one, so that
// is the id every shell exec sent to the client must carry.

func stopStreamTimers(stream *ActiveStream) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	for _, timer := range stream.StreamTimers {
		if timer != nil {
			timer.Stop()
		}
	}
}

func publishedShellConversationIDs(t *testing.T, service *Service, requestID string) []string {
	t.Helper()
	events, err := service.broker.ReadFromCursor(requestID, 0)
	if err != nil {
		t.Fatalf("ReadFromCursor() error = %v", err)
	}
	var ids []string
	for _, event := range events {
		exec := event.Message.GetExecServerMessage()
		switch {
		case exec.GetShellStreamArgs() != nil:
			ids = append(ids, exec.GetShellStreamArgs().GetConversationId())
		case exec.GetShellArgs() != nil:
			ids = append(ids, exec.GetShellArgs().GetConversationId())
		case exec.GetBackgroundShellSpawnArgs() != nil:
			ids = append(ids, exec.GetBackgroundShellSpawnArgs().GetConversationId())
		}
	}
	return ids
}

// A turn on a Cursor account can continue on a fresh upstream conversation.
// The shell execs of that conversation name its new id; passed on unchanged,
// the client would start the chat's shell over in the workspace root.
func TestNativeShellKeepsTheChatsShellWhenUpstreamStartsAFreshConversation(t *testing.T) {
	service := NewService(t.TempDir(), nilResolver{})
	defer service.multitaskDelegation.Close()
	const requestID = "req-native-shell-conversation"
	const localConversation = "conv-the-user-is-in"
	stream, err := service.broker.OpenStream(requestID, localConversation, 1, "grok-4.7", "Grok 4.7",
		agentv1.AgentMode_AGENT_MODE_AGENT, "run the tests")
	if err != nil {
		t.Fatal(err)
	}
	defer stopStreamTimers(stream)

	for index, upstreamConversation := range []string{localConversation, "fresh-upstream-conversation"} {
		callID := "call-shell-" + upstreamConversation
		raw, err := proto.Marshal(&agentv1.ExecServerMessage{
			Id:     uint32(10 + index),
			ExecId: "upstream-exec-" + upstreamConversation,
			Message: &agentv1.ExecServerMessage_ShellStreamArgs{
				ShellStreamArgs: &agentv1.ShellArgs{
					Command:         "npm test",
					ToolCallId:      callID,
					Timeout:         30000,
					TimeoutBehavior: agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND,
					ConversationId:  proto.String(upstreamConversation),
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		err = service.dispatchNativeCursorExec(stream, runtimecore.ToolInvocation{
			CallID:      callID,
			ToolName:    "Shell",
			ModelCallID: requestID,
			ArgsJSON:    []byte(`{"command":"npm test"}`),
			NativeExec:  raw,
		})
		if err != nil && !strings.Contains(err.Error(), "checkpoint conversation is not initialized") {
			t.Fatalf("dispatchNativeCursorExec() error = %v", err)
		}
	}

	ids := publishedShellConversationIDs(t, service, requestID)
	if len(ids) != 2 {
		t.Fatalf("shell execs sent to the client = %d, want 2", len(ids))
	}
	for index, id := range ids {
		if id != localConversation {
			t.Fatalf("shell exec %d names conversation %q, want %q: a different id gives the chat a new shell in the workspace root", index+1, id, localConversation)
		}
	}
}

// A model on the user's own API key runs its shells through the same client.
func TestModelShellNamesTheStreamsConversation(t *testing.T) {
	service, stream := newAwaitTestStream(t, "")

	message, _, err := service.execBridge.OpenExec(buildExecOpenContextForStream(stream, nil), runtimecore.ToolInvocation{
		CallID:   "call-model-shell",
		ToolName: "Shell",
		ArgsJSON: []byte(`{"command":"go test ./..."}`),
	})
	if err != nil {
		t.Fatalf("OpenExec(Shell) error = %v", err)
	}

	if got := message.GetExecServerMessage().GetShellStreamArgs().GetConversationId(); got != "conv-await" {
		t.Fatalf("shell conversation id = %q, want the stream's conversation conv-await", got)
	}
}

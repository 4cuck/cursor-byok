package execbridge

import (
	"testing"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"

	"google.golang.org/protobuf/proto"
)

// The Cursor client keeps a shell's state -- working directory, environment
// -- per conversation id, and starts each conversation in the workspace root.
// A shell exec without a conversation id uses the one state every such exec
// shares: a `cd` in one chat moved every other chat and subagent in the same
// window, and a new chat started wherever the last one had left off.

func openTestShell(t *testing.T, conversationID string) *agentv1.ShellArgs {
	t.Helper()
	message, pending, err := NewBridge().OpenExec(OpenExecContext{ConversationID: conversationID}, runtimecore.ToolInvocation{
		CallID:   "call-shell",
		ToolName: "Shell",
		ArgsJSON: []byte(`{"command":"cd frontend && npm test","block_until_ms":60000}`),
	})
	if err != nil {
		t.Fatalf("OpenExec(Shell) error = %v", err)
	}
	if pending.ExecKind != "shell" {
		t.Fatalf("exec kind = %q, want shell", pending.ExecKind)
	}
	args := message.GetExecServerMessage().GetShellStreamArgs()
	if args == nil {
		t.Fatalf("OpenExec(Shell) did not build shell stream args: %v", message)
	}
	return args
}

func TestShellExecNamesTheConversationWhoseShellItUses(t *testing.T) {
	args := openTestShell(t, "conversation-a")

	if got := args.GetConversationId(); got != "conversation-a" {
		t.Fatalf("shell conversation id = %q, want conversation-a: without it the client runs the command in the shell state shared by every chat", got)
	}

	if other := openTestShell(t, "conversation-b").GetConversationId(); other != "conversation-b" {
		t.Fatalf("second conversation's shell id = %q, want conversation-b", other)
	}
}

func TestShellExecWithoutAKnownConversationSendsNoID(t *testing.T) {
	args := openTestShell(t, "   ")

	if args.ConversationId != nil {
		t.Fatalf("conversation id = %q, want the field left unset when the conversation is not known", args.GetConversationId())
	}
}

// A Cursor-account exec arrives with the conversation id of the upstream
// conversation. That id changes whenever a turn continues on a fresh upstream
// conversation, and the client would then hand the same chat a new shell,
// back in the workspace root. The chat the user sees is the local
// conversation, so its id is the one the client is given.
func TestPinNativeShellConversation(t *testing.T) {
	upstream := proto.String("upstream-conversation-2")
	cases := map[string]struct {
		exec *agentv1.ExecServerMessage
		read func(*agentv1.ExecServerMessage) string
	}{
		"shell": {
			exec: &agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_ShellArgs{
				ShellArgs: &agentv1.ShellArgs{Command: "pwd", ConversationId: upstream},
			}},
			read: func(exec *agentv1.ExecServerMessage) string { return exec.GetShellArgs().GetConversationId() },
		},
		"shell stream": {
			exec: &agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_ShellStreamArgs{
				ShellStreamArgs: &agentv1.ShellArgs{Command: "pwd", ConversationId: upstream},
			}},
			read: func(exec *agentv1.ExecServerMessage) string { return exec.GetShellStreamArgs().GetConversationId() },
		},
		"background shell": {
			exec: &agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_BackgroundShellSpawnArgs{
				BackgroundShellSpawnArgs: &agentv1.BackgroundShellSpawnArgs{Command: "npm run dev", ConversationId: upstream},
			}},
			read: func(exec *agentv1.ExecServerMessage) string {
				return exec.GetBackgroundShellSpawnArgs().GetConversationId()
			},
		},
		"shell without an upstream id": {
			exec: &agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_ShellStreamArgs{
				ShellStreamArgs: &agentv1.ShellArgs{Command: "pwd"},
			}},
			read: func(exec *agentv1.ExecServerMessage) string { return exec.GetShellStreamArgs().GetConversationId() },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			PinNativeShellConversation(tc.exec, "local-conversation")

			if got := tc.read(tc.exec); got != "local-conversation" {
				t.Fatalf("conversation id = %q, want local-conversation", got)
			}
		})
	}
}

func TestPinNativeShellConversationLeavesEverythingElseAlone(t *testing.T) {
	read := &agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_ReadArgs{
		ReadArgs: &agentv1.ReadArgs{Path: "/ws/main.go"},
	}}
	before := proto.CloneOf(read)
	PinNativeShellConversation(read, "local-conversation")
	if !proto.Equal(read, before) {
		t.Fatalf("a non-shell exec was changed: %v", read)
	}

	// Without a local conversation there is nothing better than what upstream sent.
	shell := &agentv1.ExecServerMessage{Message: &agentv1.ExecServerMessage_ShellStreamArgs{
		ShellStreamArgs: &agentv1.ShellArgs{Command: "pwd", ConversationId: proto.String("upstream-conversation")},
	}}
	PinNativeShellConversation(shell, "  ")
	if got := shell.GetShellStreamArgs().GetConversationId(); got != "upstream-conversation" {
		t.Fatalf("conversation id = %q, want the upstream id kept when no local conversation is known", got)
	}

	PinNativeShellConversation(nil, "local-conversation") // must not panic
}

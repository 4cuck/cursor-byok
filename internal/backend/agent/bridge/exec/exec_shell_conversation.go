package execbridge

import (
	"strings"

	"cursor/gen/agentv1"
)

// The Cursor client keeps shell state (working directory, environment) per
// conversation id: each id gets its own shell, started in the workspace root,
// and an exec without an id uses the one shell all such execs share. Every
// shell exec therefore names the conversation it belongs to.

// shellConversationID returns the conversation id to put on a shell exec, or
// nil when the conversation is not known.
func shellConversationID(conversationID string) *string {
	trimmed := strings.TrimSpace(conversationID)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// PinNativeShellConversation makes a Cursor-account shell exec use the shell
// state of conversationID, the conversation the user is in.
//
// The exec arrives naming the upstream conversation. That id is replaced
// whenever a turn continues on a fresh upstream conversation, and the client
// would then give the same chat a new shell, back in the workspace root with
// its environment changes gone. Execs that are not shells, and a blank
// conversationID, are left as they are.
func PinNativeShellConversation(exec *agentv1.ExecServerMessage, conversationID string) {
	pinned := shellConversationID(conversationID)
	if exec == nil || pinned == nil {
		return
	}
	switch args := exec.GetMessage().(type) {
	case *agentv1.ExecServerMessage_ShellArgs:
		if args.ShellArgs != nil {
			args.ShellArgs.ConversationId = pinned
		}
	case *agentv1.ExecServerMessage_ShellStreamArgs:
		if args.ShellStreamArgs != nil {
			args.ShellStreamArgs.ConversationId = pinned
		}
	case *agentv1.ExecServerMessage_BackgroundShellSpawnArgs:
		if args.BackgroundShellSpawnArgs != nil {
			args.BackgroundShellSpawnArgs.ConversationId = pinned
		}
	}
}

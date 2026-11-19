package forwarder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/internal/appdata"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestCursorAccountForwardContextKeepsRemoteLinux(t *testing.T) {
	payload, err := protojson.Marshal(&agentv1.RequestContext{
		Env: &agentv1.RequestContextEnv{
			OsVersion:      "linux 6.12.41+deb13-amd64",
			Shell:          "/bin/bash",
			WorkspacePaths: []string{"/root/teapot"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation := &ConversationFile{Entries: []HistoryEntry{{
		Kind:    "request_context",
		Payload: payload,
	}}}
	got := cursorAccountForwardContext(conversation, []string{`C:\Users\admin\Documents\cursor-byok`}, `C:\terminals`)
	env := got.GetEnv()
	if env.GetOsVersion() != "linux 6.12.41+deb13-amd64" || env.GetShell() != "/bin/bash" {
		t.Fatalf("env os=%q shell=%q", env.GetOsVersion(), env.GetShell())
	}
	if paths := env.GetWorkspacePaths(); len(paths) != 1 || paths[0] != "/root/teapot" {
		t.Fatalf("workspace = %v", paths)
	}
	if strings.Contains(env.GetTerminalsFolder(), `C:\`) {
		t.Fatalf("local terminals folder replaced the remote context: %q", env.GetTerminalsFolder())
	}
}

func TestCursorAccountForwardContextUsesStreamWorkspace(t *testing.T) {
	got := cursorAccountForwardContext(nil, []string{"/srv/teapot"}, "/srv/teapot/terminals")
	env := got.GetEnv()
	if paths := env.GetWorkspacePaths(); len(paths) != 1 || paths[0] != "/srv/teapot" {
		t.Fatalf("workspace = %v", paths)
	}
	if env.GetProjectFolder() != "/srv/teapot" || env.GetTerminalsFolder() != "/srv/teapot/terminals" {
		t.Fatalf("project=%q terminals=%q", env.GetProjectFolder(), env.GetTerminalsFolder())
	}
}

func TestCursorAccountForwardContextPreservesTurn1EnvAcrossRealtimeTurn2(t *testing.T) {
	turn1Payload, err := protojson.Marshal(&agentv1.RequestContext{
		Env: &agentv1.RequestContextEnv{
			OsVersion:       "win32 10.0.26200",
			Shell:           "powershell",
			WorkspacePaths:  []string{`c:\Users\admin\Documents\cursor-byok`},
			TerminalsFolder: `C:\Users\admin\.cursor\projects\cursor-byok\terminals`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	commitAttr := "enabled"
	turn2RealtimePayload, err := protojson.Marshal(&agentv1.RequestContext{
		CommitAttributionMessage: &commitAttr,
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation := &ConversationFile{Entries: []HistoryEntry{
		{TurnSeq: 1, Kind: "request_context", Payload: turn1Payload},
		{TurnSeq: 2, Kind: "request_context", Payload: turn2RealtimePayload},
	}}
	got := cursorAccountForwardContext(conversation, nil, "")
	env := got.GetEnv()
	if paths := env.GetWorkspacePaths(); len(paths) != 1 || paths[0] != `c:\Users\admin\Documents\cursor-byok` {
		t.Fatalf("follow-up lost Turn 1 workspace paths: %v", paths)
	}
	if got.GetCommitAttributionMessage() != "enabled" {
		t.Fatalf("follow-up lost Turn 2 realtime fields: %q", got.GetCommitAttributionMessage())
	}
}

func TestHandleRunIntentRewindRestoresPreTurnCursorAccountCheckpoint(t *testing.T) {
	homeDir, err := os.MkdirTemp("", "cursor-byok-home-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	t.Setenv("CURSOR_BYOK_HOME", homeDir)

	store := NewConversationFileStore(t.TempDir())
	conversation := testConversation(nil)
	appendEntriesInPlace(conversation, []HistoryEntry{
		testUserMessageEntry(t, 1, "request-1", "analyze ecker-soyjak"),
		newAssistantTextEntry(1, "request-1", "analyzed wasm", "", ""),
	})
	conversation.TokenDetailsMaxTokens = 1_000_000
	conversation.TokenDetailsUsedTokens = 434_532
	conversation.CurrentTurnSeq = 1
	conversation.NextTurnSeq = 2
	persisted, err := store.SaveConversationWithEntries(conversation.ConversationID, conversation, conversation.Entries)
	if err != nil {
		t.Fatalf("SaveConversationWithEntries() error = %v", err)
	}
	convID := persisted.ConversationID
	t.Cleanup(func() { modeladapter.ClearCursorAccountStateAndSnapshots(convID) })

	// Persist a completed Turn 1 upstream Cursor checkpoint (434,532 tokens).
	turn1Checkpoint := &agentv1.ConversationStateStructure{
		Mode: agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		TokenDetails: &agentv1.ConversationTokenDetails{
			UsedTokens: 434_532,
			MaxTokens:  1_000_000,
		},
		Turns: [][]byte{[]byte("turn-1")},
	}
	rawCheckpoint, err := proto.Marshal(turn1Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(appdata.HistoryRootPath(), convID, "cursor-agent-state.bin")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, rawCheckpoint, 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &contextProjectionRequestProvider{requests: make(chan ProviderRequest, 4)}
	broker := NewStreamBroker()
	service := newServiceWithDependencies(store, NewHistoryProjector(), contextProjectionLifecycleCompiler{}, provider, broker)
	service.resolver = cursorAccountSavedStateResolver{}

	// Turn 2 attempt #1 on Gemini 4 Neon (messageId="msg-continue-2") snapshots pre-turn-2 state.
	turn2Msg := &agentv1.UserMessage{
		Text:      "continue",
		MessageId: "msg-continue-2",
		Mode:      agentv1.AgentMode_AGENT_MODE_AGENT,
	}
	makeRunIntent := func(reqID, modelID, modelName string) InboundIntent {
		clientMsg := &agentv1.AgentClientMessage{
			Message: &agentv1.AgentClientMessage_RunRequest{
				RunRequest: &agentv1.AgentRunRequest{
					ConversationId: proto.String(convID),
					ConversationState: &agentv1.ConversationStateStructure{
						Mode:  agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
						Turns: [][]byte{[]byte("turn-1")},
					},
					Action: &agentv1.ConversationAction{
						Action: &agentv1.ConversationAction_UserMessageAction{
							UserMessageAction: &agentv1.UserMessageAction{
								UserMessage: turn2Msg,
							},
						},
					},
				},
			},
		}
		return InboundIntent{
			Kind:              "run",
			StartsRun:         true,
			RequestID:         reqID,
			ConversationID:    convID,
			ModelID:           modelID,
			ModelName:         modelName,
			Mode:              agentv1.AgentMode_AGENT_MODE_AGENT,
			HasExplicitMode:   true,
			ModeSource:        ModeSourceUserMessage,
			UserMessage:       turn2Msg,
			ConversationState: clientMsg.GetRunRequest().GetConversationState(),
			ClientMessage:     clientMsg,
		}
	}

	if err := service.handleRunIntent(makeRunIntent("req-gemini-fail", "cursor_account1:gemini-4-neon", "Gemini 4 Neon")); err != nil {
		t.Fatalf("handleRunIntent(turn 2 attempt 1) error = %v", err)
	}
	select {
	case <-provider.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("expected first turn 2 provider request")
	}

	// Simulate Gemini 4 failing after writing a dirty/reset state onto active cursor-agent-state.bin.
	dirtyRaw, _ := proto.Marshal(&agentv1.ConversationStateStructure{
		Mode: agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		TokenDetails: &agentv1.ConversationTokenDetails{
			UsedTokens: 22_400,
			MaxTokens:  1_000_000,
		},
		Turns: [][]byte{[]byte("dirty-turn")},
	})
	_ = os.WriteFile(statePath, dirtyRaw, 0o600)
	// Force eviction/reload check: write dirty state through Clear + write so memory reflects dirty state.
	modeladapter.ClearCursorAccountState(convID)
	_ = os.WriteFile(statePath, dirtyRaw, 0o600)
	if got := modeladapter.CursorAccountUsedTokens(convID); got != 22_400 {
		t.Fatalf("pre-rewind dirty UsedTokens = %d, want 22400", got)
	}

	// Turn 2 attempt #2: user switches to Grok 4.7 Extra High and retries the same messageId.
	if err := service.handleRunIntent(makeRunIntent("req-grok-retry", "cursor_account1:grok-4.7", "Grok 4.7")); err != nil {
		t.Fatalf("handleRunIntent(turn 2 retry on Grok) error = %v", err)
	}
	select {
	case req := <-provider.requests:
		if req.ModelID != "cursor_account1:grok-4.7" {
			t.Fatalf("retried model = %q, want cursor_account1:grok-4.7", req.ModelID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected Grok 4.7 retry provider request")
	}

	if !modeladapter.HasCursorAccountState(convID) {
		t.Fatal("rewind retry cleared upstream Cursor checkpoint instead of restoring pre-turn-2 snapshot")
	}
	if got := modeladapter.CursorAccountUsedTokens(convID); got != 434_532 {
		t.Fatalf("restored UsedTokens after model-switch retry = %d, want 434532", got)
	}
}


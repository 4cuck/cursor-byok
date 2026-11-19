package modeladapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/gen/aiserverv1"
	"cursor/internal/cursoraccount"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// TestLiveFableLowFastRemembers talks to the signed-in Cursor account.
// It is the check the local frame tests cannot make: two real Fable 5.2 low-fast
// turns, and the second turn must still know the first.
func TestLiveFableLowFastRemembers(t *testing.T) {
	if os.Getenv("CURSOR_FABLE_LIVE") != "1" {
		t.Skip("set CURSOR_FABLE_LIVE=1 to run the live Fable memory check")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// go test sandboxes appdata, so load the real account store directly.
	root := filepath.Join(home, ".cursor-local-assistant-v2", "data")
	manager := cursoraccount.NewManager(root, filepath.Join(root, "cursor-account.json"), &http.Client{Timeout: 3 * time.Minute})
	if _, err := manager.Authorization(context.Background()); err != nil {
		t.Fatalf("cursor account is not signed in: %v", err)
	}
	gateway := OpenCursorAccountGateway(manager)
	conversationID := uuid.NewString()
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	first, err := collectCursorAccountText(ctx, gateway, conversationID, []Message{{
		Role:    "user",
		Content: "Say the single word banana and nothing else.",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(first), "banana") {
		t.Fatalf("first reply did not contain banana: %q", trimLive(first))
	}
	if state := cursorAccountLoadState(conversationID); state == nil {
		t.Logf("no conversation checkpoint after the first turn frames=%s blobs=%s", strings.Join(cursorAccountFrameLog(), ","), cursorAccountBlobSummary())
	} else {
		t.Logf("checkpoint turns=%d root_messages=%d frames=%s blobs=%s", len(state.GetTurns()), len(state.GetRootPromptMessagesJson()), strings.Join(cursorAccountFrameLog(), ","), cursorAccountBlobSummary())
	}
	second, err := collectCursorAccountText(ctx, gateway, conversationID, []Message{
		{Role: "user", Content: "Say the single word banana and nothing else."},
		{Role: "assistant", Content: first},
		{Role: "user", Content: "What single word did I just ask you to say?"},
	})
	if err != nil {
		t.Fatalf("%v frames=%s", err, strings.Join(cursorAccountFrameLog(), ","))
	}
	if !strings.Contains(strings.ToLower(second), "banana") {
		t.Fatalf("second reply forgot the first turn: %q frames=%s", trimLive(second), strings.Join(cursorAccountFrameLog(), ","))
	}
}

func TestLiveFableLowFastUsesLinuxSSH(t *testing.T) {
	if os.Getenv("CURSOR_FABLE_LIVE") != "1" {
		t.Skip("set CURSOR_FABLE_LIVE=1 to run the live Fable linux check")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".cursor-local-assistant-v2", "data")
	manager := cursoraccount.NewManager(root, filepath.Join(root, "cursor-account.json"), &http.Client{Timeout: 3 * time.Minute})
	accountID := manager.CurrentAccountID()
	if _, err := manager.AuthorizationFor(context.Background(), accountID); err != nil {
		t.Fatalf("cursor account 1 is not signed in: %v", err)
	}
	gateway := OpenCursorAccountGateway(manager)
	stateDir, err := os.MkdirTemp("", "cursor-fable-linux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("CURSOR_BYOK_HOME", stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	remote := &agentv1.RequestContext{
		Env: &agentv1.RequestContextEnv{
			OsVersion:      "linux 6.12.41+deb13-amd64",
			Shell:          "/bin/bash",
			WorkspacePaths: []string{"/root/teapot"},
			ProjectFolder:  "/root/teapot",
		},
	}
	var command string
	var toolName string
	err = gateway.Stream(ctx, StreamRequest{
		RequestID:            uuid.NewString(),
		RunID:                uuid.NewString(),
		ModelCallID:          uuid.NewString(),
		ConversationID:       uuid.NewString(),
		Mode:                 agentv1.AgentMode_AGENT_MODE_AGENT,
		ModelID:              "claude-fable-5-2-low-fast",
		ProviderModelID:      "claude-fable-5-2-low-fast",
		GroupName:            "cursor_account1",
		CursorAccountID:      accountID,
		FastMode:             true,
		ThinkingEffort:       "low",
		CursorRequestContext: remote,
		Messages: []Message{{
			Role:    "user",
			Content: "Run one shell command that prints the kernel name. Use the shell named in your environment. Do not explain.",
		}},
		Stream: true,
	}, func(event ModelEvent) error {
		if event.Kind == ModelEventKindToolLikeCompleted && event.ToolInvocation != nil && toolName == "" {
			toolName = event.ToolInvocation.ToolName
			command = string(event.ToolInvocation.ArgsJSON)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(command)
	if toolName != "Shell" || strings.Contains(lower, "get-location") || strings.Contains(lower, "get-psdrive") || strings.Contains(lower, "userprofile") || strings.Contains(lower, "powershell") {
		t.Fatalf("tool=%q command=%s", toolName, trimLive(command))
	}
	t.Logf("shell command=%s", trimLive(command))

	followCtx, followCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer followCancel()
	var reply strings.Builder
	var followTools []string
	err = gateway.Stream(followCtx, StreamRequest{
		RequestID:            uuid.NewString(),
		RunID:                uuid.NewString(),
		ModelCallID:          uuid.NewString(),
		ConversationID:       uuid.NewString(),
		Mode:                 agentv1.AgentMode_AGENT_MODE_AGENT,
		ModelID:              "claude-fable-5-2-low-fast",
		ProviderModelID:      "claude-fable-5-2-low-fast",
		GroupName:            "cursor_account1",
		CursorAccountID:      accountID,
		FastMode:             true,
		ThinkingEffort:       "low",
		CursorRequestContext: remote,
		Messages: []Message{
			{Role: "user", Content: "Run one shell command that prints the kernel name. Use the shell named in your environment. Do not explain."},
			{Role: "assistant", ToolCalls: []ToolCallDescriptor{{ID: "call-uname", Type: "function", Function: ToolCallFunctionShape{Name: "Shell", Arguments: command}}}},
			{Role: "tool", Name: "Shell", ToolCallID: "call-uname", Content: "Linux\n"},
		},
		Stream: true,
	}, func(event ModelEvent) error {
		switch event.Kind {
		case ModelEventKindTextDelta, ModelEventKindThinkingDelta:
			reply.WriteString(event.Text)
		case ModelEventKindToolLikeCompleted:
			if event.ToolInvocation != nil {
				followTools = append(followTools, event.ToolInvocation.ToolName+":"+string(event.ToolInvocation.ArgsJSON))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%v frames=%s", err, strings.Join(cursorAccountFrameLog(), ","))
	}
	if strings.TrimSpace(reply.String()) == "" && len(followTools) == 0 {
		t.Fatalf("follow-up stalled after %s frames=%s", command, strings.Join(cursorAccountFrameLog(), ","))
	}
	combined := strings.ToLower(reply.String() + strings.Join(followTools, "\n"))
	if strings.Contains(combined, "get-location") || strings.Contains(combined, "powershell") || strings.Contains(combined, "get-psdrive") {
		t.Fatalf("follow-up still used windows commands: %s", trimLive(combined))
	}
	t.Logf("follow-up=%s", trimLive(combined))
}

func TestLiveFableLowFastContinuesAfterLargeShell(t *testing.T) {
	if os.Getenv("CURSOR_FABLE_LIVE") != "1" {
		t.Skip("set CURSOR_FABLE_LIVE=1 to run the live Fable large-shell check")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".cursor-local-assistant-v2", "data")
	manager := cursoraccount.NewManager(root, filepath.Join(root, "cursor-account.json"), &http.Client{Timeout: 3 * time.Minute})
	accounts, err := manager.ListAccounts()
	if err != nil || len(accounts) == 0 {
		t.Fatalf("cursor account 1 is not available: %v", err)
	}
	accountID := accounts[0].ID
	if _, err := manager.AuthorizationFor(context.Background(), accountID); err != nil && manager.AccountIsLocked(accountID) {
		t.Logf("cursor account 1 is device-locked; the gateway may use the current unlocked account")
	}
	gateway := OpenCursorAccountGateway(manager)
	stateDir, err := os.MkdirTemp("", "cursor-fable-large-shell")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("CURSOR_BYOK_HOME", stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	results := make(chan *agentv1.ExecClientMessage, 4)
	var reply strings.Builder
	var followTools []string
	sent := false
	err = gateway.Stream(ctx, StreamRequest{
		RequestID:       uuid.NewString(),
		RunID:           uuid.NewString(),
		ModelCallID:     uuid.NewString(),
		ConversationID:  uuid.NewString(),
		Mode:            agentv1.AgentMode_AGENT_MODE_AGENT,
		ModelID:         "claude-fable-5-2-low-fast",
		ProviderModelID: "claude-fable-5-2-low-fast",
		GroupName:       "cursor_account1",
		CursorAccountID: accountID,
		FastMode:        true,
		ThinkingEffort:  "low",
		Messages: []Message{{
			Role:    "user",
			Content: "Run one shell command that prints two hundred lines of the letter x. After the command result arrives, reply with the single word done. Do not run a second command.",
		}},
		CursorToolResults: results,
		Stream:            true,
	}, func(event ModelEvent) error {
		switch event.Kind {
		case ModelEventKindTextDelta, ModelEventKindThinkingDelta:
			if sent {
				reply.WriteString(event.Text)
			}
		case ModelEventKindToolLikeCompleted:
			if event.ToolInvocation == nil {
				return nil
			}
			if !sent && event.ToolInvocation.ToolName == "Shell" {
				sent = true
				exec := &agentv1.ExecServerMessage{}
				_ = proto.Unmarshal(event.ToolInvocation.NativeExec, exec)
				output := strings.Repeat("x\n", 200)
				output += strings.Repeat("A", 8000) + "MIDDLE" + strings.Repeat("Z", 8000)
				shaped := CursorAccountShellOutput(output)
				results <- &agentv1.ExecClientMessage{
					Id:     exec.GetId(),
					ExecId: exec.GetExecId(),
					Message: &agentv1.ExecClientMessage_ShellStream{
						ShellStream: &agentv1.ShellStream{
							Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: shaped}},
						},
					},
				}
				results <- &agentv1.ExecClientMessage{
					Id:     exec.GetId(),
					ExecId: exec.GetExecId(),
					Message: &agentv1.ExecClientMessage_ShellStream{
						ShellStream: &agentv1.ShellStream{
							Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: 0, Cwd: "/root/teapot"}},
						},
					},
				}
				return nil
			}
			followTools = append(followTools, event.ToolInvocation.ToolName)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		t.Fatalf("%v frames=%s", err, strings.Join(cursorAccountFrameLog(), ","))
	}
	if !sent || (strings.TrimSpace(reply.String()) == "" && len(followTools) == 0) {
		t.Fatalf("large shell result stalled sent=%t reply=%q tools=%v frames=%s", sent, trimLive(reply.String()), followTools, strings.Join(cursorAccountFrameLog(), ","))
	}
	t.Logf("continued reply=%s tools=%v", trimLive(reply.String()), followTools)
}

func TestLiveGrokMaxModeFlag(t *testing.T) {
	if os.Getenv("CURSOR_GROK_PROBE") != "1" {
		t.Skip("set CURSOR_GROK_PROBE=1 to probe grok max mode")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".cursor-local-assistant-v2", "data")
	manager := cursoraccount.NewManager(root, filepath.Join(root, "cursor-account.json"), &http.Client{Timeout: 30 * time.Second})
	authorization, err := manager.Authorization(context.Background())
	if err != nil {
		t.Fatalf("cursor account is not signed in: %v", err)
	}
	param := func(id, value string) *agentv1.RequestedModel_ModelParameterValue {
		return &agentv1.RequestedModel_ModelParameterValue{Id: id, Value: value}
	}
	long := []*agentv1.RequestedModel_ModelParameterValue{
		param("context", "500k"),
		param("reasoning_effort", "high"),
		param("fast", "true"),
	}
	short := []*agentv1.RequestedModel_ModelParameterValue{
		param("context", "256k"),
		param("reasoning_effort", "high"),
		param("fast", "true"),
	}
	cases := []struct {
		name    string
		params  []*agentv1.RequestedModel_ModelParameterValue
		maxMode bool
	}{
		{name: "256k no max", params: short},
		{name: "500k no max", params: long},
		{name: "500k max", params: long, maxMode: true},
	}
	for _, tc := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		status := probeCursorModelShape(ctx, authorization, &agentv1.RequestedModel{ModelId: "grok-4.7", MaxMode: tc.maxMode, Parameters: tc.params})
		cancel()
		t.Logf("%s => %s", tc.name, status)
	}
}

func probeCursorModelShape(ctx context.Context, authorization string, requested *agentv1.RequestedModel) string {
	run := cursorAccountRunMessage(StreamRequest{
		ConversationID: uuid.NewString(),
		Mode:           agentv1.AgentMode_AGENT_MODE_AGENT,
		Messages:       []Message{{Role: "user", Content: "Reply with the single word ok."}},
	}, "probe")
	run.GetRunRequest().RequestedModel = requested
	host := cursorAccountAgentHost(ctx, authorization)
	requestID := uuid.NewString()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(host, "/")+cursorAccountRunPath, bytes.NewReader(cursorAccountFrame(mustCursorProto(&aiserverv1.BidiRequestId{RequestId: requestID}))))
	if err != nil {
		return err.Error()
	}
	applyCursorAccountHeaders(httpReq, authorization, "application/connect+proto")
	resp, err := cursorAccountHTTPClient.Do(httpReq)
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = appendCursorAccount(ctx, authorization, requestID, 0, mustCursorProto(run))
	}()
	buf := make([]byte, 8192)
	pending := make([]byte, 0, 8192)
	deadline := time.NewTimer(18 * time.Second)
	defer deadline.Stop()
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
		}
		for len(pending) >= 5 {
			size := int(pending[1])<<24 | int(pending[2])<<16 | int(pending[3])<<8 | int(pending[4])
			if size < 0 || size > 1<<20 || len(pending) < 5+size {
				break
			}
			flags := pending[0]
			payload := append([]byte(nil), pending[5:5+size]...)
			pending = pending[5+size:]
			if flags&0x01 != 0 {
				decoded, err := cursorAccountGunzip(payload)
				if err != nil {
					continue
				}
				payload = decoded
			}
			if flags&0x02 != 0 {
				text := strings.TrimSpace(string(payload))
				if json.Valid(payload) {
					text = string(payload)
				}
				text = strings.Join(strings.Fields(text), " ")
				if len(text) > 220 {
					text = text[:220]
				}
				return "end " + text
			}
			msg := &agentv1.AgentServerMessage{}
			if proto.Unmarshal(payload, msg) == nil && (msg.GetInteractionUpdate().GetTextDelta() != nil || msg.GetInteractionUpdate().GetThinkingDelta() != nil || msg.GetExecServerMessage() != nil) {
				return "accepted"
			}
		}
		if readErr != nil {
			return "closed"
		}
		select {
		case <-ctx.Done():
			return "timeout"
		case <-deadline.C:
			return "timeout"
		default:
		}
	}
}

func collectCursorAccountText(ctx context.Context, gateway AccountModelGateway, conversationID string, messages []Message) (string, error) {
	var text strings.Builder
	err := gateway.Stream(ctx, StreamRequest{
		RequestID:       uuid.NewString(),
		RunID:           uuid.NewString(),
		ModelCallID:     uuid.NewString(),
		ConversationID:  conversationID,
		Mode:            agentv1.AgentMode_AGENT_MODE_AGENT,
		ModelID:         "claude-fable-5-2-low-fast",
		ProviderModelID: "claude-fable-5-2-low-fast",
		GroupName:       "cursor_account1",
		FastMode:        true,
		ThinkingEffort:  "low",
		Messages:        messages,
		Stream:          true,
	}, func(event ModelEvent) error {
		if event.Kind == ModelEventKindTextDelta {
			text.WriteString(event.Text)
		}
		return nil
	})
	return text.String(), err
}

func trimLive(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 240 {
		return text[:240]
	}
	return text
}

// TestLiveModelSwitchAndSynthesizedHistoryPreservesContext verifies both layers
// against the live Cursor agent server:
//  1. Pre-turn snapshot restore when switching from Gemini 4 Neon -> Grok 4.7 on a retry.
//  2. Synthesized RootPromptMessagesJson + KV blobs when no upstream checkpoint exists.
func TestLiveModelSwitchAndSynthesizedHistoryPreservesContext(t *testing.T) {
	if os.Getenv("CURSOR_FABLE_LIVE") != "1" {
		t.Skip("set CURSOR_FABLE_LIVE=1 to run the live model-switch memory check")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".cursor-local-assistant-v2", "data")
	manager := cursoraccount.NewManager(root, filepath.Join(root, "cursor-account.json"), &http.Client{Timeout: 3 * time.Minute})
	if _, err := manager.Authorization(context.Background()); err != nil {
		t.Fatalf("cursor account is not signed in: %v", err)
	}
	gateway := OpenCursorAccountGateway(manager)
	stateDir, err := os.MkdirTemp("", "cursor-model-switch-live-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("CURSOR_BYOK_HOME", stateDir)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	remoteCtx := &agentv1.RequestContext{
		Env: &agentv1.RequestContextEnv{
			OsVersion:      "linux 6.12.41-test",
			Shell:          "/bin/bash",
			WorkspacePaths: []string{"/root/ecker-soyjak"},
			ProjectFolder:  "/root/ecker-soyjak",
		},
	}

	callModel := func(convID, modelID string, params []ModelParameter, messages []Message) (string, error) {
		var out strings.Builder
		err := gateway.Stream(ctx, StreamRequest{
			RequestID:            uuid.NewString(),
			RunID:                uuid.NewString(),
			ModelCallID:          uuid.NewString(),
			ConversationID:       convID,
			Mode:                 agentv1.AgentMode_AGENT_MODE_AGENT,
			ModelID:              modelID,
			ProviderModelID:      modelID,
			GroupName:            "cursor_account1",
			ModelParameters:      params,
			CursorRequestContext: remoteCtx,
			Messages:             messages,
			Stream:               true,
		}, func(event ModelEvent) error {
			if event.Kind == ModelEventKindTextDelta {
				out.WriteString(event.Text)
			}
			return nil
		})
		return out.String(), err
	}

	grokParams := []ModelParameter{{ID: "reasoning_effort", Value: "low"}}

	// Part 1: Gemini 4 Neon Turn 1 -> snapshot before Turn 2 -> simulate dirty/cleared active state -> restore -> Grok 4.7 Turn 2.
	convSwitch := uuid.NewString()
	t.Cleanup(func() { ClearCursorAccountStateAndSnapshots(convSwitch) })
	r1, err := callModel(convSwitch, "gemini-4-neon", nil, []Message{
		{Role: "user", Content: "Remember the secret codeword: ORBITAL77. Reply with OK only."},
	})
	if err != nil {
		t.Fatalf("Gemini 4 turn 1 failed: %v", err)
	}
	t.Logf("Gemini 4 turn 1: %q (used=%d)", trimLive(r1), CursorAccountUsedTokens(convSwitch))
	SnapshotCursorAccountStateBeforeTurn(convSwitch, 2)
	ClearCursorAccountState(convSwitch) // simulate active state lost/dirty on failed turn
	if !RestoreCursorAccountStateBeforeTurn(convSwitch, 2) {
		t.Fatal("RestoreCursorAccountStateBeforeTurn(2) returned false")
	}
	r2, err := callModel(convSwitch, "grok-4.7", grokParams, []Message{
		{Role: "user", Content: "Remember the secret codeword: ORBITAL77. Reply with OK only."},
		{Role: "assistant", Content: r1},
		{Role: "user", Content: "What is the secret codeword? Reply with ONLY the codeword."},
	})
	if err != nil {
		t.Fatalf("Grok 4.7 switched turn 2 failed: %v", err)
	}
	t.Logf("Grok 4.7 turn 2 (restored checkpoint): %q (used=%d)", trimLive(r2), CursorAccountUsedTokens(convSwitch))
	if !strings.Contains(strings.ToUpper(r2), "ORBITAL77") {
		t.Fatalf("Grok 4.7 lost context after restoring Gemini 4 checkpoint: %q", trimLive(r2))
	}

	// Part 2: No saved checkpoint at all (pure Layer 1 RootPromptMessagesJson synthesis) with summary + tool call/result.
	convSynth := uuid.NewString()
	t.Cleanup(func() { ClearCursorAccountStateAndSnapshots(convSynth) })
	r3, err := callModel(convSynth, "grok-4.7", grokParams, []Message{
		{Role: "system", Content: "local system prompt"},
		{Role: "user", Content: "<conversation_summary>\nWe are debugging ecker-soyjak decryption.\n</conversation_summary>"},
		{Role: "user", Content: "Read /root/ecker-soyjak/token.txt"},
		{Role: "assistant", Content: "Reading token.txt.", ToolCalls: []ToolCallDescriptor{{
			ID: "call-synth-1", Type: "function", Function: ToolCallFunctionShape{Name: "Read", Arguments: `{"path":"/root/ecker-soyjak/token.txt"}`},
		}}},
		{Role: "tool", Name: "Read", ToolCallID: "call-synth-1", Content: "DEPLOY_CODE=NEBULA42"},
		{Role: "assistant", Content: "I read token.txt."},
		{Role: "user", Content: "What is DEPLOY_CODE and what is your workspace path? Reply concisely without calling tools."},
	})
	if err != nil {
		t.Fatalf("Grok 4.7 synthesized history turn failed: %v", err)
	}
	t.Logf("Grok 4.7 synthesized turn: %q (used=%d)", trimLive(r3), CursorAccountUsedTokens(convSynth))
	if !strings.Contains(strings.ToUpper(r3), "NEBULA42") {
		t.Fatalf("Grok 4.7 lost synthesized tool-result history: %q", trimLive(r3))
	}
	if !strings.Contains(r3, "/root/ecker-soyjak") {
		t.Fatalf("Grok 4.7 did not get fresh <user_info> workspace path: %q", trimLive(r3))
	}
}


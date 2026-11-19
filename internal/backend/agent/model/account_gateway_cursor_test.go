package modeladapter

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/gen/aiserverv1"

	"google.golang.org/protobuf/proto"
)

type staticCursorAuthorizer struct {
	token string
	err   error
}

func (auth staticCursorAuthorizer) Authorization(context.Context) (string, error) {
	if auth.err != nil {
		return "", auth.err
	}
	return auth.token, nil
}

type lockDirectory struct {
	staticCursorAuthorizer
	locked  map[string]bool
	current string
	apiKey  map[string]bool
}

func (dir lockDirectory) AccountIsLocked(accountID string) bool { return dir.locked[accountID] }
func (dir lockDirectory) CurrentAccountID() string              { return dir.current }
func (dir lockDirectory) UsesAPIKey(accountID string) bool      { return dir.apiKey[accountID] }
func (dir lockDirectory) ForceRefresh(context.Context, string) (string, error) {
	return "", context.Canceled
}
func (dir lockDirectory) MarkDeviceLocked(string) error { return nil }

func staticCursorAccountAppender(authorization, requestID string) cursorAccountAppendFunc {
	return func(ctx context.Context, seq int64, payload []byte) error {
		return appendCursorAccount(ctx, authorization, requestID, seq, payload)
	}
}

func TestReadCursorAccountStreamReportsMissingAccessWhenSilent(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := readCursorAccountStream(ctx, staticCursorAccountAppender("Bearer test", "req"), "req", "conv", "claude-fable-5", "cursor_account1", 40*time.Millisecond, reader, nil, nil, nil, nil, func(ModelEvent) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "does not have access to claude-fable-5") {
		t.Fatalf("err = %v", err)
	}
}

func TestCursorAccountAccessDenialReadsDetail(t *testing.T) {
	raw := []byte(`{"error":{"details":{"title":"Upgrade required","detail":"claude-fable is not included in your plan"}}}`)
	if got := cursorAccountAccessDenial(raw); !strings.Contains(got, "not included in your plan") {
		t.Fatalf("denial = %q", got)
	}
	if got := cursorAccountAccessDenial([]byte(`{"text":"hello"}`)); got != "" {
		t.Fatalf("ordinary text flagged as denial: %q", got)
	}
	// Model output that quotes this checker used to abort the stream. The
	// phrase and a nearby "detail" field are not an account error.
	quoted := []byte("this account does not have access to claude-fable-5-2 \"detail\":\"); detail != \"")
	if got := cursorAccountAccessDenial(quoted); got != "" {
		t.Fatalf("model text flagged as denial: %q", got)
	}
}

func TestCursorAccountErrorTextUnescapesEscapedQuotes(t *testing.T) {
	raw := []byte(`{"error":{"code":"not_found","details":[{"debug":{"details":{"title":"AI Model Not Found","detail":"Invalid parameters for registry model: \"grok-4.7\" requires \"reasoning_effort\""}}}]}}`)
	got := cursorAccountErrorText(raw)
	want := `Invalid parameters for registry model: "grok-4.7" requires "reasoning_effort"`
	if got != want {
		t.Fatalf("error text = %q, want %q", got, want)
	}
}

func TestReadCursorAccountStreamKeepsGzippedCheckpoint(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	const conversationID = "conv-gzip-checkpoint"
	raw, err := proto.Marshal(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ConversationCheckpointUpdate{
			ConversationCheckpointUpdate: &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("saved-turn")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	frame := cursorAccountFrame(compressed.Bytes())
	frame[0] = 0x01
	err = readCursorAccountStream(context.Background(), staticCursorAccountAppender("Bearer test", "req"), "req", conversationID, "claude-fable-5-2-low-fast", "cursor_account1", 0, bytes.NewReader(frame), nil, nil, nil, nil, func(ModelEvent) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state := cursorAccountLoadState(conversationID)
	if state == nil || len(state.GetTurns()) != 1 || string(state.GetTurns()[0]) != "saved-turn" {
		t.Fatalf("checkpoint = %#v", state)
	}
}

func TestCursorAccountFollowUpSendsSavedCheckpoint(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	const id = "conv-memory"
	cursorAccountRememberState(id, &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn-one")}})
	follow := cursorAccountRunMessage(StreamRequest{
		ConversationID: id,
		Messages: []Message{
			{Role: "user", Content: "Say banana 5 times"},
			{Role: "assistant", Content: "banana banana"},
			{Role: "user", Content: "what did u just say?"},
		},
	}, "claude-fable-5-2")
	state := follow.GetRunRequest().GetConversationState()
	if len(state.GetTurns()) != 1 || string(state.GetTurns()[0]) != "turn-one" {
		t.Fatalf("turns = %q", state.GetTurns())
	}
	text := follow.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if text != "what did u just say?" {
		t.Fatalf("text = %q", text)
	}
	if follow.GetRunRequest().GetAction().GetUserMessageAction().GetConversationHistory() != nil {
		t.Fatal("checkpoint follow-up also duplicated the transcript")
	}
}

func TestCursorAccountBlobRoundTrip(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	id := []byte("blob-id")
	set := cursorAccountKVReply(&agentv1.KvServerMessage{
		Id: 1,
		Message: &agentv1.KvServerMessage_SetBlobArgs{SetBlobArgs: &agentv1.SetBlobArgs{
			BlobId: id, BlobData: []byte("turns"),
		}},
	})
	if set.GetKvClientMessage().GetSetBlobResult() == nil {
		t.Fatal("set was not acknowledged")
	}
	got := cursorAccountKVReply(&agentv1.KvServerMessage{
		Id:      2,
		Message: &agentv1.KvServerMessage_GetBlobArgs{GetBlobArgs: &agentv1.GetBlobArgs{BlobId: id}},
	}).GetKvClientMessage().GetGetBlobResult().GetBlobData()
	if string(got) != "turns" {
		t.Fatalf("blob = %q", got)
	}
}

func TestCursorAccountUserFollowUpSendsOnlyTheNewTurn(t *testing.T) {
	const stable = "11111111-2222-3333-4444-555555555555"
	follow := cursorAccountRunMessage(StreamRequest{
		ConversationID: stable,
		Messages: []Message{
			{Role: "system", Content: "Reply in English and Indonesian."},
			{Role: "user", Content: "inspect the pool"},
			{Role: "assistant", Content: "I already explained the pool in detail."},
			{Role: "user", Content: "continue"},
		},
	}, "claude-fable-5-2")
	if follow.GetRunRequest().GetConversationId() != stable {
		t.Fatalf("conversation = %q", follow.GetRunRequest().GetConversationId())
	}
	text := follow.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if text != "continue" {
		t.Fatalf("prompt = %q", text)
	}
	history := cursorAccountHistoryPlain(follow)
	if !strings.Contains(history, "inspect the pool") || !strings.Contains(history, "I already explained the pool") {
		t.Fatalf("history = %q", history)
	}
	if strings.Contains(history, "English and Indonesian") || strings.Contains(text, "English and Indonesian") {
		t.Fatal("system prompt was sent to the cursor account agent")
	}
}

func TestReadCursorAccountStreamKeepsModelTextThatMentionsAccess(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	go func() {
		defer writer.Close()
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TextDelta{
						TextDelta: &agentv1.TextDeltaUpdate{Text: "this account does not have access to claude-fable-5-2 is not a real error"},
					},
				},
			},
		})
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{}},
				},
			},
		})
	}()
	var text string
	err := readCursorAccountStream(context.Background(), staticCursorAccountAppender("Bearer test", "req"), "req", "conv", "claude-fable-5-2", "cursor_account1", 0, reader, nil, nil, nil, nil, func(event ModelEvent) error {
		if event.Kind == ModelEventKindTextDelta {
			text += event.Text
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "does not have access") {
		t.Fatalf("text = %q", text)
	}
}

func TestUsableCursorAccountIDSkipsLockedAccount(t *testing.T) {
	auth := lockDirectory{
		locked:  map[string]bool{"locked-account": true},
		current: "working-account",
	}
	if got := usableCursorAccountID(auth, "locked-account"); got != "working-account" {
		t.Fatalf("got %q, want the current unlocked account", got)
	}
	if got := usableCursorAccountID(auth, "working-account"); got != "working-account" {
		t.Fatalf("unlocked selection changed: %q", got)
	}
	auth.current = "locked-account"
	if got := usableCursorAccountID(auth, "locked-account"); got != "locked-account" {
		t.Fatalf("no unlocked fallback should keep the requested account, got %q", got)
	}
}

func TestCursorAccountGatewayStreamsText(t *testing.T) {
	events := runCursorAccountFixture(t, nil, func(w http.ResponseWriter, flusher http.Flusher, appended <-chan int64) {
		if !waitCursorAppend(appended, 0) {
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_ExecServerMessage{
				ExecServerMessage: &agentv1.ExecServerMessage{
					Id:      3,
					ExecId:  "ctx",
					Message: &agentv1.ExecServerMessage_RequestContextArgs{RequestContextArgs: &agentv1.RequestContextArgs{}},
				},
			},
		})
		flusher.Flush()
		if !waitCursorAppend(appended, 1) {
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: "hello"}},
				},
			},
		})
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{}},
				},
			},
		})
		flusher.Flush()
	})
	var text string
	finished := false
	for _, event := range events {
		if event.Kind == ModelEventKindTextDelta {
			text += event.Text
		}
		if event.Kind == ModelEventKindTurnFinished {
			finished = true
		}
	}
	if text != "hello" || !finished {
		t.Fatalf("events = %+v", events)
	}
	for _, event := range events {
		if event.Kind == ModelEventKindTextDelta && event.Provider != "cursor_account2" {
			t.Fatalf("provider = %q, want cursor_account2", event.Provider)
		}
	}
}

func TestCursorAccountGatewayContinuesAfterToolResult(t *testing.T) {
	results := make(chan *agentv1.ExecClientMessage, 1)
	events := runCursorAccountFixture(t, results, func(w http.ResponseWriter, flusher http.Flusher, appended <-chan int64) {
		if !waitCursorAppend(appended, 0) {
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_ExecServerMessage{
				ExecServerMessage: &agentv1.ExecServerMessage{
					Id:     9,
					ExecId: "read-1",
					Message: &agentv1.ExecServerMessage_ReadArgs{
						ReadArgs: &agentv1.ReadArgs{Path: "notes/todo.md", ToolCallId: "read-1"},
					},
				},
			},
		})
		flusher.Flush()
		if !waitCursorAppend(appended, 1) {
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: "created"}},
				},
			},
		})
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{}},
				},
			},
		})
		flusher.Flush()
	})
	var text, finish string
	for _, event := range events {
		if event.Kind == ModelEventKindTextDelta {
			text += event.Text
		}
		if event.Kind == ModelEventKindTurnFinished {
			finish = event.FinishReason
		}
	}
	if text != "created" || finish != "stop" {
		t.Fatalf("text=%q finish=%q events=%+v", text, finish, events)
	}
}

func TestCursorAccountGrepWithoutPatternIsGlob(t *testing.T) {
	name, _, raw, ok := cursorAccountExecTool(&agentv1.ExecServerMessage{
		Id:     4,
		ExecId: "grep-1",
		Message: &agentv1.ExecServerMessage_GrepArgs{
			GrepArgs: &agentv1.GrepArgs{
				Path:       proto.String(`c:\repo`),
				Glob:       proto.String("**/*test*"),
				OutputMode: proto.String("files_with_matches"),
				ToolCallId: "toolu_1",
			},
		},
	})
	if !ok || name != "Glob" || !strings.Contains(string(raw), "test") {
		t.Fatalf("name=%q ok=%v raw=%s", name, ok, raw)
	}
	contentName, _, _, contentOK := cursorAccountExecTool(&agentv1.ExecServerMessage{
		Message: &agentv1.ExecServerMessage_GrepArgs{
			GrepArgs: &agentv1.GrepArgs{Pattern: "func main", Path: proto.String(".")},
		},
	})
	if !contentOK || contentName != "Grep" {
		t.Fatalf("content grep = %q ok=%v", contentName, contentOK)
	}
	taskName, taskID, _, taskOK := cursorAccountExecTool(&agentv1.ExecServerMessage{
		Id: 1,
		Message: &agentv1.ExecServerMessage_SubagentArgs{
			SubagentArgs: &agentv1.SubagentArgs{ToolCallId: "toolu_parent", Prompt: "inspect the view pool"},
		},
	})
	if !taskOK || taskName != "Task" || taskID != "toolu_parent" {
		t.Fatalf("task name=%q id=%q ok=%v", taskName, taskID, taskOK)
	}
}

func TestCursorAccountGatewayReturnsToolCall(t *testing.T) {
	events := runCursorAccountFixture(t, nil, func(w http.ResponseWriter, flusher http.Flusher, appended <-chan int64) {
		if !waitCursorAppend(appended, 0) {
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_ExecServerMessage{
				ExecServerMessage: &agentv1.ExecServerMessage{
					Id:     9,
					ExecId: "read-1",
					Message: &agentv1.ExecServerMessage_ReadArgs{
						ReadArgs: &agentv1.ReadArgs{Path: "notes/todo.md", ToolCallId: "read-1"},
					},
				},
			},
		})
		flusher.Flush()
	})
	var toolName, finish string
	for _, event := range events {
		if event.Kind == ModelEventKindToolLikeCompleted && event.ToolInvocation != nil {
			toolName = event.ToolInvocation.ToolName
			if !strings.Contains(string(event.ToolInvocation.ArgsJSON), "notes/todo.md") {
				t.Fatalf("args = %s", event.ToolInvocation.ArgsJSON)
			}
		}
		if event.Kind == ModelEventKindTurnFinished {
			finish = event.FinishReason
		}
	}
	if toolName != "Read" || finish != "tool_calls" {
		t.Fatalf("tool=%q finish=%q events=%+v", toolName, finish, events)
	}
}

func TestCursorAccountFollowUpLeavesTheClosedToolRun(t *testing.T) {
	const stable = "11111111-2222-3333-4444-555555555555"
	first := cursorAccountRunMessage(StreamRequest{
		ConversationID: stable,
		Messages:       []Message{{Role: "user", Content: "hi"}},
	}, "claude-fable-5-2-max-fast")
	if first.GetRunRequest().GetConversationId() != stable {
		t.Fatalf("first conversation = %q", first.GetRunRequest().GetConversationId())
	}

	output := strings.Repeat("A", 9000) + "MIDDLE" + strings.Repeat("Z", 9000)
	follow := cursorAccountRunMessage(StreamRequest{
		ConversationID: stable,
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "checking", ReasoningContent: "private chain of thought"},
			{Role: "tool", Name: "Shell", ToolCallID: "call-1", Content: output},
		},
	}, "claude-fable-5-2-max-fast")
	gotID := follow.GetRunRequest().GetConversationId()
	if gotID == "" || gotID == stable {
		t.Fatalf("follow-up conversation = %q, want a fresh id", gotID)
	}
	text := follow.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	history := cursorAccountHistoryPlain(follow)
	if strings.Contains(text, "private chain of thought") || strings.Contains(history, "private chain of thought") {
		t.Fatal("previous reasoning was copied into the follow-up")
	}
	if strings.Contains(text, "[instruction]") || strings.Contains(history, "[instruction]") {
		t.Fatal("follow-up still contains an extra instruction")
	}
	if !strings.Contains(history, "checking") {
		t.Fatal("previous assistant answer was dropped from conversation history")
	}
	if !strings.Contains(history, "Output too long.") || !strings.Contains(history, "AAAA") || !strings.Contains(history, "ZZZZ") {
		t.Fatalf("tool output was not kept at both ends: %s", history)
	}
	if strings.Contains(history, "MIDDLE") || strings.Contains(text, "MIDDLE") {
		t.Fatal("the middle of the terminal output was still sent")
	}
	if follow.GetRunRequest().GetAction().GetResumeAction() == nil {
		t.Fatal("tool follow-up must resume the checkpoint, not invent a user message")
	}
	if strings.Contains(text, "Continue.") || strings.Contains(history, "Continue.") {
		t.Fatal("synthetic Continue. was sent to the model")
	}
}

func TestCursorAccountBackgroundNoticeDoesNotInventContinue(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	notice := "<system_reminder>\n此前转入后台继续运行的委派任务已结束（状态：completed）。任务：inspect routes。（subagent agent_id=child-agent-123）\n结果：\nAll 3 routes verified.\n</system_reminder>"
	run := cursorAccountRunMessage(StreamRequest{
		ConversationID: "conv-bg-only",
		Messages: []Message{
			{Role: "user", Content: "spawn subagent"},
			{Role: "assistant", Content: "spawned"},
			{Role: "user", Content: notice},
			{Role: "tool", Name: "Read", ToolCallID: "call-1", Content: "file body"},
		},
	}, "gemini-4-argon")
	if run.GetRunRequest().GetAction().GetResumeAction() == nil {
		t.Fatal("a turn that ends on a tool result must resume, not send a new user message")
	}
	if run.GetRunRequest().GetAction().GetUserMessageAction() != nil {
		t.Fatal("tool follow-up invented a user message")
	}
	history := cursorAccountHistoryPlain(run)
	if !strings.Contains(history, "file body") || !strings.Contains(history, "spawn subagent") {
		t.Fatalf("history = %q", history)
	}
	if strings.Contains(history, "Continue.") {
		t.Fatal("synthetic Continue. was written into the checkpoint")
	}
}

func TestCursorAccountKeepsLiteralUserContinue(t *testing.T) {
	run := cursorAccountRunMessage(StreamRequest{
		Messages: []Message{{Role: "user", Content: "Continue."}},
	}, "claude-fable-5-2")
	text := run.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if text != "Continue." {
		t.Fatalf("text = %q, want the user's own message", text)
	}
}

func cursorAccountHistoryPlain(message *agentv1.AgentClientMessage) string {
	var builder strings.Builder
	for _, id := range message.GetRunRequest().GetConversationState().GetRootPromptMessagesJson() {
		raw, ok := cursorAccountBlob(id)
		if !ok || len(raw) == 0 {
			continue
		}
		var env struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		var text string
		if json.Unmarshal(env.Content, &text) == nil {
			if isCursorBootstrapScaffoldingText(strings.TrimSpace(text)) {
				continue
			}
			builder.WriteString(text)
			continue
		}
		var parts []struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Result string `json:"result"`
		}
		if json.Unmarshal(env.Content, &parts) == nil {
			for _, part := range parts {
				builder.WriteString(part.Text)
				builder.WriteString(part.Result)
			}
		}
	}
	history := message.GetRunRequest().GetAction().GetUserMessageAction().GetConversationHistory()
	for _, item := range history.GetMessages() {
		if user := item.GetUser(); user != nil {
			for _, content := range user.GetContent() {
				builder.WriteString(content.GetText().GetText())
			}
		}
		if assistant := item.GetAssistant(); assistant != nil {
			for _, content := range assistant.GetContent() {
				builder.WriteString(content.GetText().GetText())
			}
		}
		if tool := item.GetTool(); tool != nil {
			for _, content := range tool.GetContent() {
				builder.WriteString(content.GetText().GetText())
			}
		}
	}
	return builder.String()
}

func TestCursorAccountGatewayAcksBlobWriteBeforeText(t *testing.T) {
	events := runCursorAccountFixture(t, nil, func(w http.ResponseWriter, flusher http.Flusher, appended <-chan int64) {
		if !waitCursorAppend(appended, 0) {
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_KvServerMessage{
				KvServerMessage: &agentv1.KvServerMessage{
					Id:      4,
					Message: &agentv1.KvServerMessage_SetBlobArgs{SetBlobArgs: &agentv1.SetBlobArgs{BlobId: []byte("blob")}},
				},
			},
		})
		flusher.Flush()
		if !waitCursorAppend(appended, 1) {
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: "continued"}},
				},
			},
		})
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{}},
				},
			},
		})
		flusher.Flush()
	})
	var text string
	for _, event := range events {
		if event.Kind == ModelEventKindTextDelta {
			text += event.Text
		}
	}
	if text != "continued" {
		t.Fatalf("text = %q", text)
	}
}

func runCursorAccountFixture(t *testing.T, results chan *agentv1.ExecClientMessage, play func(http.ResponseWriter, http.Flusher, <-chan int64)) []ModelEvent {
	t.Helper()
	appended := make(chan int64, 4)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cursorAccountRunPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		play(w, flusher, appended)
	}))
	defer agent.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cursorAccountServerConfigPath:
			raw, _ := proto.Marshal(&aiserverv1.GetServerConfigResponse{
				AgentUrlConfig: &aiserverv1.AgentUrlConfig{AgentnUrl: agent.URL},
			})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		case cursorAccountBidiAppendPath:
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			msg := &aiserverv1.BidiAppendRequest{}
			if err := proto.Unmarshal(body, msg); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			appended <- msg.GetAppendSeqno()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	previous := cursorAccountBackendRoot
	cursorAccountBackendRoot = api.URL
	t.Cleanup(func() { cursorAccountBackendRoot = previous })

	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var events []ModelEvent
	err := gateway.Stream(ctx, StreamRequest{
		ModelID:           "claude-4.5-opus-high",
		GroupName:         "cursor_account2",
		CursorAccountID:   "account-2",
		ConversationID:    "11111111-2222-3333-4444-555555555555",
		Messages:          []Message{{Role: "user", Content: "hi"}},
		CursorToolResults: results,
	}, func(event ModelEvent) error {
		events = append(events, event)
		if results != nil && event.Kind == ModelEventKindToolLikeCompleted {
			results <- &agentv1.ExecClientMessage{
				Message: &agentv1.ExecClientMessage_ReadResult{
					ReadResult: &agentv1.ReadResult{
						Result: &agentv1.ReadResult_FileNotFound{FileNotFound: &agentv1.ReadFileNotFound{Path: "notes/todo.md"}},
					},
				},
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestCursorAccountKeepsRemoteLinuxEnv(t *testing.T) {
	req := StreamRequest{
		CursorRequestContext: &agentv1.RequestContext{
			Env: &agentv1.RequestContextEnv{
				OsVersion:      "linux 6.12.41+deb13-amd64",
				Shell:          "/bin/bash",
				WorkspacePaths: []string{"/root/teapot"},
				ProjectFolder:  "/root/teapot",
			},
		},
		Messages: []Message{
			{Role: "system", Content: strings.Repeat("local system prompt ", 2000)},
			{Role: "user", Content: "<user_info>\nOS Version: windows\n\nShell: powershell\n</user_info>"},
			{Role: "user", Content: "<system_reminder>\nlocal reminder\n</system_reminder>"},
			{Role: "user", Content: "fix the admin stats page"},
		},
	}
	run := cursorAccountRunMessage(req, "claude-fable-5-2-low-fast")
	env := run.GetRunRequest().GetAction().GetUserMessageAction().GetRequestContext().GetEnv()
	if env.GetOsVersion() != "linux 6.12.41+deb13-amd64" || env.GetShell() != "/bin/bash" {
		t.Fatalf("run env os=%q shell=%q", env.GetOsVersion(), env.GetShell())
	}
	if paths := env.GetWorkspacePaths(); len(paths) != 1 || paths[0] != "/root/teapot" {
		t.Fatalf("workspace = %v", paths)
	}
	for _, path := range pathsOf(env) {
		if strings.Contains(path, `\`) || strings.Contains(strings.ToLower(path), "users") {
			t.Fatalf("remote workspace was replaced with a local path: %q", path)
		}
	}
	text := run.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if text != "fix the admin stats page" {
		t.Fatalf("user text = %q", text)
	}
	history := cursorAccountHistoryPlain(run)
	if strings.Contains(history, "powershell") || strings.Contains(history, "<user_info>") || strings.Contains(history, "local system prompt") || strings.Contains(history, "local reminder") {
		t.Fatalf("compiled prompt leaked into the cursor account run: %s", history)
	}
	reply := cursorAccountContextReply(1, "ctx", cursorAccountRequestContext(req))
	got := reply.GetExecClientMessage().GetRequestContextResult().GetSuccess().GetRequestContext().GetEnv()
	if got.GetOsVersion() != "linux 6.12.41+deb13-amd64" || got.GetShell() != "/bin/bash" {
		t.Fatalf("context reply os=%q shell=%q", got.GetOsVersion(), got.GetShell())
	}
	if got.GetWorkspacePaths()[0] != "/root/teapot" {
		t.Fatalf("context reply workspace = %v", got.GetWorkspacePaths())
	}
}

func TestCursorAccountHostFallbackMatchesThisMachine(t *testing.T) {
	env := CursorHostRequestContext().GetEnv()
	if env.GetOsVersion() == "" || env.GetOsVersion() == "windows" || strings.TrimSpace(env.GetShell()) == "" {
		t.Fatalf("host env os=%q shell=%q", env.GetOsVersion(), env.GetShell())
	}
	if len(env.GetWorkspacePaths()) == 0 || strings.TrimSpace(env.GetProjectFolder()) == "" {
		t.Fatalf("host workspace missing: %+v", env)
	}
	switch runtime.GOOS {
	case "windows":
		if !strings.HasPrefix(env.GetOsVersion(), "win32 ") || env.GetShell() != "powershell" {
			t.Fatalf("windows host env os=%q shell=%q", env.GetOsVersion(), env.GetShell())
		}
	case "linux":
		if !strings.HasPrefix(env.GetOsVersion(), "linux ") || !strings.Contains(env.GetShell(), "bash") && !strings.HasPrefix(env.GetShell(), "/") {
			t.Fatalf("linux host env os=%q shell=%q", env.GetOsVersion(), env.GetShell())
		}
	}
}

func TestCursorAccountFillsLinuxShellWithoutLocalPaths(t *testing.T) {
	req := StreamRequest{CursorRequestContext: &agentv1.RequestContext{
		Env: &agentv1.RequestContextEnv{OsVersion: "linux 6.12.41+deb13-amd64"},
	}}
	env := cursorAccountRequestContext(req).GetEnv()
	if runtime.GOOS == "linux" {
		if shell := strings.ToLower(env.GetShell()); shell == "" || strings.Contains(shell, "powershell") {
			t.Fatalf("shell = %q", env.GetShell())
		}
		return
	}
	if env.GetShell() != "/bin/bash" {
		t.Fatalf("shell = %q, want /bin/bash for a remote linux host", env.GetShell())
	}
	for _, path := range env.GetWorkspacePaths() {
		if strings.Contains(path, `\`) {
			t.Fatalf("windows path leaked onto the linux host: %q", path)
		}
	}
	if folder := env.GetProjectFolder(); strings.Contains(folder, `\`) {
		t.Fatalf("windows project folder leaked: %q", folder)
	}
}

func TestCursorAccountShellExitClosesTheExecStream(t *testing.T) {
	var mu sync.Mutex
	var got []*agentv1.AgentClientMessage
	appended := make(chan struct{}, 8)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cursorAccountRunPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer agent.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cursorAccountServerConfigPath:
			raw, _ := proto.Marshal(&aiserverv1.GetServerConfigResponse{
				AgentUrlConfig: &aiserverv1.AgentUrlConfig{AgentnUrl: agent.URL},
			})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		case cursorAccountBidiAppendPath:
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			req := &aiserverv1.BidiAppendRequest{}
			if err := proto.Unmarshal(body, req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			raw, err := hex.DecodeString(req.GetData())
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			msg := &agentv1.AgentClientMessage{}
			if err := proto.Unmarshal(raw, msg); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			got = append(got, msg)
			mu.Unlock()
			appended <- struct{}{}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	previous := cursorAccountBackendRoot
	cursorAccountBackendRoot = api.URL
	t.Cleanup(func() { cursorAccountBackendRoot = previous })

	results := make(chan *agentv1.ExecClientMessage, 2)
	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- gateway.Stream(ctx, StreamRequest{
			ModelID:           "claude-fable-5-2-low-fast",
			GroupName:         "cursor_account1",
			ConversationID:    "11111111-2222-3333-4444-555555555555",
			Messages:          []Message{{Role: "user", Content: "list the directory"}},
			CursorToolResults: results,
		}, func(ModelEvent) error { return nil })
	}()
	if !waitCursorAppendSignal(appended) {
		t.Fatal("run request was not appended")
	}
	output := strings.Repeat("A", 6000) + "MIDDLE" + strings.Repeat("Z", 6000)
	results <- &agentv1.ExecClientMessage{
		Id:     7,
		ExecId: "exec-7",
		Message: &agentv1.ExecClientMessage_ShellStream{
			ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Stdout{
					Stdout: &agentv1.ShellStreamStdout{Data: CursorAccountShellOutput(output)},
				},
			},
		},
	}
	results <- &agentv1.ExecClientMessage{
		Id:     7,
		ExecId: "exec-7",
		Message: &agentv1.ExecClientMessage_ShellStream{
			ShellStream: &agentv1.ShellStream{
				Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: 0}},
			},
		},
	}
	close(results)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var stdout string
	closes := 0
	for _, msg := range got {
		if data := msg.GetExecClientMessage().GetShellStream().GetStdout().GetData(); data != "" {
			stdout = data
		}
		if msg.GetExecClientControlMessage().GetStreamClose() != nil {
			closes++
			if msg.GetExecClientControlMessage().GetStreamClose().GetId() != 7 {
				t.Fatalf("stream close id = %d", msg.GetExecClientControlMessage().GetStreamClose().GetId())
			}
		}
	}
	if closes != 1 {
		t.Fatalf("stream close count = %d", closes)
	}
	if !strings.Contains(stdout, "AAAA") || !strings.Contains(stdout, "ZZZZ") || strings.Contains(stdout, "MIDDLE") || !strings.Contains(stdout, "Output too long.") {
		t.Fatalf("stdout = %s", stdout)
	}
}

func waitCursorAppendSignal(appended <-chan struct{}) bool {
	select {
	case <-appended:
		return true
	case <-time.After(4 * time.Second):
		return false
	}
}

func pathsOf(env *agentv1.RequestContextEnv) []string {
	if env == nil {
		return nil
	}
	return append(append([]string{}, env.GetWorkspacePaths()...), env.GetProjectFolder(), env.GetProcessWorkingDirectory())
}

func waitCursorAppend(appended <-chan int64, want int64) bool {
	select {
	case seq := <-appended:
		return seq == want
	case <-time.After(4 * time.Second):
		return false
	}
}

func TestCursorAccountRequestedModelKeepsRegistryParameterIDs(t *testing.T) {
	requested := cursorAccountRequestedModel("grok-4.7", "low", true, false, []ModelParameter{
		{ID: "context", Value: "256k"},
		{ID: "reasoning_effort", Value: "low"},
		{ID: "fast", Value: "true"},
	}, "", 0)
	if requested.GetModelId() != "grok-4.7" {
		t.Fatalf("model id = %q", requested.GetModelId())
	}
	got := parameterPairs(requested)
	want := []string{"context=256k", "reasoning_effort=low", "fast=true"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parameters = %v, want %v", got, want)
	}
}

func TestCursorAccountRequestedModelStillSynthesizesEffortForSlugs(t *testing.T) {
	requested := cursorAccountRequestedModel("claude-fable-5-2-low-fast", "", false, false, nil, "", 0)
	if requested.GetModelId() != "claude-fable-5-2" {
		t.Fatalf("model id = %q", requested.GetModelId())
	}
	got := parameterPairs(requested)
	want := []string{"effort=low", "fast=true"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parameters = %v, want %v", got, want)
	}
}

func parameterPairs(model *agentv1.RequestedModel) []string {
	pairs := make([]string, 0, len(model.GetParameters()))
	for _, parameter := range model.GetParameters() {
		pairs = append(pairs, parameter.GetId()+"="+parameter.GetValue())
	}
	return pairs
}

func TestReadCursorAccountStreamReturnsAskQuestionAnswerOnTheSameStream(t *testing.T) {
	query := &agentv1.InteractionQuery{
		Id: 7,
		Query: &agentv1.InteractionQuery_AskQuestionInteractionQuery{
			AskQuestionInteractionQuery: &agentv1.AskQuestionInteractionQuery{
				ToolCallId: "ask-1",
				Args: &agentv1.AskQuestionArgs{
					Title: "Confirm",
					Questions: []*agentv1.AskQuestionArgs_Question{{
						Id:     "q1",
						Prompt: "Ship it?",
					}},
				},
			},
		},
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	answered := make(chan struct{})
	go func() {
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionQuery{InteractionQuery: query},
		})
		<-answered
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{}},
				},
			},
		})
		_ = writer.Close()
	}()
	replies := make(chan *agentv1.InteractionResponse, 1)
	delivered := make(chan *agentv1.AgentClientMessage, 1)
	var toolName, callID string
	err := readCursorAccountStream(context.Background(), func(_ context.Context, _ int64, payload []byte) error {
		msg := &agentv1.AgentClientMessage{}
		if proto.Unmarshal(payload, msg) == nil && msg.GetInteractionResponse() != nil {
			delivered <- msg
		}
		return nil
	}, "req-ask", "conv-ask", "grok-4.7", "cursor_account1", 0, reader, nil, nil, replies, nil, func(event ModelEvent) error {
		if event.Kind != ModelEventKindToolLikeCompleted || event.ToolInvocation == nil {
			return nil
		}
		toolName = event.ToolInvocation.ToolName
		callID = event.ToolInvocation.CallID
		replies <- &agentv1.InteractionResponse{
			Id: 7,
			Result: &agentv1.InteractionResponse_AskQuestionInteractionResponse{
				AskQuestionInteractionResponse: &agentv1.AskQuestionInteractionResponse{
					Result: &agentv1.AskQuestionResult{
						Result: &agentv1.AskQuestionResult_Success{
							Success: &agentv1.AskQuestionSuccess{},
						},
					},
				},
			},
		}
		close(answered)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if toolName != "AskQuestion" || callID != "ask-1" {
		t.Fatalf("tool = %s %s, want AskQuestion ask-1", toolName, callID)
	}
	select {
	case replied := <-delivered:
		if replied.GetInteractionResponse().GetId() != 7 {
			t.Fatalf("upstream interaction reply id = %d", replied.GetInteractionResponse().GetId())
		}
	case <-time.After(time.Second):
		t.Fatal("interaction answer was not written back to the cursor stream")
	}
}

func writeCursorAgentFrame(w io.Writer, msg proto.Message) {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return
	}
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	_, _ = w.Write(append(header, payload...))
}

func TestReadCursorAccountStreamDrainsPostTurnEndedKVAndTerminalCheckpoint(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	const conversationID = "conv-grok47-post-turn-checkpoint"

	sysID := bytes.Repeat([]byte{0x11}, 32)
	infoID := bytes.Repeat([]byte{0x22}, 32)
	userID := bytes.Repeat([]byte{0x33}, 32)
	asstID := bytes.Repeat([]byte{0x44}, 32)
	turnID := bytes.Repeat([]byte{0x55}, 32)

	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	kvAcked := make(chan uint32, 8)
	appendFn := func(_ context.Context, _ int64, payload []byte) error {
		msg := &agentv1.AgentClientMessage{}
		if err := proto.Unmarshal(payload, msg); err == nil && msg.GetKvClientMessage() != nil {
			kvAcked <- msg.GetKvClientMessage().GetId()
		}
		return nil
	}

	go func() {
		defer writer.Close()
		// 1. Pre-turn bootstrap blobs + empty turns=0 checkpoint (what Grok 4.7 sends before answering).
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_KvServerMessage{
				KvServerMessage: &agentv1.KvServerMessage{
					Id: 1,
					Message: &agentv1.KvServerMessage_SetBlobArgs{SetBlobArgs: &agentv1.SetBlobArgs{
						BlobId:   sysID,
						BlobData: []byte(`{"role":"system","content":"You are powered by Grok 4.7."}`),
					}},
				},
			},
		})
		<-kvAcked
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_KvServerMessage{
				KvServerMessage: &agentv1.KvServerMessage{
					Id: 2,
					Message: &agentv1.KvServerMessage_SetBlobArgs{SetBlobArgs: &agentv1.SetBlobArgs{
						BlobId:   infoID,
						BlobData: []byte(`{"role":"user","content":"<user_info>\nOS Version: win32\n</user_info>"}`),
					}},
				},
			},
		})
		<-kvAcked
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_ConversationCheckpointUpdate{
				ConversationCheckpointUpdate: &agentv1.ConversationStateStructure{
					Mode:                   agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
					RootPromptMessagesJson: [][]byte{sysID, infoID},
				},
			},
		})

		// 2. Assistant streams reply and emits TurnEnded BEFORE flushing final blobs/checkpoint.
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TextDelta{
						TextDelta: &agentv1.TextDeltaUpdate{Text: "bananana bananana bananana bananana bananana"},
					},
				},
			},
		})
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TurnEnded{
						TurnEnded: &agentv1.TurnEndedUpdate{InputTokens: proto.Int64(1200), OutputTokens: proto.Int64(25)},
					},
				},
			},
		})

		// 3. Post-TurnEnded (~40ms later): server writes user/assistant blobs, waits for ack, then emits terminal checkpoint.
		time.Sleep(40 * time.Millisecond)
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_KvServerMessage{
				KvServerMessage: &agentv1.KvServerMessage{
					Id: 3,
					Message: &agentv1.KvServerMessage_SetBlobArgs{SetBlobArgs: &agentv1.SetBlobArgs{
						BlobId:   userID,
						BlobData: []byte(`{"role":"user","content":"say bananana 5 times"}`),
					}},
				},
			},
		})
		<-kvAcked
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_KvServerMessage{
				KvServerMessage: &agentv1.KvServerMessage{
					Id: 4,
					Message: &agentv1.KvServerMessage_SetBlobArgs{SetBlobArgs: &agentv1.SetBlobArgs{
						BlobId:   asstID,
						BlobData: []byte(`{"role":"assistant","content":"bananana bananana bananana bananana bananana"}`),
					}},
				},
			},
		})
		<-kvAcked
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_ConversationCheckpointUpdate{
				ConversationCheckpointUpdate: &agentv1.ConversationStateStructure{
					Mode:                   agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
					RootPromptMessagesJson: [][]byte{sysID, infoID, userID, asstID},
					Turns:                  [][]byte{turnID},
				},
			},
		})
		// End-of-stream Connect trailer frame.
		_, _ = writer.Write([]byte{0x02, 0, 0, 0, 2, '{', '}'})
	}()

	var gotText string
	var finished bool
	err := readCursorAccountStream(context.Background(), appendFn, "req", conversationID, "grok-4.7", "cursor_account1", 0, reader, nil, nil, nil, nil, func(event ModelEvent) error {
		if event.Kind == ModelEventKindTextDelta {
			gotText += event.Text
		}
		if event.Kind == ModelEventKindTurnFinished {
			finished = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("readCursorAccountStream error = %v", err)
	}
	if !finished || !strings.Contains(gotText, "bananana") {
		t.Fatalf("finished=%v text=%q", finished, gotText)
	}

	saved := cursorAccountLoadState(conversationID)
	if saved == nil || len(saved.GetTurns()) != 1 || len(saved.GetRootPromptMessagesJson()) != 4 {
		t.Fatalf("saved checkpoint lost post-turnEnded state: %+v", saved)
	}

	// Follow-up turn must use the saved complete checkpoint without duplicating ConversationHistory.
	follow := cursorAccountRunMessage(StreamRequest{
		ConversationID: conversationID,
		Messages: []Message{
			{Role: "user", Content: "say bananana 5 times"},
			{Role: "assistant", Content: "bananana bananana bananana bananana bananana"},
			{Role: "user", Content: "what did u just say?"},
		},
	}, "grok-4.7")
	if len(follow.GetRunRequest().GetConversationState().GetTurns()) != 1 {
		t.Fatalf("follow-up did not carry saved turn checkpoint: %+v", follow.GetRunRequest().GetConversationState())
	}
	if follow.GetRunRequest().GetAction().GetUserMessageAction().GetConversationHistory() != nil {
		t.Fatal("follow-up duplicated ConversationHistory despite up-to-date checkpoint")
	}
}

func TestCursorAccountBootstrapStubOrBehindCheckpointFallsBackToHistory(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	const convBootstrap = "conv-bootstrap-stub"
	sysID := bytes.Repeat([]byte{0xaa}, 32)
	infoID := bytes.Repeat([]byte{0xbb}, 32)
	cursorAccountRememberBlob(sysID, []byte(`{"role":"system","content":"system prompt"}`))
	cursorAccountRememberBlob(infoID, []byte(`{"role":"user","content":"<user_info>\nOS: win32\n</user_info>"}`))

	// Simulate a legacy/aborted turns=0 bootstrap file already on disk.
	rawStub, _ := proto.Marshal(&agentv1.ConversationStateStructure{
		Mode:                   agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		RootPromptMessagesJson: [][]byte{sysID, infoID},
	})
	stubPath := cursorAccountStatePath(convBootstrap)
	_ = os.MkdirAll(filepath.Dir(stubPath), 0o755)
	_ = os.WriteFile(stubPath, rawStub, 0o600)

	if HasCursorAccountState(convBootstrap) {
		t.Fatal("header-only turns=0 bootstrap stub must not report HasCursorAccountState=true")
	}

	follow := cursorAccountRunMessage(StreamRequest{
		ConversationID: convBootstrap,
		Messages: []Message{
			{Role: "user", Content: "say bananana 5 times"},
			{Role: "assistant", Content: "bananana bananana bananana bananana bananana"},
			{Role: "user", Content: "what did u just say?"},
		},
	}, "grok-4.7")
	if follow.GetRunRequest().GetAction().GetUserMessageAction().GetConversationHistory() != nil {
		t.Fatal("must not populate ignored UserMessageAction.ConversationHistory")
	}
	roots := follow.GetRunRequest().GetConversationState().GetRootPromptMessagesJson()
	if len(roots) != 3 {
		t.Fatalf("expected synthesized RootPromptMessagesJson with 3 blobs (user_info placeholder + 2 history messages), got %d", len(roots))
	}
	uiBlob, ok := cursorAccountBlob(roots[0])
	if !ok || !strings.Contains(string(uiBlob), "<user_info>") || !strings.Contains(string(uiBlob), `"requestContextCompleteness"`) {
		t.Fatalf("root[0] is not a user_info recovery placeholder: ok=%v data=%q", ok, string(uiBlob))
	}
	historyPlain := cursorAccountHistoryPlain(follow)
	if !strings.Contains(historyPlain, "say bananana 5 times") || !strings.Contains(historyPlain, "bananana bananana") {
		t.Fatalf("synthesized root prompt blobs lost history: %q", historyPlain)
	}

	// Also verify an in-memory completed checkpoint is never clobbered by a new turn's pre-turn bootstrap stub.
	const convProtected = "conv-protected-from-stub"
	cursorAccountRememberState(convProtected, &agentv1.ConversationStateStructure{
		Mode:  agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		Turns: [][]byte{[]byte("completed-turn-1")},
	})
	cursorAccountRememberState(convProtected, &agentv1.ConversationStateStructure{
		Mode:                   agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		RootPromptMessagesJson: [][]byte{sysID, infoID},
	})
	if loaded := cursorAccountLoadState(convProtected); loaded == nil || len(loaded.GetTurns()) != 1 {
		t.Fatalf("completed checkpoint was overwritten by pre-turn bootstrap stub: %+v", loaded)
	}
}

func TestCursorAccountStripsEndOfImageAndRetriesWhenEmpty(t *testing.T) {
	homeDir, err := os.MkdirTemp("", "cursor-byok-home-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	t.Setenv("CURSOR_BYOK_HOME", homeDir)
	var attempts int
	appended := make(chan int64, 8)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cursorAccountRunPath {
			http.NotFound(w, r)
			return
		}
		attempts++
		attempt := attempts
		w.Header().Set("content-type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		if !waitCursorAppend(appended, 0) {
			return
		}
		if attempt == 1 {
			writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
				Message: &agentv1.AgentServerMessage_InteractionUpdate{
					InteractionUpdate: &agentv1.InteractionUpdate{
						Message: &agentv1.InteractionUpdate_TextDelta{
							TextDelta: &agentv1.TextDeltaUpdate{Text: "<end_of_image>"},
						},
					},
				},
			})
			writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
				Message: &agentv1.AgentServerMessage_InteractionUpdate{
					InteractionUpdate: &agentv1.InteractionUpdate{
						Message: &agentv1.InteractionUpdate_TurnEnded{
							TurnEnded: &agentv1.TurnEndedUpdate{InputTokens: proto.Int64(790734), OutputTokens: proto.Int64(1)},
						},
					},
				},
			})
			_, _ = w.Write([]byte{0x02, 0, 0, 0, 2, '{', '}'})
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TextDelta{
						TextDelta: &agentv1.TextDeltaUpdate{Text: "Recovered answer after <end_of_image>"},
					},
				},
			},
		})
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TurnEnded{
						TurnEnded: &agentv1.TurnEndedUpdate{InputTokens: proto.Int64(1000), OutputTokens: proto.Int64(10)},
					},
				},
			},
		})
		_, _ = w.Write([]byte{0x02, 0, 0, 0, 2, '{', '}'})
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer agent.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cursorAccountServerConfigPath:
			raw, _ := proto.Marshal(&aiserverv1.GetServerConfigResponse{
				AgentUrlConfig: &aiserverv1.AgentUrlConfig{AgentnUrl: agent.URL},
			})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		case cursorAccountBidiAppendPath:
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			msg := &aiserverv1.BidiAppendRequest{}
			if err := proto.Unmarshal(body, msg); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			appended <- msg.GetAppendSeqno()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	previous := cursorAccountBackendRoot
	cursorAccountBackendRoot = api.URL
	t.Cleanup(func() { cursorAccountBackendRoot = previous })

	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var gotText string
	var turnFinishedCount int
	err = gateway.Stream(ctx, StreamRequest{
		ModelID:        "gemini-4-carbon",
		GroupName:      "cursor_account1",
		ConversationID: "conv-end-of-image-retry",
		Messages:       []Message{{Role: "user", Content: "investigate subagent continue"}},
	}, func(event ModelEvent) error {
		if event.Kind == ModelEventKindTextDelta {
			gotText += event.Text
		}
		if event.Kind == ModelEventKindTurnFinished {
			turnFinishedCount++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (1st emitted <end_of_image>, 2nd recovered)", attempts)
	}
	if strings.Contains(gotText, "<end_of_image>") || gotText != "Recovered answer after " {
		t.Fatalf("gotText = %q, want '<end_of_image>' stripped and recovered", gotText)
	}
	if turnFinishedCount != 1 {
		t.Fatalf("turnFinishedCount = %d, want 1", turnFinishedCount)
	}
}

func TestCursorAccountPostToolEndOfImageTriggersFollowUpContinuation(t *testing.T) {
	results := make(chan *agentv1.ExecClientMessage, 1)
	events := runCursorAccountFixture(t, results, func(w http.ResponseWriter, flusher http.Flusher, appended <-chan int64) {
		if !waitCursorAppend(appended, 0) {
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_ExecServerMessage{
				ExecServerMessage: &agentv1.ExecServerMessage{
					Id:     9,
					ExecId: "read-1",
					Message: &agentv1.ExecServerMessage_ReadArgs{
						ReadArgs: &agentv1.ReadArgs{Path: "notes/todo.md", ToolCallId: "read-1"},
					},
				},
			},
		})
		flusher.Flush()
		if !waitCursorAppend(appended, 1) {
			return
		}
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: "<end_of_image>"}},
				},
			},
		})
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{}},
				},
			},
		})
		_, _ = w.Write([]byte{0x02, 0, 0, 0, 2, '{', '}'})
		flusher.Flush()
	})
	var text, finish string
	for _, event := range events {
		if event.Kind == ModelEventKindTextDelta {
			text += event.Text
		}
		if event.Kind == ModelEventKindTurnFinished {
			finish = event.FinishReason
		}
	}
	if text != "" {
		t.Fatalf("text = %q, want empty (<end_of_image> stripped)", text)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls so forwarder continues after post-tool <end_of_image>", finish)
	}
}

func TestCursorAccountSanitizeStateDropsEndOfImageAssistantBlob(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	sysID := bytes.Repeat([]byte{0x01}, 32)
	userID := bytes.Repeat([]byte{0x02}, 32)
	eoiID := bytes.Repeat([]byte{0x03}, 32)
	cursorAccountRememberBlob(sysID, []byte(`{"role":"system","content":"You are an assistant."}`))
	cursorAccountRememberBlob(userID, []byte(`{"role":"user","content":[{"type":"text","text":"hello"}]}`))
	cursorAccountRememberBlob(eoiID, []byte(`{"role":"assistant","content":[{"type":"text","text":"<end_of_image>"}],"id":"1"}`))

	state := &agentv1.ConversationStateStructure{
		Mode:                   agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		RootPromptMessagesJson: [][]byte{sysID, userID, eoiID},
	}
	if !sanitizeCursorAccountState(state) {
		t.Fatal("expected sanitizeCursorAccountState to strip <end_of_image> assistant blob")
	}
	if got := len(state.GetRootPromptMessagesJson()); got != 2 {
		t.Fatalf("roots len = %d, want 2 (sys + user)", got)
	}
}

func TestCursorAccountSplitTurnIncludesBackgroundDelegationNoticeInNewTurn(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	const convID = "conv-bg-delegation"
	cursorAccountRememberState(convID, &agentv1.ConversationStateStructure{
		Mode:  agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		Turns: [][]byte{[]byte("turn-1")},
	})
	bgNotice := "<system_reminder>\n此前转入后台继续运行的委派任务已结束（状态：completed）。任务：inspect routes。（subagent agent_id=child-agent-123，如需继续该子代理可调用 Task 并传 resume=\"child-agent-123\"）\n结果：\nAll 3 routes verified.\n</system_reminder>"
	run := cursorAccountRunMessage(StreamRequest{
		ConversationID: convID,
		Messages: []Message{
			{Role: "user", Content: "spawn subagent"},
			{Role: "assistant", Content: "spawned"},
			{Role: "user", Content: bgNotice},
			{Role: "user", Content: "continue"},
		},
	}, "gemini-4-argon")
	text := run.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if !strings.Contains(text, "child-agent-123") || !strings.Contains(text, "All 3 routes verified.") || !strings.Contains(text, "continue") {
		t.Fatalf("expected background subagent completion and agent_id in user message text, got %q", text)
	}
}

func TestCursorAccountTurnSnapshotAndRestoreAcrossModelSwitchRetry(t *testing.T) {
	homeDir, err := os.MkdirTemp("", "cursor-byok-home-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	t.Setenv("CURSOR_BYOK_HOME", homeDir)

	const convID = "conv-turn-snapshot-restore"
	t.Cleanup(func() { ClearCursorAccountStateAndSnapshots(convID) })

	// Turn 1 completed with a 434k-token checkpoint.
	turn1State := &agentv1.ConversationStateStructure{
		Mode:  agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		Turns: [][]byte{bytes.Repeat([]byte{0x11}, 32)},
		TokenDetails: &agentv1.ConversationTokenDetails{
			UsedTokens: 434_532,
			MaxTokens:  1_000_000,
		},
	}
	cursorAccountRememberState(convID, turn1State)

	// Turn 2 starts: snapshot pre-turn-2 checkpoint.
	SnapshotCursorAccountStateBeforeTurn(convID, 2)

	// Turn 2 streams a partial/dirty checkpoint before failing on Gemini 4.
	dirtyTurn2State := &agentv1.ConversationStateStructure{
		Mode:  agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		Turns: [][]byte{bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x22}, 32)},
		TokenDetails: &agentv1.ConversationTokenDetails{
			UsedTokens: 435_000,
			MaxTokens:  1_000_000,
		},
	}
	cursorAccountRememberState(convID, dirtyTurn2State)
	// Repeated Snapshot before turn 2 must NOT clobber the original pre-turn-2 snapshot.
	SnapshotCursorAccountStateBeforeTurn(convID, 2)

	// User switches to Grok 4.7 and retries Turn 2 (rewind target_turn=2).
	if !RestoreCursorAccountStateBeforeTurn(convID, 2) {
		t.Fatal("expected RestoreCursorAccountStateBeforeTurn(2) to restore Turn 1 checkpoint")
	}
	if got := CursorAccountUsedTokens(convID); got != 434_532 {
		t.Fatalf("restored UsedTokens = %d, want 434532", got)
	}
	if got := len(cursorAccountLoadState(convID).GetTurns()); got != 1 {
		t.Fatalf("restored turns = %d, want 1", got)
	}

	// Simulate Grok 4.7 aborted after mutating state, then user switches to Claude Opus 5.5 and retries Turn 2 again.
	cursorAccountRememberState(convID, &agentv1.ConversationStateStructure{
		Mode:  agentv1.AgentMode_AGENT_MODE_AGENT.Enum(),
		Turns: [][]byte{bytes.Repeat([]byte{0x99}, 32)},
		TokenDetails: &agentv1.ConversationTokenDetails{
			UsedTokens: 22_400,
			MaxTokens:  1_000_000,
		},
	})
	if !RestoreCursorAccountStateBeforeTurn(convID, 2) {
		t.Fatal("expected second RestoreCursorAccountStateBeforeTurn(2) to still restore pre-turn-2 checkpoint")
	}
	if got := CursorAccountUsedTokens(convID); got != 434_532 {
		t.Fatalf("second restored UsedTokens = %d, want 434532", got)
	}

	// Verify disk persistence across in-memory eviction.
	cursorAgentMemory.mu.Lock()
	delete(cursorAgentMemory.states, convID)
	delete(cursorAgentMemory.turnStates, convID)
	cursorAgentMemory.mu.Unlock()
	if !RestoreCursorAccountStateBeforeTurn(convID, 2) {
		t.Fatal("expected disk-persisted pre-turn-2 snapshot to restore after memory eviction")
	}
	if got := CursorAccountUsedTokens(convID); got != 434_532 {
		t.Fatalf("disk-restored UsedTokens = %d, want 434532", got)
	}

	// Rewinding to Turn 1 (no prior snapshot) clears active state and discards future turn snapshots.
	if RestoreCursorAccountStateBeforeTurn(convID, 1) {
		t.Fatal("RestoreCursorAccountStateBeforeTurn(1) should return false when no pre-turn-1 snapshot exists")
	}
	if HasCursorAccountState(convID) {
		t.Fatal("active checkpoint should be cleared when rewind target has no snapshot")
	}
	if RestoreCursorAccountStateBeforeTurn(convID, 2) {
		t.Fatal("future snapshot before-turn-2 should have been discarded when rewinding to Turn 1")
	}
}

func TestCursorAccountSynthesizesRootPromptBlobsWithToolCallsAndSummary(t *testing.T) {
	homeDir, err := os.MkdirTemp("", "cursor-byok-home-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeDir) })
	t.Setenv("CURSOR_BYOK_HOME", homeDir)

	const convID = "conv-synth-tools-summary"
	summary := "<conversation_summary>\n" + strings.Repeat("Important prior project context. ", 900) + "\n</conversation_summary>"
	run := cursorAccountRunMessageFor(StreamRequest{
		ConversationID:              convID,
		ResolvedContextWindowTokens: 4_000, // budget = 6_000 bytes (smaller than summary alone -> test larger below)
		Messages: []Message{
			{Role: "system", Content: "local system prompt"},
			{Role: "user", Content: "<user_info>\nOS: win32\n</user_info>"},
			{Role: "user", Content: summary},
			{Role: "user", Content: strings.Repeat("old filler turn ", 200)},
			{Role: "assistant", Content: "old filler reply"},
			// Adjacent parallel tool calls (as emitted by projector.go) + out-of-order results
			{Role: "assistant", Content: "Checking both files.", ToolCalls: []ToolCallDescriptor{{
				ID: "call-1", Type: "function", Function: ToolCallFunctionShape{Name: "Read", Arguments: `{"path":"/root/a.js"}`},
			}}},
			{Role: "assistant", ToolCalls: []ToolCallDescriptor{{
				ID: "call-2", Type: "function", Function: ToolCallFunctionShape{Name: "Read", Arguments: `{"path":"/root/b.js"}`},
			}}},
			{Role: "tool", Name: "Read", ToolCallID: "call-2", Content: "contents of b"},
			{Role: "tool", Name: "Read", ToolCallID: "call-1", Content: "contents of a"},
			// Interrupted dangling tool call without a result must be dropped cleanly
			{Role: "assistant", ToolCalls: []ToolCallDescriptor{{
				ID: "call-dangling", Type: "function", Function: ToolCallFunctionShape{Name: "Shell", Arguments: `{"command":"sleep 99"}`},
			}}},
			{Role: "user", Content: "continue"},
		},
	}, "grok-4.7", convID, len(summary)+2500)

	if run.GetRunRequest().GetAction().GetUserMessageAction().GetConversationHistory() != nil {
		t.Fatal("must not populate ignored UserMessageAction.ConversationHistory")
	}
	roots := run.GetRunRequest().GetConversationState().GetRootPromptMessagesJson()
	if len(roots) < 5 {
		t.Fatalf("expected user_info placeholder + summary + merged assistant + 2 tool results, got %d roots", len(roots))
	}
	// Verify every synthesized blob is retrievable via KV GetBlob.
	var decoded []string
	for idx, id := range roots {
		reply := cursorAccountKVReply(&agentv1.KvServerMessage{
			Id:      uint32(idx + 1),
			Message: &agentv1.KvServerMessage_GetBlobArgs{GetBlobArgs: &agentv1.GetBlobArgs{BlobId: id}},
		})
		blob := reply.GetKvClientMessage().GetGetBlobResult().GetBlobData()
		if len(blob) == 0 {
			t.Fatalf("root[%d] missing from KV blob store", idx)
		}
		decoded = append(decoded, string(blob))
	}
	if !strings.Contains(decoded[0], "<user_info>") {
		t.Fatalf("root[0] = %q, want user_info recovery placeholder", decoded[0])
	}
	if !strings.Contains(decoded[1], "<conversation_summary>") {
		t.Fatalf("root[1] = %q, want preserved compaction summary", decoded[1])
	}
	joined := strings.Join(decoded[1:], "\n")
	if strings.Contains(joined, "old filler turn") {
		t.Fatal("expected old filler turn to be trimmed to fit budget while keeping summary and newest tool turn")
	}
	if strings.Contains(joined, "call-dangling") {
		t.Fatal("dangling unanswered tool call should have been stripped")
	}
	// Merged assistant message should contain both call-1 and call-2 with parsed JSON object args.
	foundMerged := false
	for _, item := range decoded[2:] {
		if strings.Contains(item, `"type":"tool-call"`) &&
			strings.Contains(item, `"toolCallId":"call-1"`) &&
			strings.Contains(item, `"toolCallId":"call-2"`) &&
			strings.Contains(item, `"args":{"path":"/root/a.js"}`) {
			foundMerged = true
			break
		}
	}
	if !foundMerged {
		t.Fatalf("merged assistant tool-call blob not found in decoded blobs: %s", joined)
	}
	if !strings.Contains(joined, `"type":"tool-result"`) || !strings.Contains(joined, "contents of a") || !strings.Contains(joined, "contents of b") {
		t.Fatalf("tool results missing from synthesized blobs: %s", joined)
	}
}

func TestReadCursorAccountStreamResetsSilentTimeoutOnKVActivity(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	go func() {
		defer writer.Close()
		// Emit 4 KV GetBlob frames spaced 50ms apart with a 90ms silentTimeout:
		// total 200ms > 90ms, which must NOT trigger a false silent timeout because KV frames are active.
		for i := uint32(1); i <= 4; i++ {
			time.Sleep(50 * time.Millisecond)
			writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
				Message: &agentv1.AgentServerMessage_KvServerMessage{
					KvServerMessage: &agentv1.KvServerMessage{
						Id:      i,
						Message: &agentv1.KvServerMessage_GetBlobArgs{GetBlobArgs: &agentv1.GetBlobArgs{BlobId: bytes.Repeat([]byte{byte(i)}, 32)}},
					},
				},
			})
		}
		time.Sleep(30 * time.Millisecond)
		writeCursorAgentText(writer, "restored ok")
		writeCursorAgentTurnEnded(writer)
	}()
	var got string
	err := readCursorAccountStream(
		context.Background(),
		func(context.Context, int64, []byte) error { return nil },
		"req-kv-silent",
		"conv-kv-silent",
		"grok-4.7",
		"cursor_account1",
		90*time.Millisecond,
		reader,
		nil,
		nil,
		nil,
		nil,
		func(event ModelEvent) error {
			if event.Kind == ModelEventKindTextDelta {
				got += event.Text
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("readCursorAccountStream failed despite active KV frames: %v", err)
	}
	if got != "restored ok" {
		t.Fatalf("got = %q, want %q", got, "restored ok")
	}
}



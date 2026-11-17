package modeladapter

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestReadCursorAccountStreamReportsMissingAccessWhenSilent(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := readCursorAccountStream(ctx, "Bearer test", "req", "claude-fable-5", "cursor_account1", 40*time.Millisecond, reader, nil, func(ModelEvent) error {
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
	events := runCursorAccountFixture(t, func(w http.ResponseWriter, flusher http.Flusher, appended <-chan int64) {
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

func TestCursorAccountGatewayReturnsToolCall(t *testing.T) {
	events := runCursorAccountFixture(t, func(w http.ResponseWriter, flusher http.Flusher, appended <-chan int64) {
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

func runCursorAccountFixture(t *testing.T, play func(http.ResponseWriter, http.Flusher, <-chan int64)) []ModelEvent {
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
		ModelID:         "claude-4.5-opus-high",
		GroupName:       "cursor_account2",
		CursorAccountID: "account-2",
		ConversationID:  "11111111-2222-3333-4444-555555555555",
		Messages:        []Message{{Role: "user", Content: "hi"}},
	}, func(event ModelEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func waitCursorAppend(appended <-chan int64, want int64) bool {
	select {
	case seq := <-appended:
		return seq == want
	case <-time.After(4 * time.Second):
		return false
	}
}

func writeCursorAgentFrame(w http.ResponseWriter, msg proto.Message) {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return
	}
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	_, _ = w.Write(append(header, payload...))
}

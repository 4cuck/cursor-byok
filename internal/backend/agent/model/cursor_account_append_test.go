package modeladapter

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/gen/aiserverv1"

	"google.golang.org/protobuf/proto"
)

// renewingDirectory is an account manager whose token can rotate and whose
// API key can mint a replacement.
type renewingDirectory struct {
	mu        sync.Mutex
	token     string
	refreshed string
	apiKey    bool
	refreshes int
}

func (dir *renewingDirectory) Authorization(context.Context) (string, error) {
	dir.mu.Lock()
	defer dir.mu.Unlock()
	return dir.token, nil
}

func (dir *renewingDirectory) AccountIsLocked(string) bool { return false }
func (dir *renewingDirectory) CurrentAccountID() string    { return "acct" }
func (dir *renewingDirectory) UsesAPIKey(string) bool      { return dir.apiKey }
func (dir *renewingDirectory) MarkDeviceLocked(string) error {
	return nil
}

func (dir *renewingDirectory) ForceRefresh(context.Context, string) (string, error) {
	dir.mu.Lock()
	defer dir.mu.Unlock()
	dir.refreshes++
	dir.token = dir.refreshed
	return dir.refreshed, nil
}

func (dir *renewingDirectory) setToken(token string) {
	dir.mu.Lock()
	defer dir.mu.Unlock()
	dir.token = token
}

func (dir *renewingDirectory) refreshCount() int {
	dir.mu.Lock()
	defer dir.mu.Unlock()
	return dir.refreshes
}

type cursorAppendRecord struct {
	authorization string
	requestID     string
	seq           int64
}

type cursorAppendServer struct {
	mu      sync.Mutex
	records []cursorAppendRecord
}

func (server *cursorAppendServer) all() []cursorAppendRecord {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]cursorAppendRecord(nil), server.records...)
}

func startCursorAppendServer(t *testing.T, respond func(record cursorAppendRecord, attempt int) (int, string)) *cursorAppendServer {
	t.Helper()
	server := &cursorAppendServer{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := &aiserverv1.BidiAppendRequest{}
		if r.URL.Path != cursorAccountBidiAppendPath || proto.Unmarshal(body, request) != nil {
			http.NotFound(w, r)
			return
		}
		record := cursorAppendRecord{
			authorization: r.Header.Get("authorization"),
			requestID:     request.GetRequestId().GetRequestId(),
			seq:           request.GetAppendSeqno(),
		}
		server.mu.Lock()
		server.records = append(server.records, record)
		attempt := len(server.records)
		server.mu.Unlock()
		if status, text := respond(record, attempt); status != http.StatusOK {
			http.Error(w, text, status)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(api.Close)
	previous := cursorAccountBackendRoot
	cursorAccountBackendRoot = api.URL
	t.Cleanup(func() { cursorAccountBackendRoot = previous })
	return server
}

func fastCursorAppendBackoff(t *testing.T) *[]int {
	t.Helper()
	var failures []int
	previous := cursorAccountAppendBackoff
	cursorAccountAppendBackoff = func(failure int) time.Duration {
		failures = append(failures, failure)
		return time.Millisecond
	}
	t.Cleanup(func() { cursorAccountAppendBackoff = previous })
	return &failures
}

func TestCursorAccountAuthRejectedMatchesExpiredAppend(t *testing.T) {
	expired := &cursorAccountAppendError{
		status: http.StatusUnauthorized,
		err:    errors.New("cursor append returned HTTP 401: If you are logged in, try logging out and back in."),
	}
	if !cursorAccountAuthRejected(expired) {
		t.Fatal("expired append token was not treated as a sign-in failure")
	}
	if !cursorAccountAuthRejected(fmt.Errorf("cursor refused the model call: %w", expired)) {
		t.Fatal("wrapped expired append token was not treated as a sign-in failure")
	}
	if cursorAccountAuthRejected(errors.New("cursor append returned HTTP 503: upstream unavailable")) {
		t.Fatal("a 503 was treated as a sign-in failure")
	}
}

func TestCursorAccountAppenderRenewsRejectedToken(t *testing.T) {
	server := startCursorAppendServer(t, func(record cursorAppendRecord, _ int) (int, string) {
		if record.authorization != "Bearer fresh" {
			return http.StatusUnauthorized, "If you are logged in, try logging out and back in."
		}
		return http.StatusOK, ""
	})
	dir := &renewingDirectory{token: "Bearer stale", refreshed: "Bearer fresh", apiKey: true}
	appender := newCursorAccountGateway(dir).newStreamAppender("acct", "req-renew", "Bearer stale")
	for _, seq := range []int64{7, 8} {
		if err := appender.append(context.Background(), seq, []byte("frame")); err != nil {
			t.Fatalf("append seq=%d: %v", seq, err)
		}
	}
	want := []cursorAppendRecord{
		{authorization: "Bearer stale", requestID: "req-renew", seq: 7},
		{authorization: "Bearer fresh", requestID: "req-renew", seq: 7},
		{authorization: "Bearer fresh", requestID: "req-renew", seq: 8},
	}
	if got := server.all(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("appends = %+v, want %+v", got, want)
	}
	if got := dir.refreshCount(); got != 1 {
		t.Fatalf("refreshes = %d, want 1", got)
	}
}

func TestCursorAccountAppenderDoesNotExchangeSessionTokens(t *testing.T) {
	server := startCursorAppendServer(t, func(cursorAppendRecord, int) (int, string) {
		return http.StatusUnauthorized, "If you are logged in, try logging out and back in."
	})
	dir := &renewingDirectory{token: "Bearer session", refreshed: "Bearer minted"}
	appender := newCursorAccountGateway(dir).newStreamAppender("acct", "req-session", "Bearer session")
	err := appender.append(context.Background(), 1, []byte("frame"))
	if !cursorAccountAuthRejected(err) {
		t.Fatalf("err = %v, want the sign-in failure", err)
	}
	if got := dir.refreshCount(); got != 0 {
		t.Fatalf("refreshes = %d, want 0 for a session account", got)
	}
	if got := len(server.all()); got != 1 {
		t.Fatalf("appends = %d, want 1", got)
	}
}

func TestCursorAccountAppenderPicksUpTokenRenewedBetweenFrames(t *testing.T) {
	previous := cursorAccountAuthRecheck
	cursorAccountAuthRecheck = 0
	t.Cleanup(func() { cursorAccountAuthRecheck = previous })
	server := startCursorAppendServer(t, func(cursorAppendRecord, int) (int, string) { return http.StatusOK, "" })
	dir := &renewingDirectory{token: "Bearer one", apiKey: true}
	appender := newCursorAccountGateway(dir).newStreamAppender("acct", "req-rotate", "Bearer one")
	if err := appender.append(context.Background(), 1, []byte("frame")); err != nil {
		t.Fatal(err)
	}
	dir.setToken("Bearer two")
	if err := appender.append(context.Background(), 2, []byte("frame")); err != nil {
		t.Fatal(err)
	}
	records := server.all()
	if len(records) != 2 || records[0].authorization != "Bearer one" || records[1].authorization != "Bearer two" {
		t.Fatalf("appends = %+v", records)
	}
	if got := dir.refreshCount(); got != 0 {
		t.Fatalf("refreshes = %d, want 0", got)
	}
}

func TestCursorAccountAppenderRetriesLikeTheIDE(t *testing.T) {
	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})

	t.Run("transient failures keep the seqno", func(t *testing.T) {
		failures := fastCursorAppendBackoff(t)
		server := startCursorAppendServer(t, func(_ cursorAppendRecord, attempt int) (int, string) {
			if attempt <= 2 {
				return http.StatusServiceUnavailable, "upstream unavailable"
			}
			return http.StatusOK, ""
		})
		if err := gateway.newStreamAppender("", "req-retry", "Bearer test").append(context.Background(), 3, []byte("frame")); err != nil {
			t.Fatal(err)
		}
		records := server.all()
		if len(records) != 3 {
			t.Fatalf("appends = %+v, want 3", records)
		}
		for _, record := range records {
			if record.seq != 3 || record.requestID != "req-retry" {
				t.Fatalf("retry changed the frame identity: %+v", records)
			}
		}
		if fmt.Sprint(*failures) != "[1 2]" {
			t.Fatalf("backoff failures = %v, want [1 2]", *failures)
		}
	})

	t.Run("gives up after five attempts", func(t *testing.T) {
		fastCursorAppendBackoff(t)
		server := startCursorAppendServer(t, func(cursorAppendRecord, int) (int, string) {
			return http.StatusBadGateway, "bad gateway"
		})
		err := gateway.newStreamAppender("", "req-down", "Bearer test").append(context.Background(), 1, []byte("frame"))
		if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
			t.Fatalf("err = %v", err)
		}
		if got := len(server.all()); got != cursorAccountAppendAttempts {
			t.Fatalf("appends = %d, want %d", got, cursorAccountAppendAttempts)
		}
	})

	t.Run("server errors are not retried", func(t *testing.T) {
		fastCursorAppendBackoff(t)
		server := startCursorAppendServer(t, func(cursorAppendRecord, int) (int, string) {
			return http.StatusInternalServerError, "boom"
		})
		if err := gateway.newStreamAppender("", "req-500", "Bearer test").append(context.Background(), 1, []byte("frame")); err == nil {
			t.Fatal("expected the 500 to fail the append")
		}
		if got := len(server.all()); got != 1 {
			t.Fatalf("appends = %d, want 1", got)
		}
	})
}

func TestCursorAccountShouldResetSavedState(t *testing.T) {
	refused := func(text string) error { return fmt.Errorf("cursor refused the model call: %s", text) }
	appendStatus := func(status int) error {
		return fmt.Errorf("cursor refused the model call: %w", &cursorAccountAppendError{
			status: status,
			err:    fmt.Errorf("cursor append returned HTTP %d: refused", status),
		})
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "canceled", err: fmt.Errorf("stream: %w", context.Canceled), want: false},
		{name: "server refused the state", err: refused(`{"error":{"code":"invalid_argument","message":"conversation state could not be loaded"}}`), want: true},
		{name: "oversized frame", err: errors.New("cursor agent frame is too large"), want: true},
		{name: "append payload too large", err: appendStatus(http.StatusRequestEntityTooLarge), want: true},
		{name: "append unavailable", err: appendStatus(http.StatusServiceUnavailable), want: false},
		{name: "append transport failure", err: fmt.Errorf("cursor refused the model call: %w", &cursorAccountAppendError{err: errors.New("connection reset by peer")}), want: false},
		{name: "expired token", err: appendStatus(http.StatusUnauthorized), want: false},
		{name: "rate limit", err: refused("rate limit exceeded, try again later"), want: false},
		{name: "plan access", err: errors.New("this account does not have access to claude-fable-5-2"), want: false},
		{name: "server unavailable", err: refused(`{"error":{"code":"unavailable","message":"try again"}}`), want: false},
	}
	for _, tc := range cases {
		if got := cursorAccountShouldResetSavedState(tc.err); got != tc.want {
			t.Errorf("%s: reset = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCursorAccountKVBlobWriteLeavesCheckpointRoots(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	const conversationID = "conv-kv-roots"
	system := []byte("kv-roots-system")
	user := []byte("kv-roots-user")
	cursorAccountRememberBlob(system, []byte(`{"role":"system","content":"agent prompt"}`))
	cursorAccountRememberBlob(user, []byte(`{"role":"user","content":"first question"}`))
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	go func() {
		defer writer.Close()
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_ConversationCheckpointUpdate{
				ConversationCheckpointUpdate: &agentv1.ConversationStateStructure{RootPromptMessagesJson: [][]byte{system, user}},
			},
		})
		writeCursorAgentFrame(writer, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_KvServerMessage{
				KvServerMessage: &agentv1.KvServerMessage{
					Id: 9,
					Message: &agentv1.KvServerMessage_SetBlobArgs{SetBlobArgs: &agentv1.SetBlobArgs{
						BlobId:   []byte("kv-roots-late"),
						BlobData: []byte(`{"role":"user","content":"late message"}`),
					}},
				},
			},
		})
		writeCursorAgentTurnEnded(writer)
	}()
	noAppend := func(context.Context, int64, []byte) error { return nil }
	if err := readCursorAccountStream(context.Background(), noAppend, "req", conversationID, "claude-4.5-sonnet", "cursor_account1", 0, reader, nil, nil, nil, func(ModelEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	roots := cursorAccountLoadState(conversationID).GetRootPromptMessagesJson()
	if len(roots) != 2 || string(roots[0]) != string(system) || string(roots[1]) != string(user) {
		t.Fatalf("roots = %q, want only the checkpoint's own roots", roots)
	}
}

func TestSanitizeCursorAccountRootMessagesKeepsLatestSummary(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	blobs := []struct{ id, data string }{
		{"sanitize-system", `{"role":"system","content":"agent prompt"}`},
		{"sanitize-info", `{"role":"user","content":"<user_info>\nOS Version: windows\n</user_info>"}`},
		{"sanitize-old-question", `{"role":"user","content":"old question"}`},
		{"sanitize-old-answer", `{"role":"assistant","content":"old answer"}`},
		{"sanitize-empty-answer", `{"role":"assistant","content":""}`},
		{"sanitize-title-system", `{"role":"system","content":"Generate a concise task title"}`},
		{"sanitize-summary", `{"role":"user","content":"<summary>\nwork so far\n</summary>"}`},
		{"sanitize-new-question", `{"role":"user","content":"continue"}`},
	}
	ids := make([][]byte, 0, len(blobs))
	for _, blob := range blobs {
		cursorAccountRememberBlob([]byte(blob.id), []byte(blob.data))
		ids = append(ids, []byte(blob.id))
	}
	cleaned, changed := sanitizeCursorAccountRootMessages(ids)
	got := make([]string, 0, len(cleaned))
	for _, id := range cleaned {
		got = append(got, string(id))
	}
	want := "sanitize-system,sanitize-info,sanitize-summary,sanitize-new-question"
	if !changed || strings.Join(got, ",") != want {
		t.Fatalf("roots = %v (changed=%v), want %s", got, changed, want)
	}
}

func TestCursorAccountBoundHistoryKeepsNewestTurns(t *testing.T) {
	history := []*agentv1.ConversationHistoryMessage{
		cursorAccountHistoryMessage(Message{Role: "user", Content: strings.Repeat("o", 4000)}),
		cursorAccountHistoryMessage(Message{Role: "assistant", Content: "reading the file"}),
		cursorAccountHistoryMessage(Message{Role: "tool", Name: "Read", ToolCallID: "call-1", Content: "file body"}),
		cursorAccountHistoryMessage(Message{Role: "user", Content: "latest question"}),
	}
	if got := cursorAccountBoundHistory(history, 0); len(got) != len(history) {
		t.Fatalf("unbounded history kept %d of %d", len(got), len(history))
	}
	// The cut lands on the tool result; its call is gone, so it goes too.
	kept := cursorAccountBoundHistory(history, proto.Size(history[2])+proto.Size(history[3]))
	if len(kept) != 1 || kept[0].GetUser() == nil {
		t.Fatalf("kept = %v, want only the latest user message", kept)
	}
	for window, want := range map[int]int{
		1_000_000: (1_000_000 - 64_000) * 3,
		100_000:   50_000 * 3,
		0:         (200_000 - 64_000) * 3,
	} {
		if got := cursorAccountHistoryBudget(window); got != want {
			t.Errorf("budget(%d) = %d, want %d", window, got, want)
		}
	}
}

type cursorAccountTestStream struct {
	authorization string
	run           *agentv1.AgentClientMessage
}

// cursorAccountTestBackend serves RunSSE and BidiAppend. play runs once per
// stream after the run request (append seq 0) has arrived.
type cursorAccountTestBackend struct {
	mu      sync.Mutex
	frames  map[string]chan *agentv1.AgentClientMessage
	streams []cursorAccountTestStream
}

func (backend *cursorAccountTestBackend) channel(requestID string) chan *agentv1.AgentClientMessage {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	ch, ok := backend.frames[requestID]
	if !ok {
		ch = make(chan *agentv1.AgentClientMessage, 16)
		backend.frames[requestID] = ch
	}
	return ch
}

func (backend *cursorAccountTestBackend) recorded() []cursorAccountTestStream {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]cursorAccountTestStream(nil), backend.streams...)
}

func startCursorAccountTestBackend(t *testing.T, play func(stream int, w http.ResponseWriter, flusher http.Flusher)) *cursorAccountTestBackend {
	t.Helper()
	backend := &cursorAccountTestBackend{frames: map[string]chan *agentv1.AgentClientMessage{}}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		id := &aiserverv1.BidiRequestId{}
		if r.URL.Path != cursorAccountRunPath || len(body) < 5 || proto.Unmarshal(body[5:], id) != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		var run *agentv1.AgentClientMessage
		select {
		case run = <-backend.channel(id.GetRequestId()):
		case <-time.After(4 * time.Second):
			return
		}
		backend.mu.Lock()
		backend.streams = append(backend.streams, cursorAccountTestStream{authorization: r.Header.Get("authorization"), run: run})
		stream := len(backend.streams)
		backend.mu.Unlock()
		play(stream, w, flusher)
	}))
	t.Cleanup(agent.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cursorAccountServerConfigPath:
			raw, _ := proto.Marshal(&aiserverv1.GetServerConfigResponse{AgentUrlConfig: &aiserverv1.AgentUrlConfig{AgentnUrl: agent.URL}})
			_, _ = w.Write(raw)
		case cursorAccountBidiAppendPath:
			body, _ := io.ReadAll(r.Body)
			request := &aiserverv1.BidiAppendRequest{}
			if err := proto.Unmarshal(body, request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			raw, err := hex.DecodeString(request.GetData())
			message := &agentv1.AgentClientMessage{}
			if err != nil || proto.Unmarshal(raw, message) != nil {
				http.Error(w, "bad frame", http.StatusBadRequest)
				return
			}
			backend.channel(request.GetRequestId().GetRequestId()) <- message
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	previous := cursorAccountBackendRoot
	cursorAccountBackendRoot = api.URL
	t.Cleanup(func() { cursorAccountBackendRoot = previous })
	return backend
}

func streamCursorAccountText(gateway *cursorAccountGateway, req StreamRequest) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var text strings.Builder
	err := gateway.Stream(ctx, req, func(event ModelEvent) error {
		if event.Kind == ModelEventKindTextDelta {
			text.WriteString(event.Text)
		}
		return nil
	})
	return text.String(), err
}

func writeCursorAgentText(w io.Writer, text string) {
	writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: text}},
			},
		},
	})
}

func writeCursorAgentTurnEnded(w io.Writer) {
	writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{}},
			},
		},
	})
}

func writeCursorAgentEnd(w io.Writer, payload string) {
	header := make([]byte, 5)
	header[0] = 0x02
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	_, _ = w.Write(append(header, payload...))
}

func TestCursorAccountGatewayResetsRefusedCheckpoint(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	const conversationID = "conv-refused-checkpoint"
	cursorAccountRememberState(conversationID, &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("stale-turn")}})
	backend := startCursorAccountTestBackend(t, func(stream int, w http.ResponseWriter, flusher http.Flusher) {
		if stream == 1 {
			writeCursorAgentEnd(w, `{"error":{"code":"invalid_argument","message":"conversation state could not be loaded"}}`)
			flusher.Flush()
			return
		}
		writeCursorAgentText(w, "recovered")
		writeCursorAgentTurnEnded(w)
		flusher.Flush()
	})
	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})
	text, err := streamCursorAccountText(gateway, StreamRequest{
		ModelID:                     "claude-4.5-sonnet",
		ConversationID:              conversationID,
		ResolvedContextWindowTokens: 2_000,
		Messages: []Message{
			{Role: "user", Content: strings.Repeat("old context ", 1000)},
			{Role: "assistant", Content: "old answer"},
			{Role: "user", Content: "recent question"},
			{Role: "assistant", Content: "recent answer"},
			{Role: "user", Content: "continue"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "recovered" {
		t.Fatalf("text = %q", text)
	}
	streams := backend.recorded()
	if len(streams) != 2 {
		t.Fatalf("streams = %d, want the refused attempt and one retry", len(streams))
	}
	first := streams[0].run.GetRunRequest()
	if len(first.GetConversationState().GetTurns()) != 1 || first.GetAction().GetUserMessageAction().GetConversationHistory() != nil {
		t.Fatal("first attempt did not send only the saved checkpoint")
	}
	if len(streams[1].run.GetRunRequest().GetConversationState().GetTurns()) != 0 {
		t.Fatal("retry sent the refused checkpoint again")
	}
	history := cursorAccountHistoryPlain(streams[1].run)
	if !strings.Contains(history, "recent question") || !strings.Contains(history, "recent answer") {
		t.Fatalf("retry history = %q", history)
	}
	if strings.Contains(history, "old context") {
		t.Fatal("retry history was not bounded to the context window")
	}
	if HasCursorAccountState(conversationID) {
		t.Fatal("refused checkpoint was kept")
	}
}

func TestCursorAccountSideCallKeepsConversationCheckpoint(t *testing.T) {
	t.Setenv("CURSOR_BYOK_HOME", t.TempDir())
	const conversationID = "conv-side-call"
	cursorAccountRememberState(conversationID, &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("main-turn")}})
	backend := startCursorAccountTestBackend(t, func(_ int, w http.ResponseWriter, flusher http.Flusher) {
		writeCursorAgentFrame(w, &agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_ConversationCheckpointUpdate{
				ConversationCheckpointUpdate: &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("summary-turn")}},
			},
		})
		writeCursorAgentText(w, "short summary")
		writeCursorAgentTurnEnded(w)
		flusher.Flush()
	})
	gateway := newCursorAccountGateway(staticCursorAuthorizer{token: "Bearer test"})
	text, err := streamCursorAccountText(gateway, StreamRequest{
		ModelID:        "claude-4.5-sonnet",
		ConversationID: conversationID,
		CompileSummary: "compaction trigger=auto source=agent turns=4 messages=12",
		Messages: []Message{
			{Role: "system", Content: "Summarize the conversation for a continuation."},
			{Role: "user", Content: "<user_info>\nOS Version: windows\n</user_info>\n\nTranscript to summarize.\nReturn only the replacement summary text."},
		},
	})
	if err != nil || text != "short summary" {
		t.Fatalf("text = %q err = %v", text, err)
	}
	run := backend.recorded()[0].run.GetRunRequest()
	if run.GetConversationId() == conversationID {
		t.Fatal("side call reused the conversation's upstream id")
	}
	if len(run.GetConversationState().GetTurns()) != 0 {
		t.Fatal("side call loaded the conversation checkpoint")
	}
	prompt := run.GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if !strings.HasPrefix(prompt, "<instructions>\nSummarize the conversation for a continuation.\n</instructions>\n\n") || !strings.Contains(prompt, "Transcript to summarize.") {
		t.Fatalf("prompt = %q", prompt)
	}
	state := cursorAccountLoadState(conversationID)
	if len(state.GetTurns()) != 1 || string(state.GetTurns()[0]) != "main-turn" {
		t.Fatalf("conversation checkpoint = %v", state)
	}
}

func TestCursorAccountGatewayReplaysOnlyBeforeVisibleOutput(t *testing.T) {
	t.Run("auth refusal before output is replayed with a renewed token", func(t *testing.T) {
		backend := startCursorAccountTestBackend(t, func(stream int, w http.ResponseWriter, flusher http.Flusher) {
			if stream == 1 {
				writeCursorAgentEnd(w, `{"error":"unauthenticated"}`)
			} else {
				writeCursorAgentText(w, "ok")
				writeCursorAgentTurnEnded(w)
			}
			flusher.Flush()
		})
		dir := &renewingDirectory{token: "Bearer stale", refreshed: "Bearer fresh", apiKey: true}
		text, err := streamCursorAccountText(newCursorAccountGateway(dir), StreamRequest{
			ModelID:  "claude-4.5-sonnet",
			Messages: []Message{{Role: "user", Content: "hi"}},
		})
		if err != nil || text != "ok" {
			t.Fatalf("text = %q err = %v", text, err)
		}
		streams := backend.recorded()
		if len(streams) != 2 || streams[1].authorization != "Bearer fresh" || dir.refreshCount() != 1 {
			t.Fatalf("streams = %d refreshes = %d", len(streams), dir.refreshCount())
		}
	})

	t.Run("auth refusal after output is returned", func(t *testing.T) {
		backend := startCursorAccountTestBackend(t, func(_ int, w http.ResponseWriter, flusher http.Flusher) {
			writeCursorAgentText(w, "partial")
			writeCursorAgentEnd(w, `{"error":"unauthenticated"}`)
			flusher.Flush()
		})
		dir := &renewingDirectory{token: "Bearer stale", refreshed: "Bearer fresh", apiKey: true}
		text, err := streamCursorAccountText(newCursorAccountGateway(dir), StreamRequest{
			ModelID:  "claude-4.5-sonnet",
			Messages: []Message{{Role: "user", Content: "hi"}},
		})
		if err == nil || !strings.Contains(err.Error(), "unauthenticated") {
			t.Fatalf("err = %v", err)
		}
		if text != "partial" || len(backend.recorded()) != 1 || dir.refreshCount() != 0 {
			t.Fatalf("text = %q streams = %d refreshes = %d", text, len(backend.recorded()), dir.refreshCount())
		}
	})
}

package modeladapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/internal/cursoraccount"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// liveDropTransport lets a test lose the RunSSE stream it is reading, the way
// the connection is lost when Cursor's server goes away: the next read fails.
type liveDropTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	bodies []*liveDropBody
}

func (t *liveDropTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || !strings.HasSuffix(req.URL.Path, cursorAccountRunPath) {
		return resp, err
	}
	body := &liveDropBody{ReadCloser: resp.Body, dropped: make(chan struct{})}
	resp.Body = body
	t.mu.Lock()
	t.bodies = append(t.bodies, body)
	t.mu.Unlock()
	return resp, nil
}

func (t *liveDropTransport) dropLatest() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.bodies) > 0 {
		t.bodies[len(t.bodies)-1].drop()
	}
}

type liveDropBody struct {
	io.ReadCloser
	once    sync.Once
	dropped chan struct{}
}

func (b *liveDropBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	select {
	case <-b.dropped:
		// The error the logged failures carried.
		return 0, errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=5, ErrCode=NO_ERROR, debug=""`)
	default:
	}
	return n, err
}

func (b *liveDropBody) drop() {
	b.once.Do(func() {
		close(b.dropped)
		_ = b.ReadCloser.Close()
	})
}

// TestLiveFableResumesAfterLostStream talks to the signed-in Cursor account.
// It loses the stream while a shell call is pending, as Cursor's server did in
// the logged failures, and checks what only the real server can show: that the
// next pass of the run resumes the turn from the checkpoint, that the server
// asks for the pending call again under the same tool call id, and that the
// turn then ends normally.
func TestLiveFableResumesAfterLostStream(t *testing.T) {
	if os.Getenv("CURSOR_FABLE_LIVE") != "1" {
		t.Skip("set CURSOR_FABLE_LIVE=1 to run the live lost-stream check")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// go test sandboxes appdata, so load the real account store directly.
	root := filepath.Join(home, ".cursor-local-assistant-v2", "data")
	manager := cursoraccount.NewManager(root, filepath.Join(root, "cursor-account.json"), &http.Client{Timeout: 3 * time.Minute})
	accountID := manager.CurrentAccountID()
	if _, err := manager.AuthorizationFor(context.Background(), accountID); err != nil {
		t.Fatalf("cursor account is not signed in: %v", err)
	}
	gateway := OpenCursorAccountGateway(manager)
	stateDir, err := os.MkdirTemp("", "cursor-fable-resume")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("CURSOR_BYOK_HOME", stateDir)

	transport := &liveDropTransport{base: http.DefaultTransport}
	previousClient := cursorAccountHTTPClient
	cursorAccountHTTPClient = &http.Client{Transport: transport}
	t.Cleanup(func() { cursorAccountHTTPClient = previousClient })

	const marker = "resume-check-7319"
	conversationID, runID := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() { cursorAccountForgetInterruptions(runID) })
	question := Message{Role: "user", Content: "Run exactly one shell command: `echo " + marker + "`. After its result arrives, reply with the single word done followed by the command output. Do not run any other command."}
	request := func(messages ...Message) StreamRequest {
		return StreamRequest{
			RequestID:       runID,
			RunID:           runID,
			ModelCallID:     uuid.NewString(),
			ConversationID:  conversationID,
			Mode:            agentv1.AgentMode_AGENT_MODE_AGENT,
			ModelID:         "claude-fable-5-2-low-fast",
			ProviderModelID: "claude-fable-5-2-low-fast",
			GroupName:       "cursor_account1",
			CursorAccountID: accountID,
			FastMode:        true,
			ThinkingEffort:  "low",
			Stream:          true,
			Messages:        messages,
			CursorRequestContext: &agentv1.RequestContext{Env: &agentv1.RequestContextEnv{
				OsVersion:      "win32 10.0.26200",
				Shell:          "powershell",
				WorkspacePaths: []string{`C:\work\resume-check`},
				ProjectFolder:  `C:\work\resume-check`,
			}},
		}
	}
	shellOf := func(invocation *ModelEvent) (exec *agentv1.ExecServerMessage, toolCallID string, ok bool) {
		if invocation.ToolInvocation == nil {
			return nil, "", false
		}
		exec = &agentv1.ExecServerMessage{}
		if err := proto.Unmarshal(invocation.ToolInvocation.NativeExec, exec); err != nil {
			return nil, "", false
		}
		switch {
		case exec.GetShellStreamArgs() != nil:
			return exec, exec.GetShellStreamArgs().GetToolCallId(), true
		case exec.GetShellArgs() != nil:
			return exec, exec.GetShellArgs().GetToolCallId(), true
		}
		return nil, "", false
	}
	began := time.Now()
	var trace []string
	var traceMu sync.Mutex
	note := func(format string, args ...any) {
		traceMu.Lock()
		trace = append(trace, fmt.Sprintf("%6dms  ", time.Since(began).Milliseconds())+fmt.Sprintf(format, args...))
		traceMu.Unlock()
	}

	// The first pass: the shell call is dispatched and never answered; a
	// moment later, once Cursor's checkpoint has had time to arrive, the
	// stream is lost.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	first := request(question)
	first.CursorToolResults = make(chan *agentv1.ExecClientMessage, 4)
	var firstToolCallID, firstDispatchID, firstArgs, firstFinish string
	var dropOnce sync.Once
	err = gateway.Stream(ctx, first, func(event ModelEvent) error {
		switch event.Kind {
		case ModelEventKindToolLikeCompleted:
			if _, toolCallID, ok := shellOf(&event); ok && firstToolCallID == "" {
				firstToolCallID, firstDispatchID, firstArgs = toolCallID, event.ToolInvocation.CallID, string(event.ToolInvocation.ArgsJSON)
				note("pass 1: Shell dispatched tool_call=%s; left unanswered", toolCallID)
				dropOnce.Do(func() {
					go func() {
						time.Sleep(2500 * time.Millisecond)
						note("pass 1: stream dropped; saved checkpoint: %t", HasCursorAccountState(conversationID))
						transport.dropLatest()
					}()
				})
			}
		case ModelEventKindTurnFinished:
			firstFinish = event.FinishReason
		}
		return nil
	})
	note("pass 1: returned err=%v finish=%q", err, firstFinish)
	if err != nil || firstFinish != FinishReasonCursorStreamInterrupted {
		t.Fatalf("the pass that lost its stream: err=%v finish=%q, want no error and %q\n%s", err, firstFinish, FinishReasonCursorStreamInterrupted, strings.Join(trace, "\n"))
	}
	if firstToolCallID == "" {
		t.Fatalf("the model never ran the shell command\n%s", strings.Join(trace, "\n"))
	}

	// The second pass of the same run. The client has finished the command in
	// the meantime, so the transcript ends with its result.
	second := request(question,
		Message{Role: "assistant", ToolCalls: []ToolCallDescriptor{{ID: firstDispatchID, Type: "function", Function: ToolCallFunctionShape{Name: "Shell", Arguments: firstArgs}}}},
		Message{Role: "tool", ToolCallID: firstDispatchID, Name: "Shell", Content: marker + "\n"},
	)
	answers := make(chan *agentv1.ExecClientMessage, 4)
	second.CursorToolResults = answers
	var reply strings.Builder
	var secondFinish string
	var reissued []string
	err = gateway.Stream(ctx, second, func(event ModelEvent) error {
		switch event.Kind {
		case ModelEventKindTextDelta:
			reply.WriteString(event.Text)
		case ModelEventKindToolLikeCompleted:
			exec, toolCallID, ok := shellOf(&event)
			if !ok {
				note("pass 2: unexpected tool %s", event.ToolInvocation.ToolName)
				cancel()
				return nil
			}
			reissued = append(reissued, toolCallID)
			note("pass 2: Shell requested again tool_call=%s dispatch_id=%s same_call=%t same_dispatch_id=%t",
				toolCallID, event.ToolInvocation.CallID, toolCallID == firstToolCallID, event.ToolInvocation.CallID == firstDispatchID)
			// What the forwarder answers from its record.
			for _, message := range []*agentv1.ExecClientMessage{
				{Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: marker + "\n"}}}}},
				{Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: 0, Cwd: `C:\work\resume-check`}}}}},
			} {
				message.Id, message.ExecId = exec.GetId(), exec.GetExecId()
				answers <- message
			}
		case ModelEventKindTurnFinished:
			secondFinish = event.FinishReason
		}
		return nil
	})
	note("pass 2: returned err=%v finish=%q reply=%q", err, secondFinish, trimLive(reply.String()))
	t.Logf("trace:\n%s", strings.Join(trace, "\n"))
	if err != nil {
		t.Fatalf("the resuming pass failed: %v frames=%s", err, strings.Join(cursorAccountFrameLog(), ","))
	}
	if len(reissued) != 1 || reissued[0] != firstToolCallID {
		t.Fatalf("the resumed turn requested shell calls %v, want the pending one again (%s)", reissued, firstToolCallID)
	}
	if secondFinish != "stop" || !strings.Contains(reply.String(), marker) {
		t.Fatalf("the resumed turn ended with finish=%q reply=%q, want a normal end and the command output", secondFinish, trimLive(reply.String()))
	}
}

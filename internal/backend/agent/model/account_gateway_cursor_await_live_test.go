package modeladapter

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cursor/gen/agentv1"
	"cursor/internal/cursoraccount"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// TestLiveFableAwaitShellPollsStaySilent talks to the signed-in Cursor account.
// It is the check the local frame tests cannot make: what Cursor's server
// really sends while a model waits on a shell with AwaitShell. The test plays
// the IDE. It starts no process: it answers the background spawn with a shell
// id and serves that shell's terminal file from memory, running at first and
// finished a few seconds later.
//
// What it establishes is printed as a trace: whether the wait is announced
// with a tool card, how many times the terminal file is read, and that those
// reads reach the forwarder as silent execs instead of Read calls of the
// model.
func TestLiveFableAwaitShellPollsStaySilent(t *testing.T) {
	if os.Getenv("CURSOR_FABLE_LIVE") != "1" {
		t.Skip("set CURSOR_FABLE_LIVE=1 to run the live AwaitShell check")
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
	stateDir, err := os.MkdirTemp("", "cursor-fable-await")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("CURSOR_BYOK_HOME", stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		shellID   = 424242
		command   = "ping -n 30 127.0.0.1"
		workspace = `C:\work\await-check`
		terminals = `C:\Users\dev\.cursor\projects\await-check/terminals`
		// The pretended command runs this long, a few dozen polls.
		runFor = 8 * time.Second
	)
	terminalFile := fmt.Sprintf("%s/%d.txt", terminals, shellID)
	terminalFileAt := func(spawned, now time.Time) string {
		elapsed := now.Sub(spawned)
		status, runningFor := "running", (elapsed/(5*time.Second))*(5*time.Second)
		if elapsed >= runFor {
			status, runningFor = "succeeded", runFor
		}
		text := fmt.Sprintf("---\npid: 4242\ncwd: %q\ncommand: %q\ntitle: \"live await check\"\nstatus: %s\nstarted_at: %s\nrunning_for_ms: %d\n---\nPinging 127.0.0.1 with 32 bytes of data:\n",
			workspace, command, status, spawned.UTC().Format("2006-01-02T15:04:05.000Z"), runningFor.Milliseconds())
		if elapsed >= runFor {
			text += fmt.Sprintf("Ping statistics for 127.0.0.1: Packets: Sent = 30, Received = 30, Lost = 0\n\n---\nexit_code: 0\nelapsed_ms: %d\nended_at: %s\n---\n",
				runFor.Milliseconds(), spawned.Add(runFor).UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		return text
	}

	began := time.Now()
	var trace []string
	note := func(format string, args ...any) {
		trace = append(trace, fmt.Sprintf("%6dms  ", time.Since(began).Milliseconds())+fmt.Sprintf(format, args...))
	}
	results := make(chan *agentv1.ExecClientMessage, 256)
	answer := func(exec *agentv1.ExecServerMessage, message *agentv1.ExecClientMessage) {
		message.Id, message.ExecId = exec.GetId(), exec.GetExecId()
		results <- message
	}

	var (
		spawned                              time.Time
		silentPolls, visibleTerminalReads    int
		awaitStarted, awaitCompleted         int
		firstPollAfterCard, finishedSeenByIt bool
		unexpected                           []string
		reply                                strings.Builder
		awaitOutcome                         string
	)
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
		CursorRequestContext: &agentv1.RequestContext{Env: &agentv1.RequestContextEnv{
			OsVersion:       "win32 10.0.26200",
			Shell:           "powershell",
			WorkspacePaths:  []string{workspace},
			ProjectFolder:   workspace,
			TerminalsFolder: terminals,
		}},
		Messages: []Message{{
			Role: "user",
			Content: "Make exactly these two tool calls, in this order, and no others. " +
				"First: Shell with the command `" + command + "` and block_until_ms 0, so that it starts in the background. " +
				"Second: AwaitShell on the shell id the first call returned, with block_until_ms 60000. " +
				"When AwaitShell returns, reply with the single word done.",
		}},
		CursorToolResults: results,
		Stream:            true,
	}, func(event ModelEvent) error {
		switch event.Kind {
		case ModelEventKindTextDelta:
			reply.WriteString(event.Text)
		case ModelEventKindCursorAwaitStarted:
			awaitStarted++
			args := event.CursorAwaitStarted.GetToolCall().GetAwaitToolCall().GetArgs()
			note("wait card started   id=%s task=%s block_until_ms=%d", event.ToolCallID, args.GetTaskId(), args.GetBlockUntilMs())
		case ModelEventKindCursorAwaitCompleted:
			awaitCompleted++
			result := event.CursorAwaitCompleted.GetToolCall().GetAwaitToolCall().GetResult()
			switch {
			case result.GetSuccess().GetComplete() != nil || result.GetComplete() != nil:
				awaitOutcome = "complete"
			case result.GetSuccess().GetStillRunning() != nil || result.GetStillRunning() != nil:
				awaitOutcome = "still_running"
			case result.GetError() != nil:
				awaitOutcome = "error: " + result.GetError().GetError()
			default:
				awaitOutcome = "no result"
			}
			note("wait card completed id=%s outcome=%s", event.ToolCallID, awaitOutcome)
		case ModelEventKindToolLikeCompleted:
			invocation := event.ToolInvocation
			if invocation == nil {
				return nil
			}
			exec := &agentv1.ExecServerMessage{}
			if err := proto.Unmarshal(invocation.NativeExec, exec); err != nil || exec.GetMessage() == nil {
				unexpected = append(unexpected, invocation.ToolName+" without an exec")
				cancel()
				return nil
			}
			now := time.Now()
			switch {
			case exec.GetBackgroundShellSpawnArgs() != nil:
				spawned = now
				note("Shell (background spawn) call=%s command=%q", invocation.CallID, exec.GetBackgroundShellSpawnArgs().GetCommand())
				answer(exec, &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_BackgroundShellSpawnResult{
					BackgroundShellSpawnResult: &agentv1.BackgroundShellSpawnResult{Result: &agentv1.BackgroundShellSpawnResult_Success{
						Success: &agentv1.BackgroundShellSpawnSuccess{ShellId: shellID, Command: command, WorkingDirectory: workspace, Pid: proto.Uint32(4242)},
					}},
				}})
			case exec.GetShellStreamArgs() != nil || exec.GetShellArgs() != nil:
				// A foreground shell: the client moves it to the background at once.
				spawned = now
				note("Shell (stream) call=%s", invocation.CallID)
				answer(exec, &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ShellStream{
					ShellStream: &agentv1.ShellStream{Event: &agentv1.ShellStream_Backgrounded{Backgrounded: &agentv1.ShellStreamBackgrounded{
						ShellId: shellID, Command: command, WorkingDirectory: workspace, Pid: proto.Uint32(4242),
						MsToWait: proto.Int32(0), Reason: agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.Enum(),
					}}},
				}})
			case exec.GetReadArgs() != nil:
				read := exec.GetReadArgs()
				if strings.ReplaceAll(read.GetPath(), `\`, "/") != strings.ReplaceAll(terminalFile, `\`, "/") || spawned.IsZero() {
					note("Read of another file: %q silent=%t", read.GetPath(), invocation.SilentUI)
					answer(exec, &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{
						ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_FileNotFound{FileNotFound: &agentv1.ReadFileNotFound{Path: read.GetPath()}}},
					}})
					return nil
				}
				if invocation.SilentUI {
					silentPolls++
				} else {
					visibleTerminalReads++
				}
				polls := silentPolls + visibleTerminalReads
				if polls == 1 {
					firstPollAfterCard = awaitStarted > 0
				}
				content := terminalFileAt(spawned, now)
				finished := strings.Contains(content, "exit_code:")
				// Every poll would drown the trace: keep the first few, and the one that sees the end.
				if polls <= 3 || (finished && !finishedSeenByIt) {
					note("terminal file read #%d silent=%t tool_call=%s max_chars=%d finished=%t dispatch_id=%s", polls, invocation.SilentUI, read.GetToolCallId(), read.GetMaxChars(), finished, invocation.CallID)
				}
				finishedSeenByIt = finishedSeenByIt || finished
				answer(exec, &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{
					ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
						Path: read.GetPath(), TotalLines: int32(strings.Count(content, "\n")), FileSize: int64(len(content)),
						Output: &agentv1.ReadSuccess_Content{Content: content},
					}}},
				}})
			default:
				unexpected = append(unexpected, invocation.ToolName)
				note("unexpected exec %s: %s", invocation.ToolName, trimLive(string(invocation.ArgsJSON)))
				cancel()
			}
		}
		return nil
	})
	note("stream ended err=%v", err)
	t.Logf("trace:\n%s", strings.Join(trace, "\n"))
	t.Logf("terminal file reads: %d silent, %d shown as a Read of the model; wait cards: %d started, %d completed (%s); first read came after the card: %t; reply=%q",
		silentPolls, visibleTerminalReads, awaitStarted, awaitCompleted, awaitOutcome, firstPollAfterCard, trimLive(reply.String()))

	if len(unexpected) > 0 {
		t.Fatalf("the model made calls this check does not serve: %v", unexpected)
	}
	if err != nil {
		t.Fatalf("%v frames=%s", err, strings.Join(cursorAccountFrameLog(), ","))
	}
	if spawned.IsZero() {
		t.Fatal("the model never started the shell")
	}
	if silentPolls+visibleTerminalReads == 0 {
		t.Fatal("the model never waited on the shell: the terminal file was not read")
	}
	if !finishedSeenByIt {
		t.Fatal("the wait ended before the terminal file showed the command's end")
	}
	// A wait that polls must not surface as one Read call after another: at
	// most the first read may, when Cursor sends no card before it.
	if visibleTerminalReads > 1 {
		t.Fatalf("%d terminal file reads were handed on as Read calls of the model", visibleTerminalReads)
	}
	if silentPolls < 3 {
		t.Fatalf("only %d reads were answered silently; an %s wait polls many times", silentPolls, runFor)
	}
	if awaitStarted != awaitCompleted {
		t.Fatalf("wait cards: %d started, %d completed", awaitStarted, awaitCompleted)
	}
}

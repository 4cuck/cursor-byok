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

// TestLiveFableIsToldAboutAFinishedBackgroundTask talks to the signed-in
// Cursor account. The model starts a shell in the background in one turn. The
// next run carries no user message: its action reports that the shell has
// finished, the way the IDE starts such a run. The check is what only the
// real server can show: that it takes the relayed action, writes the notice
// of the finished task into the conversation, and that the model answers it.
func TestLiveFableIsToldAboutAFinishedBackgroundTask(t *testing.T) {
	if os.Getenv("CURSOR_FABLE_LIVE") != "1" {
		t.Skip("set CURSOR_FABLE_LIVE=1 to run the live background-task notice check")
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
	stateDir, err := os.MkdirTemp("", "cursor-fable-task-notice")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("CURSOR_BYOK_HOME", stateDir)

	const (
		shellID   = 424242
		command   = "ping -n 30 127.0.0.1"
		workspace = `C:\work\wakeup-check`
		terminals = `C:\Users\dev\.cursor\projects\wakeup-check/terminals`
	)
	terminalFile := fmt.Sprintf("%s/%d.txt", terminals, shellID)
	finishedFile := "---\npid: 4242\ncommand: \"ping -n 30 127.0.0.1\"\n---\nPing statistics for 127.0.0.1: Packets: Sent = 30, Received = 30, Lost = 0\n\n---\nexit_code: 0\nelapsed_ms: 29412\n---\n"
	conversationID := uuid.NewString()
	type turn struct {
		reply    string
		finish   string
		notices  []*agentv1.UserMessage
		spawned  bool
		surprise []string
	}
	play := func(messages []Message, action *agentv1.ConversationAction) turn {
		runID := uuid.NewString()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		results := make(chan *agentv1.ExecClientMessage, 16)
		var played turn
		var reply strings.Builder
		err := gateway.Stream(ctx, StreamRequest{
			RequestID: runID, RunID: runID, ModelCallID: uuid.NewString(), ConversationID: conversationID,
			Mode: agentv1.AgentMode_AGENT_MODE_AGENT, ModelID: "claude-fable-5-2-low-fast", ProviderModelID: "claude-fable-5-2-low-fast",
			GroupName: "cursor_account1", CursorAccountID: accountID, FastMode: true, ThinkingEffort: "low", Stream: true,
			Messages: messages, CursorToolResults: results, CursorConversationAction: action,
			CursorRequestContext: &agentv1.RequestContext{Env: &agentv1.RequestContextEnv{
				OsVersion: "win32 10.0.26200", Shell: "powershell", WorkspacePaths: []string{workspace}, ProjectFolder: workspace, TerminalsFolder: terminals,
			}},
		}, func(event ModelEvent) error {
			switch event.Kind {
			case ModelEventKindTextDelta:
				reply.WriteString(event.Text)
			case ModelEventKindCursorUserMessageAppended:
				played.notices = append(played.notices, event.CursorUserMessage)
			case ModelEventKindTurnFinished:
				played.finish = event.FinishReason
			case ModelEventKindToolLikeCompleted:
				invocation := event.ToolInvocation
				exec := &agentv1.ExecServerMessage{}
				if invocation == nil || proto.Unmarshal(invocation.NativeExec, exec) != nil || exec.GetMessage() == nil {
					return nil
				}
				answer := func(message *agentv1.ExecClientMessage) {
					message.Id, message.ExecId = exec.GetId(), exec.GetExecId()
					results <- message
				}
				switch {
				case exec.GetBackgroundShellSpawnArgs() != nil:
					played.spawned = true
					answer(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_BackgroundShellSpawnResult{
						BackgroundShellSpawnResult: &agentv1.BackgroundShellSpawnResult{Result: &agentv1.BackgroundShellSpawnResult_Success{
							Success: &agentv1.BackgroundShellSpawnSuccess{ShellId: shellID, Command: command, WorkingDirectory: workspace, Pid: proto.Uint32(4242)},
						}},
					}})
				case exec.GetShellStreamArgs() != nil || exec.GetShellArgs() != nil:
					// A foreground shell: the client moves it to the background at once.
					played.spawned = true
					answer(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ShellStream{
						ShellStream: &agentv1.ShellStream{Event: &agentv1.ShellStream_Backgrounded{Backgrounded: &agentv1.ShellStreamBackgrounded{
							ShellId: shellID, Command: command, WorkingDirectory: workspace, Pid: proto.Uint32(4242),
							MsToWait: proto.Int32(0), Reason: agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.Enum(),
						}}},
					}})
				case exec.GetReadArgs() != nil:
					// The model may look at the output the notice points to.
					read := exec.GetReadArgs()
					if strings.ReplaceAll(read.GetPath(), `\`, "/") == strings.ReplaceAll(terminalFile, `\`, "/") {
						answer(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{
							ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
								Path: read.GetPath(), TotalLines: 9, FileSize: int64(len(finishedFile)), Output: &agentv1.ReadSuccess_Content{Content: finishedFile},
							}}},
						}})
						return nil
					}
					answer(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{
						ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_FileNotFound{FileNotFound: &agentv1.ReadFileNotFound{Path: read.GetPath()}}},
					}})
				default:
					played.surprise = append(played.surprise, invocation.ToolName)
					cancel()
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%v frames=%s", err, strings.Join(cursorAccountFrameLog(), ","))
		}
		played.reply = reply.String()
		return played
	}

	request := "Start the command `" + command + "` in the background with the Shell tool (block_until_ms 0). Do not wait for it. Then reply with the single word started."
	first := play([]Message{{Role: "user", Content: request}}, nil)
	if !first.spawned || len(first.surprise) > 0 {
		t.Fatalf("the model did not just start the shell: spawned=%t other calls=%v reply=%q", first.spawned, first.surprise, trimLive(first.reply))
	}

	second := play(
		[]Message{{Role: "user", Content: request}, {Role: "assistant", Content: first.reply}},
		&agentv1.ConversationAction{Action: &agentv1.ConversationAction_BackgroundTaskCompletionAction{
			BackgroundTaskCompletionAction: &agentv1.BackgroundTaskCompletionAction{Completions: []*agentv1.BackgroundTaskCompletion{{
				TaskId:     fmt.Sprint(shellID),
				Kind:       agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SHELL,
				Status:     agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS,
				Title:      command,
				Detail:     proto.String("exit code 0"),
				OutputPath: proto.String(terminalFile),
				Reason:     agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED,
			}}},
		}},
	)
	t.Logf("the run that reports the finished shell: notices=%d finish=%q reply=%q", len(second.notices), second.finish, trimLive(second.reply))
	if len(second.notices) != 1 {
		t.Fatalf("Cursor's server wrote %d notices for the finished task, want one", len(second.notices))
	}
	notice := second.notices[0]
	t.Logf("the notice Cursor's server wrote:\n%s", notice.GetText())
	if !notice.GetIsSimulatedMsg() || notice.GetSimulatedMsgReason() != agentv1.SimulatedMsgReason_SIMULATED_MSG_REASON_BACKGROUND_TASK_COMPLETION {
		t.Fatalf("the notice is not marked as a background task completion: %v", notice)
	}
	for _, want := range []string{"<system_notification>", "kind: shell", "status: success", fmt.Sprintf("task_id: %d", shellID), "title: " + command} {
		if !strings.Contains(notice.GetText(), want) {
			t.Fatalf("the notice does not say %q:\n%s", want, notice.GetText())
		}
	}
	if len(second.surprise) > 0 || second.finish != "stop" || strings.TrimSpace(second.reply) == "" {
		t.Fatalf("the model did not answer the notice: finish=%q other calls=%v reply=%q", second.finish, second.surprise, trimLive(second.reply))
	}
}

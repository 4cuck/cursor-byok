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

// The checks in this file talk to the signed-in Cursor account. In one turn
// the model leaves work running in the background. The next run carries no
// user message: its action reports that the work has finished, the way the
// IDE starts such a run. They check what only the real server can show: that
// it takes the relayed action, what notice it writes into the conversation
// for it, and that the model answers the notice.

const (
	liveNoticeWorkspace = `C:\work\wakeup-check`
	liveNoticeTerminals = `C:\Users\dev\.cursor\projects\wakeup-check/terminals`
	liveNoticeShellID   = 424242
	liveNoticeCommand   = "ping -n 30 127.0.0.1"
	liveNoticeTaskTitle = "Pong check"
)

// liveNoticeTurn is what one run of a liveNoticeSession showed.
type liveNoticeTurn struct {
	reply     string
	finish    string
	notices   []*agentv1.UserMessage
	shellCall string                // tool call id of the Shell call that started the shell
	launched  *agentv1.SubagentArgs // the subagent the model sent to the background
	surprise  []string              // tool calls no check expects
}

// liveNoticeSession plays the client's side of one conversation.
type liveNoticeSession struct {
	t              *testing.T
	gateway        AccountModelGateway
	accountID      string
	conversationID string
	agentID        string
	// files is what a Read of a path returns, keyed by the path with forward
	// slashes in lower case. Any other path is reported as not found.
	files map[string]string
}

func liveNoticePathKey(path string) string {
	return strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
}

func (session *liveNoticeSession) terminalFile() string {
	return fmt.Sprintf("%s/%d.txt", liveNoticeTerminals, liveNoticeShellID)
}

func (session *liveNoticeSession) transcriptFile() string {
	return `C:\Users\dev\.cursor\projects\wakeup-check\agent-transcripts\` + session.agentID + `.jsonl`
}

func newLiveNoticeSession(t *testing.T) *liveNoticeSession {
	t.Helper()
	if os.Getenv("CURSOR_FABLE_LIVE") != "1" {
		t.Skip("set CURSOR_FABLE_LIVE=1 to run the live background-task notice checks")
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
	stateDir, err := os.MkdirTemp("", "cursor-fable-task-notice")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	t.Setenv("CURSOR_BYOK_HOME", stateDir)

	session := &liveNoticeSession{
		t:              t,
		gateway:        OpenCursorAccountGateway(manager),
		accountID:      accountID,
		conversationID: uuid.NewString(),
		agentID:        uuid.NewString(),
	}
	session.files = map[string]string{
		liveNoticePathKey(session.terminalFile()): "---\npid: 4242\ncommand: \"ping -n 30 127.0.0.1\"\n---\nPing statistics for 127.0.0.1: Packets: Sent = 30, Received = 30, Lost = 0\n\n---\nexit_code: 0\nelapsed_ms: 29412\n---\n",
		liveNoticePathKey(session.transcriptFile()): `{"role":"user","message":{"content":[{"type":"text","text":"Reply with the single word pong."}]}}` + "\n" +
			`{"role":"assistant","message":{"content":[{"type":"text","text":"pong"}]}}` + "\n",
	}
	return session
}

// play runs one turn. messages is this process's own transcript; action, when
// set, is what the run starts with in place of a user message.
func (session *liveNoticeSession) play(messages []Message, action *agentv1.ConversationAction) liveNoticeTurn {
	t := session.t
	t.Helper()
	runID := uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	results := make(chan *agentv1.ExecClientMessage, 16)
	var played liveNoticeTurn
	var reply strings.Builder
	err := session.gateway.Stream(ctx, StreamRequest{
		RequestID: runID, RunID: runID, ModelCallID: uuid.NewString(), ConversationID: session.conversationID,
		Mode: agentv1.AgentMode_AGENT_MODE_AGENT, ModelID: "claude-fable-5-2-low-fast", ProviderModelID: "claude-fable-5-2-low-fast",
		GroupName: "cursor_account1", CursorAccountID: session.accountID, FastMode: true, ThinkingEffort: "low", Stream: true,
		Messages: messages, CursorToolResults: results, CursorConversationAction: action,
		CursorRequestContext: &agentv1.RequestContext{Env: &agentv1.RequestContextEnv{
			OsVersion: "win32 10.0.26200", Shell: "powershell", WorkspacePaths: []string{liveNoticeWorkspace}, ProjectFolder: liveNoticeWorkspace, TerminalsFolder: liveNoticeTerminals,
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
			case exec.GetBackgroundShellSpawnArgs() != nil && played.shellCall == "":
				played.shellCall = firstNonEmptyString(exec.GetBackgroundShellSpawnArgs().GetToolCallId(), invocation.CallID)
				answer(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_BackgroundShellSpawnResult{
					BackgroundShellSpawnResult: &agentv1.BackgroundShellSpawnResult{Result: &agentv1.BackgroundShellSpawnResult_Success{
						Success: &agentv1.BackgroundShellSpawnSuccess{ShellId: liveNoticeShellID, Command: liveNoticeCommand, WorkingDirectory: liveNoticeWorkspace, Pid: proto.Uint32(4242)},
					}},
				}})
			case (exec.GetShellStreamArgs() != nil || exec.GetShellArgs() != nil) && played.shellCall == "":
				// A foreground shell: the client moves it to the background at once.
				played.shellCall = firstNonEmptyString(exec.GetShellStreamArgs().GetToolCallId(), exec.GetShellArgs().GetToolCallId(), invocation.CallID)
				answer(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ShellStream{
					ShellStream: &agentv1.ShellStream{Event: &agentv1.ShellStream_Backgrounded{Backgrounded: &agentv1.ShellStreamBackgrounded{
						ShellId: liveNoticeShellID, Command: liveNoticeCommand, WorkingDirectory: liveNoticeWorkspace, Pid: proto.Uint32(4242),
						MsToWait: proto.Int32(0), Reason: agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.Enum(),
					}}},
				}})
			case exec.GetSubagentArgs() != nil && played.launched == nil:
				// The client hands the subagent to its background and answers
				// the exec at once: who it is, not what it found.
				played.launched = proto.CloneOf(exec.GetSubagentArgs())
				answer(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_SubagentResult{
					SubagentResult: &agentv1.SubagentResult{Result: &agentv1.SubagentResult_Success{Success: &agentv1.SubagentSuccess{
						AgentId:          session.agentID,
						BackgroundReason: agentv1.SubagentBackgroundReason_SUBAGENT_BACKGROUND_REASON_AGENT_REQUEST,
						TranscriptPath:   proto.String(session.transcriptFile()),
					}}},
				}})
			case exec.GetReadArgs() != nil:
				// The model may look at the output a notice points to.
				read := exec.GetReadArgs()
				if content, known := session.files[liveNoticePathKey(read.GetPath())]; known {
					answer(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{
						ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{
							Path: read.GetPath(), TotalLines: int32(strings.Count(content, "\n")), FileSize: int64(len(content)), Output: &agentv1.ReadSuccess_Content{Content: content},
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

// shellFinished is what the client's exec extension reports for the shell
// once it has ended with exit code 0: the Shell call it came from, its
// terminal file, and no detail.
func (session *liveNoticeSession) shellFinished(shellCall string) *agentv1.BackgroundTaskCompletion {
	finished := &agentv1.BackgroundTaskCompletion{
		TaskId:     fmt.Sprint(liveNoticeShellID),
		Kind:       agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SHELL,
		Status:     agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS,
		Title:      liveNoticeCommand,
		OutputPath: proto.String(session.terminalFile()),
		Reason:     agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED,
	}
	if shellCall != "" {
		finished.ToolCallId = proto.String(shellCall)
	}
	return finished
}

// subagentFinished is what the IDE reports for a background subagent that
// ended: its id as task and subagent id, its final message as detail, its
// transcript, and the Task call that launched it.
func (session *liveNoticeSession) subagentFinished(taskCall string) *agentv1.BackgroundTaskCompletion {
	finished := &agentv1.BackgroundTaskCompletion{
		TaskId:     session.agentID,
		Kind:       agentv1.BackgroundTaskKind_BACKGROUND_TASK_KIND_SUBAGENT,
		Status:     agentv1.BackgroundTaskStatus_BACKGROUND_TASK_STATUS_SUCCESS,
		Title:      liveNoticeTaskTitle,
		Detail:     proto.String("pong"),
		OutputPath: proto.String(session.transcriptFile()),
		SubagentId: proto.String(session.agentID),
		Reason:     agentv1.BackgroundTaskCompletionReason_BACKGROUND_TASK_COMPLETION_REASON_TASK_FINISHED,
	}
	if taskCall != "" {
		finished.ToolCallId = proto.String(taskCall)
	}
	return finished
}

func liveNoticeAction(finished ...*agentv1.BackgroundTaskCompletion) *agentv1.ConversationAction {
	return &agentv1.ConversationAction{Action: &agentv1.ConversationAction_BackgroundTaskCompletionAction{
		BackgroundTaskCompletionAction: &agentv1.BackgroundTaskCompletionAction{Completions: finished},
	}}
}

// requireLiveNotices checks the notices of the run that reported finished
// tasks: each is marked as a background task completion, and together they
// say every wanted string.
func requireLiveNotices(t *testing.T, played liveNoticeTurn, want ...string) {
	t.Helper()
	if len(played.notices) == 0 {
		t.Fatal("Cursor's server wrote no notice for the finished work")
	}
	var all strings.Builder
	for index, notice := range played.notices {
		t.Logf("notice %d of %d (title=%q task_id=%q):\n%s", index+1, len(played.notices),
			notice.GetSimulatedMessageMetadata().GetTitle(), notice.GetSimulatedMessageMetadata().GetTaskId(), notice.GetText())
		if !notice.GetIsSimulatedMsg() || notice.GetSimulatedMsgReason() != agentv1.SimulatedMsgReason_SIMULATED_MSG_REASON_BACKGROUND_TASK_COMPLETION {
			t.Fatalf("notice %d is not marked as a background task completion: %v", index+1, notice)
		}
		all.WriteString(notice.GetText())
		all.WriteString("\n")
	}
	for _, text := range want {
		if !strings.Contains(all.String(), text) {
			t.Fatalf("no notice says %q:\n%s", text, all.String())
		}
	}
	if len(played.surprise) > 0 || played.finish != "stop" || strings.TrimSpace(played.reply) == "" {
		t.Fatalf("the model did not answer the notice: finish=%q other calls=%v reply=%q", played.finish, played.surprise, trimLive(played.reply))
	}
}

const liveNoticeShellRequest = "Start the command `" + liveNoticeCommand + "` in the background with the Shell tool (block_until_ms 0). Do not wait for it. Then reply with the single word started."

const liveNoticeSubagentRequest = "Use the Task tool once: subagent_type generalPurpose, description `" + liveNoticeTaskTitle + "`, prompt `Reply with the single word pong.`, run_in_background true. Do not wait for the subagent and do not call any other tool. Then reply with the single word started."

// A shell the model left running has finished.
func TestLiveFableIsToldAboutAFinishedBackgroundTask(t *testing.T) {
	session := newLiveNoticeSession(t)
	first := session.play([]Message{{Role: "user", Content: liveNoticeShellRequest}}, nil)
	if first.shellCall == "" || len(first.surprise) > 0 {
		t.Fatalf("the model did not just start the shell: started=%t other calls=%v reply=%q", first.shellCall != "", first.surprise, trimLive(first.reply))
	}
	t.Logf("the Shell call Cursor's server sent: tool_call_id=%s", first.shellCall)

	second := session.play(
		[]Message{{Role: "user", Content: liveNoticeShellRequest}, {Role: "assistant", Content: first.reply}},
		liveNoticeAction(session.shellFinished(first.shellCall)),
	)
	t.Logf("the run that reports the finished shell: notices=%d finish=%q reply=%q", len(second.notices), second.finish, trimLive(second.reply))
	if len(second.notices) != 1 {
		t.Fatalf("Cursor's server wrote %d notices for the finished shell, want one", len(second.notices))
	}
	requireLiveNotices(t, second, "<system_notification>", "kind: shell", "status: success", fmt.Sprintf("task_id: %d", liveNoticeShellID), "title: "+liveNoticeCommand)
}

// A subagent the model sent to the background has finished. The client
// answered its Task call at once with the handoff; the run reports the end
// with the fields the IDE reports for a subagent.
func TestLiveFableIsToldAboutAFinishedBackgroundSubagent(t *testing.T) {
	session := newLiveNoticeSession(t)
	first := session.play([]Message{{Role: "user", Content: liveNoticeSubagentRequest}}, nil)
	if first.launched == nil || len(first.surprise) > 0 {
		t.Fatalf("the model did not just launch the subagent: launched=%t other calls=%v reply=%q", first.launched != nil, first.surprise, trimLive(first.reply))
	}
	t.Logf("the Task call Cursor's server sent: tool_call_id=%s type=%s run_in_background=%t reply=%q",
		first.launched.GetToolCallId(), first.launched.GetSubagentType(), first.launched.GetRunInBackground(), trimLive(first.reply))
	if !first.launched.GetRunInBackground() {
		t.Fatalf("the subagent was not asked to run in the background: %v", first.launched)
	}

	second := session.play(
		[]Message{{Role: "user", Content: liveNoticeSubagentRequest}, {Role: "assistant", Content: first.reply}},
		liveNoticeAction(session.subagentFinished(first.launched.GetToolCallId())),
	)
	t.Logf("the run that reports the finished subagent: notices=%d finish=%q reply=%q", len(second.notices), second.finish, trimLive(second.reply))
	if len(second.notices) != 1 {
		t.Fatalf("Cursor's server wrote %d notices for the finished subagent, want one", len(second.notices))
	}
	requireLiveNotices(t, second, "<system_notification>", "kind: subagent", "status: success", "task_id: "+session.agentID, "title: "+liveNoticeTaskTitle, "agent_id: "+session.agentID, "detail: pong")
}

// A shell and a subagent finished while the conversation was busy, so the
// IDE reports both with one run. The subagent is one this conversation never
// launched: the server is not asked whether it knows it.
func TestLiveFableIsToldAboutSeveralFinishedBackgroundTasks(t *testing.T) {
	session := newLiveNoticeSession(t)
	first := session.play([]Message{{Role: "user", Content: liveNoticeShellRequest}}, nil)
	if first.shellCall == "" || len(first.surprise) > 0 {
		t.Fatalf("the model did not just start the shell: started=%t other calls=%v reply=%q", first.shellCall != "", first.surprise, trimLive(first.reply))
	}

	second := session.play(
		[]Message{{Role: "user", Content: liveNoticeShellRequest}, {Role: "assistant", Content: first.reply}},
		liveNoticeAction(session.shellFinished(first.shellCall), session.subagentFinished("toolu_01TaskNotOfThisConversation")),
	)
	t.Logf("the run that reports both: notices=%d finish=%q reply=%q", len(second.notices), second.finish, trimLive(second.reply))
	requireLiveNotices(t, second, "kind: shell", fmt.Sprintf("task_id: %d", liveNoticeShellID), "kind: subagent", "task_id: "+session.agentID)
}

// cursor_native_await.go shows the AwaitShell calls of a Cursor-account model.
//
// On a Cursor account the wait is not run here (await_shell_tool.go is the
// BYOK tool). It runs on Cursor's server, which watches the shell by reading
// its terminal file through the client several times a second and reports
// the wait itself with a tool card: started when the wait begins, completed
// with the outcome when it ends. The reads are answered silently as internal
// execs; this file forwards the card and records the call in history, the way
// cursor_native_edit.go does for Cursor's edits.
package forwarder

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"cursor/gen/agentv1"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const awaitShellToolName = "AwaitShell"

// cursorPendingAwait is an upstream AwaitShell card that has started and has
// not reported its result yet.
type cursorPendingAwait struct {
	ToolCallID       string
	ModelCallID      string
	Args             *agentv1.AwaitArgs
	ReasoningContent string
}

func isAwaitShellHistoryToolName(name string) bool {
	return strings.TrimSpace(name) == awaitShellToolName
}

// applyCursorAwaitStarted opens the wait's card in the IDE. The IDE also uses
// this update to know which shell the agent is waiting on, so it does not
// notify the agent a second time when that shell finishes.
func (service *Service) applyCursorAwaitStarted(stream *ActiveStream, started *agentv1.ToolCallStartedUpdate, modelCallID string) error {
	if service == nil || stream == nil || started == nil {
		return nil
	}
	id := modeladapter.CursorAwaitStartedCallID(started)
	card := proto.CloneOf(started.GetToolCall())
	if id == "" || card.GetAwaitToolCall() == nil {
		return nil
	}
	effectiveModelCallID := firstNonEmpty(strings.TrimSpace(modelCallID), strings.TrimSpace(started.GetModelCallId()))

	stream.mu.Lock()
	_, alreadyStarted := stream.activeCursorAwaits[id]
	skip := isTerminalStreamStatus(stream.Status) || alreadyStarted || hasToolResultEntryLocked(stream, id, isAwaitShellHistoryToolName)
	stream.mu.Unlock()
	if skip {
		return nil
	}

	// Text the model wrote before the wait belongs before it in history, and
	// its thinking goes on the call.
	reasoningContent, reasoningSignature, reasoningSignatureSource, reasoningItemID, reasoningStatus, reasoningSummary, err := service.flushPendingTextAndHarvestReasoningForCursorEdit(stream)
	if err != nil {
		return err
	}
	args := proto.CloneOf(card.GetAwaitToolCall().GetArgs())
	stream.mu.Lock()
	if isTerminalStreamStatus(stream.Status) {
		stream.mu.Unlock()
		return nil
	}
	if stream.activeCursorAwaits == nil {
		stream.activeCursorAwaits = make(map[string]cursorPendingAwait)
	}
	stream.activeCursorAwaits[id] = cursorPendingAwait{
		ToolCallID:       id,
		ModelCallID:      effectiveModelCallID,
		Args:             args,
		ReasoningContent: reasoningContent,
	}
	stream.ToolInvocationCount++
	// The wait is answered on the open Cursor stream. When the model then
	// finishes, the turn is over; another pass would resume a finished turn.
	stream.CursorInlineToolTurn = true
	stream.UpdatedAt = time.Now().UTC()
	stream.mu.Unlock()

	if checkpointConversationInitialized(stream) {
		if toolCallPayload, marshalErr := protojson.Marshal(buildAwaitShellToolCall(args, nil)); marshalErr == nil {
			_, _ = service.appendConversationEntries(stream, stream.ConversationID, []HistoryEntry{
				newToolCallEntryWithProviderMetadata(
					stream.TurnSeq,
					stream.RequestID,
					id,
					awaitShellToolName,
					reasoningContent,
					reasoningSignature,
					reasoningSignatureSource,
					reasoningItemID,
					reasoningStatus,
					reasoningSummary,
					"",
					"",
					"",
					toolCallPayload,
				),
			})
		}
	}
	if err := service.broker.Publish(stream.RequestID, StreamEvent{
		Message: buildToolCallStartedMessage(id, effectiveModelCallID, card),
	}); err != nil {
		return err
	}
	return service.publishCheckpoint(stream.RequestID, stream.ConversationID)
}

// applyCursorAwaitCompleted closes the wait's card with the outcome Cursor
// reported and records the result in history.
func (service *Service) applyCursorAwaitCompleted(stream *ActiveStream, completion *agentv1.ToolCallCompletedUpdate, modelCallID string) error {
	if service == nil || stream == nil || completion == nil {
		return nil
	}
	id := modeladapter.CursorAwaitCompletedCallID(completion)
	card := proto.CloneOf(completion.GetToolCall())
	await := card.GetAwaitToolCall()
	if id == "" || await == nil {
		return nil
	}

	stream.mu.Lock()
	if isTerminalStreamStatus(stream.Status) || hasToolResultEntryLocked(stream, id, isAwaitShellHistoryToolName) {
		stream.mu.Unlock()
		return nil
	}
	pending, hadPending := stream.activeCursorAwaits[id]
	delete(stream.activeCursorAwaits, id)
	stream.mu.Unlock()

	reasoningContent := pending.ReasoningContent
	if !hadPending {
		// The start was never reported, so nothing has been flushed or
		// counted for this call yet.
		var err error
		reasoningContent, _, _, _, _, _, err = service.flushPendingTextAndHarvestReasoningForCursorEdit(stream)
		if err != nil {
			return err
		}
		stream.mu.Lock()
		stream.ToolInvocationCount++
		stream.CursorInlineToolTurn = true
		stream.UpdatedAt = time.Now().UTC()
		stream.mu.Unlock()
	}
	if await.GetArgs() == nil && pending.Args != nil {
		await.Args = proto.CloneOf(pending.Args)
	}

	summary := cursorAwaitResultSummary(await.GetArgs(), await.GetResult())
	resultText, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	historyCard := buildAwaitShellToolCall(await.GetArgs(), await.GetResult())
	if err := service.appendToolResult(stream, id, awaitShellToolName, cursorAwaitHistoryArgsJSON(await.GetArgs()), string(resultText), reasoningContent, historyCard); err != nil {
		return err
	}
	effectiveModelCallID := firstNonEmpty(pending.ModelCallID, strings.TrimSpace(modelCallID), strings.TrimSpace(completion.GetModelCallId()))
	if err := service.publishToolCallCompleted(stream.RequestID, id, effectiveModelCallID, card); err != nil {
		return err
	}
	service.deferSummaryCarryForward(stream.ConversationID, stream.RequestID, effectiveModelCallID)
	return service.publishCheckpoint(stream.RequestID, stream.ConversationID)
}

// closeUnreportedCursorAwaits closes every wait whose card is still open
// because Cursor's stream ended, or the turn was canceled, before the wait
// reported a result. The call gets its result in history either way: a call
// left without one breaks the next replay of this conversation.
func (service *Service) closeUnreportedCursorAwaits(stream *ActiveStream, reason string) error {
	if service == nil || stream == nil {
		return nil
	}
	stream.mu.Lock()
	waiting := stream.activeCursorAwaits
	stream.activeCursorAwaits = nil
	stream.mu.Unlock()
	reason = firstNonEmpty(strings.TrimSpace(reason), "The wait ended without a result.")
	for _, id := range slices.Sorted(maps.Keys(waiting)) {
		pending := waiting[id]
		result := &agentv1.AwaitResult{Result: &agentv1.AwaitResult_Error{Error: &agentv1.AwaitError{Error: reason}}}
		card := buildAwaitShellToolCall(pending.Args, result)
		resultText, err := json.Marshal(cursorAwaitResultSummary(pending.Args, result))
		if err != nil {
			return err
		}
		if err := service.appendToolResult(stream, id, awaitShellToolName, cursorAwaitHistoryArgsJSON(pending.Args), string(resultText), pending.ReasoningContent, card); err != nil {
			return err
		}
		if err := service.publishToolCallCompleted(stream.RequestID, id, pending.ModelCallID, card); err != nil {
			return err
		}
	}
	return nil
}

// cursorAwaitHistoryArgsJSON writes the wait's arguments the way the
// AwaitShell tool takes them, so history reads the same for both kinds of
// model.
func cursorAwaitHistoryArgsJSON(args *agentv1.AwaitArgs) []byte {
	values := map[string]any{}
	if shellID := strings.TrimSpace(args.GetTaskId()); shellID != "" {
		values["shell_id"] = shellID
	}
	if args != nil && args.BlockUntilMs != nil {
		values["block_until_ms"] = args.GetBlockUntilMs()
	}
	if pattern := strings.TrimSpace(args.GetRegex()); pattern != "" {
		values["pattern"] = pattern
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// cursorAwaitResultSummary puts the outcome Cursor reported into the result
// shape of the AwaitShell tool.
func cursorAwaitResultSummary(args *agentv1.AwaitArgs, result *agentv1.AwaitResult) awaitShellResult {
	summary := awaitShellResult{ShellID: strings.TrimSpace(args.GetTaskId())}
	complete, running := result.GetComplete(), result.GetStillRunning()
	if success := result.GetSuccess(); success != nil {
		if success.GetComplete() != nil {
			complete = success.GetComplete()
		}
		if success.GetStillRunning() != nil {
			running = success.GetStillRunning()
		}
	}
	patternNote := func(requested bool, match *string) string {
		summary.RegexRequested = requested
		if !requested {
			return ""
		}
		if match == nil || *match == "" {
			return " Pattern did NOT match."
		}
		shown := truncateAwaitShellMatch(*match)
		summary.Matched = true
		summary.RegexMatch = &shown
		return " " + strings.TrimRight("Pattern matched: "+shown, " \t\r\n")
	}
	switch {
	case complete != nil:
		summary.complete = true
		summary.ShellID = firstNonEmpty(summary.ShellID, strings.TrimSpace(complete.GetTaskId()))
		summary.RuntimeMS = complete.GetRuntimeMs()
		summary.OutputFilePath = strings.TrimSpace(complete.GetOutputFilePath())
		summary.OutputLength = complete.GetOutputLength()
		if summary.ShellID == "" {
			// A wait on no shell is a plain sleep.
			seconds := int64(math.Ceil(float64(summary.RuntimeMS) / 1000))
			summary.Status = "waited"
			summary.Message = fmt.Sprintf("Slept for %ds.", max(seconds, 1))
			break
		}
		summary.Status = backgroundShellStatusCompleted
		exitText := "unknown"
		if complete.ExitCode != nil {
			code := int64(complete.GetExitCode())
			summary.ExitCode = &code
			exitText = strconv.FormatInt(code, 10)
		}
		summary.Message = fmt.Sprintf("Task completed in %dms with exit code: %s.", summary.RuntimeMS, exitText) +
			patternNote(complete.GetRegexRequested(), complete.RegexMatch)
	case running != nil:
		summary.ShellID = firstNonEmpty(summary.ShellID, strings.TrimSpace(running.GetTaskId()))
		summary.Status = backgroundShellStatusRunning
		summary.RuntimeMS = running.GetRuntimeMs()
		summary.OutputFilePath = strings.TrimSpace(running.GetOutputFilePath())
		summary.OutputLength = running.GetOutputLength()
		summary.Message = fmt.Sprintf("Task still running after %dms...", summary.RuntimeMS) +
			patternNote(running.GetRegexRequested(), running.RegexMatch)
		// Still running after a blocking wait that no pattern ended: the
		// wait ran out.
		summary.TimedOut = args.GetBlockUntilMs() > 0 && !summary.Matched
	default:
		reason := firstNonEmpty(strings.TrimSpace(result.GetError().GetError()), "Cursor reported no result for this wait")
		summary.Status = "error"
		summary.Message = "Error awaiting task: " + reason
		summary.errorText = reason
	}
	return summary
}

// hasToolResultEntryLocked reports whether this turn's history already holds
// a result for the tool call, from a tool whose name isName accepts. Caller
// holds stream.mu.
func hasToolResultEntryLocked(stream *ActiveStream, toolCallID string, isName func(string) bool) bool {
	if stream == nil || stream.CheckpointConversation == nil || strings.TrimSpace(toolCallID) == "" {
		return false
	}
	id := strings.TrimSpace(toolCallID)
	entries := stream.CheckpointConversation.Entries
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Kind != "tool_result" || entry.TurnSeq != stream.TurnSeq || entry.RequestID != stream.RequestID {
			continue
		}
		if historyEntryToolCallID(entry) != id {
			continue
		}
		var payload toolResultEntryPayload
		if err := json.Unmarshal(entry.Payload, &payload); err == nil && isName(payload.ToolName) {
			return true
		}
	}
	return false
}

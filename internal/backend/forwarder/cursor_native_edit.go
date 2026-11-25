package forwarder

import (
	"encoding/json"
	"maps"
	pathpkg "path"
	"slices"
	"strings"
	"time"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	maxObservedFileSnapshotBytes   = 2 << 20 // 2 MiB per file
	maxObservedFileSnapshotEntries = 64
)

type cursorPendingEdit struct {
	ToolCallID       string
	ModelCallID      string
	Path             string
	ReasoningContent string
	Started          bool
}

type observedFileSnapshot struct {
	Exists  bool
	Content string
}

func observedFileContentKey(rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return ""
	}
	normalized := strings.ReplaceAll(trimmed, `\`, "/")
	cleaned := pathpkg.Clean(normalized)
	if len(cleaned) >= 2 && cleaned[1] == ':' {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func setObservedFileContent(stream *ActiveStream, rawPath string, snapshot observedFileSnapshot) {
	if stream == nil {
		return
	}
	key := observedFileContentKey(rawPath)
	if key == "" {
		return
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.observedFileContents == nil {
		stream.observedFileContents = make(map[string]observedFileSnapshot)
	}
	if _, exists := stream.observedFileContents[key]; !exists && len(stream.observedFileContents) >= maxObservedFileSnapshotEntries {
		// Evict an arbitrary entry to keep memory strictly bounded.
		for k := range stream.observedFileContents {
			delete(stream.observedFileContents, k)
			break
		}
	}
	stream.observedFileContents[key] = snapshot
}

func deleteObservedFileContent(stream *ActiveStream, rawPath string) {
	if stream == nil {
		return
	}
	key := observedFileContentKey(rawPath)
	if key == "" {
		return
	}
	stream.mu.Lock()
	delete(stream.observedFileContents, key)
	stream.mu.Unlock()
}

func lookupObservedFileContent(stream *ActiveStream, rawPath string) (observedFileSnapshot, bool) {
	if stream == nil {
		return observedFileSnapshot{}, false
	}
	key := observedFileContentKey(rawPath)
	if key == "" {
		return observedFileSnapshot{}, false
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	snapshot, ok := stream.observedFileContents[key]
	return snapshot, ok
}

func extractPathFromArgsJSON(argsJSON []byte) string {
	if len(argsJSON) == 0 {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return ""
	}
	return firstNonEmpty(
		readStringAny(args["path"]),
		readStringAny(args["file_path"]),
		readStringAny(args["filePath"]),
		readStringAny(args["Path"]),
		readStringAny(args["FilePath"]),
	)
}

func readArgsWasPartialOrDigest(argsJSON []byte) bool {
	if len(argsJSON) == 0 {
		return false
	}
	var args map[string]any
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return false
	}
	if offset, found, valid := readJSONIntAny(args, "offset", "Offset"); found && (!valid || offset != 0) {
		return true
	}
	if limit, found, valid := readJSONIntAny(args, "limit", "Limit"); found && (!valid || limit != 0) {
		return true
	}
	if digestOnly, found, valid := readJSONBoolAny(args, "digest_only", "digestOnly"); found && (!valid || digestOnly) {
		return true
	}
	return false
}

func extractWriteAfterTextFromArgsJSON(argsJSON []byte) (string, bool) {
	if len(argsJSON) == 0 {
		return "", false
	}
	var args map[string]any
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", false
	}
	for _, key := range []string{"contents", "content", "fileText", "file_text", "stream_content", "streamContent"} {
		if text, ok := args[key].(string); ok {
			return text, true
		}
	}
	return "", false
}

// observeNativeExecFileSnapshot records complete UTF-8 file snapshots seen via
// native Read/Write/Delete execs (including SilentUI execs) so standalone
// native Write fallback cards compute real before/after diffs.
func observeNativeExecFileSnapshot(stream *ActiveStream, pending runtimecore.PendingExec, msg *agentv1.ExecClientMessage) {
	if stream == nil || msg == nil {
		return
	}
	argsPath := extractPathFromArgsJSON(pending.ArgsJSON)
	switch strings.TrimSpace(pending.ExecKind) {
	case "read":
		readResult := msg.GetReadResult()
		if readResult == nil {
			readResult = msg.GetRedactedReadResult()
		}
		if readResult == nil {
			return
		}
		switch item := readResult.GetResult().(type) {
		case *agentv1.ReadResult_FileNotFound:
			setObservedFileContent(stream, firstNonEmpty(item.FileNotFound.GetPath(), argsPath), observedFileSnapshot{Exists: false})
		case *agentv1.ReadResult_Success:
			path := firstNonEmpty(item.Success.GetPath(), argsPath)
			if item.Success.GetTruncated() || item.Success.GetOutputBlobId() != nil || readArgsWasPartialOrDigest(pending.ArgsJSON) {
				deleteObservedFileContent(stream, path)
				return
			}
			content, ok := extractReadContentForEdit(readResult)
			if !ok || len(content) > maxObservedFileSnapshotBytes {
				deleteObservedFileContent(stream, path)
				return
			}
			setObservedFileContent(stream, path, observedFileSnapshot{Exists: true, Content: content})
		}
	case "delete":
		if success := msg.GetDeleteResult().GetSuccess(); success != nil {
			setObservedFileContent(stream, firstNonEmpty(success.GetPath(), argsPath), observedFileSnapshot{Exists: false})
		}
	case "write":
		recordObservedWriteResult(stream, pending.ArgsJSON, msg.GetWriteResult())
	}
}

func recordObservedWriteResult(stream *ActiveStream, argsJSON []byte, writeResult *agentv1.WriteResult) {
	if stream == nil || writeResult == nil || writeResult.GetSuccess() == nil {
		return
	}
	success := writeResult.GetSuccess()
	path := firstNonEmpty(success.GetPath(), extractPathFromArgsJSON(argsJSON))
	if success.FileContentAfterWrite != nil {
		after := normalizeLineEndingsToLF(success.GetFileContentAfterWrite())
		if len(after) <= maxObservedFileSnapshotBytes {
			setObservedFileContent(stream, path, observedFileSnapshot{Exists: true, Content: after})
		} else {
			deleteObservedFileContent(stream, path)
		}
		return
	}
	if after, ok := extractWriteAfterTextFromArgsJSON(argsJSON); ok && len(after) <= maxObservedFileSnapshotBytes {
		setObservedFileContent(stream, path, observedFileSnapshot{Exists: true, Content: normalizeLineEndingsToLF(after)})
		return
	}
	deleteObservedFileContent(stream, path)
}

// enrichNativeWriteToolCallWithObservedBefore replaces the default "+N new file"
// diff on a standalone native Write card with a real before/after unified diff
// whenever the stream previously observed the file's complete content.
func enrichNativeWriteToolCallWithObservedBefore(stream *ActiveStream, pending runtimecore.PendingExec, writeResult *agentv1.WriteResult, toolCall *agentv1.ToolCall) {
	if stream == nil || pending.SilentUI || strings.TrimSpace(pending.ExecKind) != "write" || writeResult.GetSuccess() == nil {
		return
	}
	edit := toolCall.GetEditToolCall()
	if edit == nil || edit.GetResult().GetSuccess() == nil {
		return
	}
	path := firstNonEmpty(writeResult.GetSuccess().GetPath(), edit.GetArgs().GetPath(), extractPathFromArgsJSON(pending.ArgsJSON))
	before, ok := lookupObservedFileContent(stream, path)
	if !ok || !before.Exists {
		return
	}
	afterContent := edit.GetResult().GetSuccess().GetAfterFullFileContent()
	if writeResult.GetSuccess().FileContentAfterWrite != nil {
		afterContent = writeResult.GetSuccess().GetFileContentAfterWrite()
	} else if fromArgs, hasText := extractWriteAfterTextFromArgsJSON(pending.ArgsJSON); hasText {
		afterContent = fromArgs
	}
	edit.Result = buildSuccessfulWriteResult(path, before.Content, afterContent)
}

func noteCursorEditPartial(stream *ActiveStream, toolCallID string, modelCallID string, toolCall *agentv1.ToolCall) {
	if stream == nil || toolCall.GetEditToolCall() == nil {
		return
	}
	id := strings.TrimSpace(toolCallID)
	if id == "" {
		return
	}
	path := strings.TrimSpace(toolCall.GetEditToolCall().GetArgs().GetPath())
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.CursorToolResults == nil || isTerminalStreamStatus(stream.Status) {
		return
	}
	if stream.activeCursorEdits == nil {
		stream.activeCursorEdits = make(map[string]cursorPendingEdit)
	}
	existing, ok := stream.activeCursorEdits[id]
	if !ok {
		existing = cursorPendingEdit{
			ToolCallID:  id,
			ModelCallID: strings.TrimSpace(modelCallID),
		}
	}
	if path != "" {
		existing.Path = path
	}
	if existing.ModelCallID == "" {
		existing.ModelCallID = strings.TrimSpace(modelCallID)
	}
	stream.activeCursorEdits[id] = existing
}

func isEditHistoryToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case "Write", "Edit", "PatchEdit", "PatchEditLines", "PatchEditSpan":
		return true
	default:
		return false
	}
}

func hasCompletedEditHistoryEntryLocked(stream *ActiveStream, toolCallID string) bool {
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
		if err := json.Unmarshal(entry.Payload, &payload); err == nil && isEditHistoryToolName(payload.ToolName) {
			return true
		}
	}
	return false
}

func (service *Service) flushPendingTextAndHarvestReasoningForCursorEdit(stream *ActiveStream) (
	reasoningContent string,
	reasoningSignature string,
	reasoningSignatureSource string,
	reasoningItemID string,
	reasoningStatus string,
	reasoningSummary json.RawMessage,
	err error,
) {
	if stream == nil {
		return "", "", "", "", "", nil, nil
	}
	stream.mu.Lock()
	conversationID := stream.ConversationID
	turnSeq := stream.TurnSeq
	requestID := stream.RequestID
	accumulatedText := string(stream.ProviderAccumulatedText)
	reasoningContent = string(stream.ProviderAccumulatedReasoning)
	reasoningSignature = stream.ProviderAccumulatedReasoningSignature
	reasoningSignatureSource = stream.ProviderAccumulatedReasoningSignatureSource
	reasoningItemID = stream.ProviderAccumulatedReasoningItemID
	reasoningStatus = stream.ProviderAccumulatedReasoningStatus
	reasoningSummary = append(json.RawMessage(nil), stream.ProviderAccumulatedReasoningSummary...)
	stream.mu.Unlock()

	flushedVisibleText := strings.TrimSpace(accumulatedText) != ""
	if flushedVisibleText {
		if err := service.flushAssistantText(
			stream,
			conversationID,
			turnSeq,
			requestID,
			accumulatedText,
			reasoningContent,
			reasoningSignature,
			reasoningSignatureSource,
			reasoningItemID,
			reasoningStatus,
			reasoningSummary,
			false,
		); err != nil {
			return "", "", "", "", "", nil, err
		}
	}
	stream.mu.Lock()
	if flushedVisibleText {
		stream.ProviderPassFlushedAssistantText = true
	}
	stream.ProviderAccumulatedText = nil
	clearProviderAccumulatedReasoning(stream)
	stream.UpdatedAt = time.Now().UTC()
	stream.mu.Unlock()
	return reasoningContent, reasoningSignature, reasoningSignatureSource, reasoningItemID, reasoningStatus, reasoningSummary, nil
}

// applyCursorEditStarted forwards Cursor's upstream EditToolCall start card so
// the IDE tracks the real edit lifecycle while low-level Read/Write execs run
// silently underneath.
func (service *Service) applyCursorEditStarted(stream *ActiveStream, started *agentv1.ToolCallStartedUpdate, modelCallID string) error {
	if service == nil || stream == nil || started == nil {
		return nil
	}
	id := modeladapter.CursorEditStartedCallID(started)
	if id == "" {
		return nil
	}
	card := proto.CloneOf(started.GetToolCall())
	if card == nil || card.GetEditToolCall() == nil {
		return nil
	}
	path := strings.TrimSpace(card.GetEditToolCall().GetArgs().GetPath())
	effectiveModelCallID := firstNonEmpty(strings.TrimSpace(modelCallID), strings.TrimSpace(started.GetModelCallId()))

	stream.mu.Lock()
	if isTerminalStreamStatus(stream.Status) || hasCompletedEditHistoryEntryLocked(stream, id) {
		stream.mu.Unlock()
		return nil
	}
	existing, exists := stream.activeCursorEdits[id]
	alreadyStarted := exists && existing.Started
	stream.mu.Unlock()

	if !alreadyStarted {
		reasoningContent, reasoningSignature, reasoningSignatureSource, reasoningItemID, reasoningStatus, reasoningSummary, err := service.flushPendingTextAndHarvestReasoningForCursorEdit(stream)
		if err != nil {
			return err
		}
		stream.mu.Lock()
		if isTerminalStreamStatus(stream.Status) {
			stream.mu.Unlock()
			return nil
		}
		if stream.activeCursorEdits == nil {
			stream.activeCursorEdits = make(map[string]cursorPendingEdit)
		}
		existing = stream.activeCursorEdits[id]
		existing.ToolCallID = id
		existing.ModelCallID = firstNonEmpty(effectiveModelCallID, existing.ModelCallID)
		existing.Path = firstNonEmpty(path, existing.Path)
		existing.ReasoningContent = reasoningContent
		existing.Started = true
		stream.activeCursorEdits[id] = existing
		stream.ToolInvocationCount++
		stream.CursorInlineToolTurn = true
		stream.UpdatedAt = time.Now().UTC()
		stream.mu.Unlock()

		if checkpointConversationInitialized(stream) {
			historyCard := buildStartedWriteHistoryToolCall(existing.Path)
			if toolCallPayload, marshalErr := protojson.Marshal(historyCard); marshalErr == nil {
				_, _ = service.appendConversationEntries(stream, stream.ConversationID, []HistoryEntry{
					newToolCallEntryWithProviderMetadata(
						stream.TurnSeq,
						stream.RequestID,
						id,
						"Edit",
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
	} else if path != "" {
		stream.mu.Lock()
		if cur, ok := stream.activeCursorEdits[id]; ok {
			cur.Path = path
			stream.activeCursorEdits[id] = cur
		}
		stream.mu.Unlock()
	}

	if err := service.broker.Publish(stream.RequestID, StreamEvent{
		Message: buildToolCallStartedMessage(id, effectiveModelCallID, card),
	}); err != nil {
		return err
	}
	return service.publishCheckpoint(stream.RequestID, stream.ConversationID)
}

// applyCursorEditCompleted publishes Cursor's authoritative EditToolCall outcome
// (including BeforeFullFileContent, AfterFullFileContent, DiffString,
// LinesAdded, and LinesRemoved) directly to the IDE.
func (service *Service) applyCursorEditCompleted(stream *ActiveStream, completion *agentv1.ToolCallCompletedUpdate, modelCallID string) error {
	if service == nil || stream == nil || completion == nil {
		return nil
	}
	id := modeladapter.CursorEditCompletedCallID(completion)
	if id == "" {
		return nil
	}
	card := proto.CloneOf(completion.GetToolCall())
	edit := card.GetEditToolCall()
	if edit == nil || edit.GetResult().GetResult() == nil {
		return nil
	}

	stream.mu.Lock()
	if isTerminalStreamStatus(stream.Status) || hasCompletedEditHistoryEntryLocked(stream, id) {
		stream.mu.Unlock()
		return nil
	}
	pending, hadPending := stream.activeCursorEdits[id]
	delete(stream.activeCursorEdits, id)
	stream.mu.Unlock()

	reasoningContent := pending.ReasoningContent
	if !hadPending || !pending.Started {
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

	result := edit.GetResult()
	path := firstNonEmpty(resultPath(result), strings.TrimSpace(edit.GetArgs().GetPath()), pending.Path)
	if edit.Args == nil {
		edit.Args = &agentv1.EditArgs{Path: path}
	} else if strings.TrimSpace(edit.Args.GetPath()) == "" && path != "" {
		edit.Args.Path = path
	}
	if success := result.GetSuccess(); success != nil {
		after := normalizeLineEndingsToLF(success.GetAfterFullFileContent())
		if path != "" && len(after) <= maxObservedFileSnapshotBytes {
			setObservedFileContent(stream, path, observedFileSnapshot{Exists: true, Content: after})
		} else if path != "" {
			deleteObservedFileContent(stream, path)
		}
	}

	toolName := firstNonEmpty(inferEditToolNameFromToolCall(edit), "Edit")
	historyArgsJSON, err := json.Marshal(map[string]any{"path": path})
	if err != nil {
		return err
	}
	historyCard := buildCompletedWriteHistoryToolCall(path, result)
	historyText := summarizeWriteHistoryResult(path, result)
	if err := service.appendToolResult(stream, id, toolName, historyArgsJSON, historyText, reasoningContent, historyCard); err != nil {
		return err
	}
	effectiveModelCallID := firstNonEmpty(pending.ModelCallID, strings.TrimSpace(modelCallID), strings.TrimSpace(completion.GetModelCallId()))
	if err := service.publishToolCallCompleted(stream.RequestID, id, effectiveModelCallID, card); err != nil {
		return err
	}
	service.deferSummaryCarryForward(stream.ConversationID, stream.RequestID, effectiveModelCallID)
	return service.publishCheckpoint(stream.RequestID, stream.ConversationID)
}

func (service *Service) closeUnreportedCursorEdits(stream *ActiveStream, reason string, canceled bool) error {
	if service == nil || stream == nil {
		return nil
	}
	stream.mu.Lock()
	waiting := stream.activeCursorEdits
	stream.activeCursorEdits = nil
	stream.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(waiting)) {
		pending := waiting[id]
		var result *agentv1.EditResult
		if canceled {
			result = &agentv1.EditResult{
				Result: &agentv1.EditResult_Rejected{
					Rejected: &agentv1.EditRejected{
						Path:   pending.Path,
						Reason: strings.TrimSpace(reason),
					},
				},
			}
		} else {
			result = buildEditErrorResult(pending.Path, reason)
		}
		card := buildCompletedEditToolCall(pending.Path, result)
		historyArgsJSON, _ := json.Marshal(map[string]any{"path": pending.Path})
		if err := service.appendToolResult(stream, id, "Edit", historyArgsJSON, summarizeWriteHistoryResult(pending.Path, result), pending.ReasoningContent, card); err != nil {
			return err
		}
		if err := service.publishToolCallCompleted(stream.RequestID, id, pending.ModelCallID, card); err != nil {
			return err
		}
	}
	return nil
}

func silentNativeExecErrorMessage(pending runtimecore.PendingExec, errText string) *agentv1.ExecClientMessage {
	path := extractPathFromArgsJSON(pending.ArgsJSON)
	detail := firstNonEmpty(strings.TrimSpace(errText), "internal exec failed")
	switch strings.TrimSpace(pending.ExecKind) {
	case "read":
		return &agentv1.ExecClientMessage{
			Message: &agentv1.ExecClientMessage_ReadResult{
				ReadResult: &agentv1.ReadResult{
					Result: &agentv1.ReadResult_Error{
						Error: &agentv1.ReadError{Path: path, Error: detail},
					},
				},
			},
		}
	default:
		return &agentv1.ExecClientMessage{
			Message: &agentv1.ExecClientMessage_WriteResult{
				WriteResult: &agentv1.WriteResult{
					Result: &agentv1.WriteResult_Error{
						Error: &agentv1.WriteError{Path: path, Error: detail},
					},
				},
			},
		}
	}
}

func (service *Service) rejectSilentNativeExecBeforeDispatch(stream *ActiveStream, invocation runtimecore.ToolInvocation, cause error) error {
	if stream == nil || cause == nil {
		return cause
	}
	original := &agentv1.ExecServerMessage{}
	if err := proto.Unmarshal(invocation.NativeExec, original); err == nil {
		rememberNativeExecSlot(stream, invocation.CallID, original)
		if original.GetReadArgs() != nil || original.GetRedactedReadArgs() != nil {
			path := firstNonEmpty(original.GetReadArgs().GetPath(), original.GetRedactedReadArgs().GetPath())
			errMsg := &agentv1.ExecClientMessage{
				Message: &agentv1.ExecClientMessage_ReadResult{
					ReadResult: &agentv1.ReadResult{
						Result: &agentv1.ReadResult_Error{
							Error: &agentv1.ReadError{Path: strings.TrimSpace(path), Error: strings.TrimSpace(cause.Error())},
						},
					},
				},
			}
			if deliverCursorToolResult(stream, invocation.CallID, errMsg) {
				return nil
			}
			return nil
		}
		if write := original.GetWriteArgs(); write != nil {
			return service.rejectNativeWriteBeforeDispatch(stream, invocation, write.GetPath(), cause)
		}
	}
	return service.rejectNativeWriteBeforeDispatch(stream, invocation, extractPathFromArgsJSON(invocation.ArgsJSON), cause)
}

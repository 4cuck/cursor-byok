package forwarder

import (
	"fmt"
	"strings"
	"time"

	"cursor/gen/agentv1"
	execbridge "cursor/internal/backend/agent/bridge/exec"
	runtimecore "cursor/internal/backend/agent/core"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type nativeExecSlot struct {
	id     uint32
	execID string
}

func rememberNativeExecSlot(stream *ActiveStream, callID string, original *agentv1.ExecServerMessage) {
	toolCallID := strings.TrimSpace(callID)
	if toolCallID == "" {
		return
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.nativeExecSlots == nil {
		stream.nativeExecSlots = map[string]nativeExecSlot{}
	}
	stream.nativeExecSlots[toolCallID] = nativeExecSlot{id: original.GetId(), execID: strings.TrimSpace(original.GetExecId())}
}

func nativeParentParameters(parameters []modeladapter.ModelParameter) []execbridge.NativeModelParameter {
	if len(parameters) == 0 {
		return nil
	}
	copied := make([]execbridge.NativeModelParameter, 0, len(parameters))
	for _, parameter := range parameters {
		copied = append(copied, execbridge.NativeModelParameter{ID: parameter.ID, Value: parameter.Value})
	}
	return copied
}

func nativeExecIsInternalSpillWrite(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	msg := &agentv1.ExecServerMessage{}
	if err := proto.Unmarshal(raw, msg); err != nil {
		return false
	}
	write := msg.GetWriteArgs()
	return write != nil && modeladapter.IsCursorInternalSpillWritePath(write.GetPath())
}

// dispatchNativeCursorExec sends the agent exec to the local Cursor client
// unchanged. Rebuilding it through our tool decoders drops IDE-valid shapes
// such as a Glob search represented as grep with no pattern.
func (service *Service) dispatchNativeCursorExec(stream *ActiveStream, invocation runtimecore.ToolInvocation) error {
	if stream == nil || service == nil || service.execBridge == nil {
		return nil
	}
	original := &agentv1.ExecServerMessage{}
	if err := proto.Unmarshal(invocation.NativeExec, original); err != nil {
		return err
	}
	// Keep the gateway's dispatch ID for card/history and exec correlation.
	rememberNativeExecSlot(stream, invocation.CallID, original)
	silentUI := invocation.SilentUI
	if write := original.GetWriteArgs(); write != nil {
		isSpill := modeladapter.IsCursorInternalSpillWritePath(write.GetPath())
		silentUI = silentUI || isSpill
		if !isSpill {
			if strings.TrimSpace(invocation.ToolName) != "Write" {
				err := fmt.Errorf("native Write does not match tool %q", invocation.ToolName)
				return service.rejectNativeWriteBeforeDispatch(stream, invocation, write.GetPath(), err)
			}
			if err := validateWritePath(write.GetPath()); err != nil {
				return service.rejectNativeWriteBeforeDispatch(stream, invocation, write.GetPath(), err)
			}
			// A leading space can make an otherwise absolute path relative on the
			// client. Never validate a trimmed path and forward a different one.
			if strings.TrimSpace(write.GetPath()) != write.GetPath() {
				return service.rejectNativeWriteBeforeDispatch(stream, invocation, write.GetPath(), fmt.Errorf("native write path has surrounding whitespace"))
			}
		} else if err := validateWritePath(write.GetPath()); err != nil {
			return service.rejectNativeWriteBeforeDispatch(stream, invocation, write.GetPath(), err)
		}
		argsJSON, err := nativeWriteDisplayArgs(write).MarshalJSON()
		if err != nil {
			return err
		}
		invocation.ArgsJSON = argsJSON
		if !silentUI && isCanvasFilePath(write.GetPath()) {
			// Managed canvases still need the proxy's edit and diagnostics lifecycle.
			return service.handleWriteToolInvocation(stream, invocation)
		}
	}
	// Cursor's cloud agent resolves Task model "auto" to composer before this
	// process sees the exec. The IDE then launches that child as Auto and never
	// consults the parent's Inherit from parent setting. Rewrite the forwarded
	// subagent model to the parent catalog model first.
	stream.mu.Lock()
	rootConversationID := strings.TrimSpace(stream.ConversationID)
	if stream.CheckpointConversation != nil && strings.TrimSpace(stream.CheckpointConversation.RootConversationID) != "" {
		rootConversationID = strings.TrimSpace(stream.CheckpointConversation.RootConversationID)
	}
	parentModel := execbridge.NativeSubagentParent{
		ModelID:            stream.ModelID,
		ThinkingEffort:     stream.ThinkingEffort,
		Fast:               stream.FastMode,
		Parameters:         nativeParentParameters(stream.ModelParameters),
		Overrides:          cloneSubagentModelOverrides(stream.SubagentModelOverrides),
		ConversationID:     strings.TrimSpace(stream.ConversationID),
		RootConversationID: rootConversationID,
		ToolCallID:         strings.TrimSpace(invocation.CallID),
	}
	displayModel := strings.TrimSpace(stream.ModelName)
	stream.mu.Unlock()
	if err := execbridge.RewriteNativeSubagentModel(original, parentModel); err != nil {
		return service.completePreDispatchToolError(stream, invocation, nil, false, false, err)
	}
	// The client keeps a shell's working directory and environment per
	// conversation id. Upstream names its own conversation, which changes when
	// a turn continues on a fresh one; this chat's shell must not change with it.
	execbridge.PinNativeShellConversation(original, parentModel.ConversationID)
	serverMessage, pendingExec, err := service.execBridge.OpenNativeExec(original, invocation)
	if err != nil {
		return service.completePreDispatchToolError(stream, invocation, nil, false, false, err)
	}
	pendingExec.ModelCallID = invocation.ModelCallID
	pendingExec.ReasoningContent = invocation.ReasoningContent
	pendingExec.ReasoningSignature = invocation.ReasoningSignature
	pendingExec.ReasoningSignatureSource = invocation.ReasoningSignatureSource
	pendingExec.SilentUI = silentUI
	pendingExec = initializePendingExecForTracking(pendingExec)
	if !silentUI {
		// What this call is answered with is kept: a turn resumed after a
		// lost stream is asked for it again (cursor_exec_replay.go).
		noteCursorExecCall(stream, invocation.CallID, invocation.ToolName)
	}
	var started *agentv1.ToolCall
	if !silentUI {
		started = buildStartedToolCall(invocation)
	}
	if started != nil {
		if subagent := original.GetSubagentArgs(); subagent != nil && strings.TrimSpace(subagent.GetModelId()) == strings.TrimSpace(parentModel.ModelID) && strings.TrimSpace(parentModel.ModelID) != "" {
			label := displayModel
			if label == "" {
				label = strings.TrimSpace(subagent.GetModelId())
			}
			ensureTaskToolCallModel(started, label)
		}
		if checkpointConversationInitialized(stream) {
			historyCard := started
			if write := original.GetWriteArgs(); write != nil {
				historyCard = buildStartedWriteHistoryToolCall(write.GetPath())
			}
			if toolCallPayload, marshalErr := protojson.Marshal(historyCard); marshalErr == nil {
				_, _ = service.appendConversationEntries(stream, stream.ConversationID, []HistoryEntry{
					newToolCallEntryWithProviderMetadata(stream.TurnSeq, stream.RequestID, invocation.CallID, invocation.ToolName, invocation.ReasoningContent, invocation.ReasoningSignature, invocation.ReasoningSignatureSource, invocation.ReasoningProviderItemID, invocation.ReasoningProviderStatus, invocation.ReasoningProviderSummary, invocation.ProviderItemID, invocation.ProviderCallID, invocation.ProviderStatus, toolCallPayload),
				})
			}
		}
	}
	stream.mu.Lock()
	pendingExec.ProviderPass = stream.ProviderPassCount
	if stream.PendingExecs == nil {
		stream.PendingExecs = map[string]runtimecore.PendingExec{}
	}
	stream.PendingExecs[pendingExec.ExecID] = pendingExec
	stream.UpdatedAt = time.Now().UTC()
	stream.mu.Unlock()
	isSubagent := strings.TrimSpace(pendingExec.ExecKind) == "subagent"
	if isSubagent {
		if !service.registerNativeDelegation(stream, pendingExec, serverMessage) {
			stream.mu.Lock()
			delete(stream.PendingExecs, pendingExec.ExecID)
			stream.mu.Unlock()
			return service.completePreDispatchToolError(stream, invocation, started, started != nil, false, errNativeDelegationConcurrencyLimit)
		}
		if checkpointConversationInitialized(stream) {
			_ = service.publishCheckpointForce(stream.RequestID, strings.TrimSpace(stream.ConversationID))
		}
	}
	service.scheduleExecWatchdog(stream.RequestID, pendingExec)
	if strings.TrimSpace(pendingExec.ExecKind) == "shell" {
		// Native account shells skip the generic watchdog. Without this timer a
		// shell that streamed output but never exited leaves the model on
		// "Planning next moves" with nothing to read.
		service.scheduleShellForegroundRecovery(stream.RequestID, pendingExec)
	}
	if started != nil {
		if err := service.broker.Publish(stream.RequestID, StreamEvent{
			Message: buildToolCallStartedMessage(invocation.CallID, invocation.ModelCallID, started),
		}); err != nil {
			stream.mu.Lock()
			delete(stream.PendingExecs, pendingExec.ExecID)
			stream.mu.Unlock()
			if isSubagent {
				service.releaseNativeDelegationQuietly(pendingExec.ExecID)
			}
			return err
		}
	}
	if err := service.broker.Publish(stream.RequestID, StreamEvent{Message: serverMessage}); err != nil {
		stream.mu.Lock()
		delete(stream.PendingExecs, pendingExec.ExecID)
		stream.mu.Unlock()
		if isSubagent {
			service.releaseNativeDelegationQuietly(pendingExec.ExecID)
		}
		return err
	}
	if silentUI {
		// A silent exec shows no card, adds no history and is not listed among
		// the pending tool calls, so a checkpoint would carry nothing new. A
		// wait sends several of these a second for as long as it blocks.
		return nil
	}
	if err := service.publishCheckpoint(stream.RequestID, strings.TrimSpace(stream.ConversationID)); err != nil {
		if isSubagent && !checkpointConversationInitialized(stream) {
			service.releaseNativeDelegationQuietly(pendingExec.ExecID)
		}
		return err
	}
	return nil
}

// rejectNativeWriteBeforeDispatch returns a typed WriteResult error to Cursor's
// open stream instead of dropping the exec or closing the RunSSE stream (which
// would abort concurrent in-flight WebFetch/WebSearch calls).
func (service *Service) rejectNativeWriteBeforeDispatch(stream *ActiveStream, invocation runtimecore.ToolInvocation, path string, cause error) error {
	if stream == nil || cause == nil {
		return cause
	}
	errMsg := &agentv1.ExecClientMessage{
		Message: &agentv1.ExecClientMessage_WriteResult{
			WriteResult: &agentv1.WriteResult{
				Result: &agentv1.WriteResult_Error{
					Error: &agentv1.WriteError{
						Path:  strings.TrimSpace(path),
						Error: strings.TrimSpace(cause.Error()),
					},
				},
			},
		},
	}
	if deliverCursorToolResult(stream, invocation.CallID, errMsg) {
		return nil
	}
	return service.completePreDispatchToolError(stream, invocation, nil, false, false, cause)
}

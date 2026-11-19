package forwarder

import (
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

func rememberNativeExecSlot(stream *ActiveStream, invocation runtimecore.ToolInvocation) {
	if stream == nil || len(invocation.NativeExec) == 0 {
		return
	}
	original := &agentv1.ExecServerMessage{}
	if err := proto.Unmarshal(invocation.NativeExec, original); err != nil {
		return
	}
	toolCallID := strings.TrimSpace(invocation.CallID)
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

func nativeExecIsWrite(raw []byte) bool {
	message := &agentv1.ExecServerMessage{}
	if err := proto.Unmarshal(raw, message); err != nil {
		return false
	}
	return message.GetWriteArgs() != nil
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
	serverMessage, pendingExec, err := service.execBridge.OpenNativeExec(original, invocation)
	if err != nil {
		return service.completePreDispatchToolError(stream, invocation, nil, false, false, err)
	}
	pendingExec.ModelCallID = invocation.ModelCallID
	pendingExec.ReasoningContent = invocation.ReasoningContent
	pendingExec.ReasoningSignature = invocation.ReasoningSignature
	pendingExec.ReasoningSignatureSource = invocation.ReasoningSignatureSource
	pendingExec = initializePendingExecForTracking(pendingExec)
	started := buildStartedToolCall(invocation)
	if started != nil {
		if subagent := original.GetSubagentArgs(); subagent != nil && strings.TrimSpace(subagent.GetModelId()) == strings.TrimSpace(parentModel.ModelID) && strings.TrimSpace(parentModel.ModelID) != "" {
			label := displayModel
			if label == "" {
				label = strings.TrimSpace(subagent.GetModelId())
			}
			ensureTaskToolCallModel(started, label)
		}
		if checkpointConversationInitialized(stream) {
			if toolCallPayload, marshalErr := protojson.Marshal(started); marshalErr == nil {
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
	if err := service.publishCheckpoint(stream.RequestID, strings.TrimSpace(stream.ConversationID)); err != nil {
		if isSubagent && !checkpointConversationInitialized(stream) {
			service.releaseNativeDelegationQuietly(pendingExec.ExecID)
		}
		return err
	}
	return nil
}

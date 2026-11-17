package forwarder

import (
	"strings"
	"time"

	"cursor/gen/agentv1"
	execbridge "cursor/internal/backend/agent/bridge/exec"
	runtimecore "cursor/internal/backend/agent/core"

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
	parentModel := execbridge.NativeSubagentParent{
		ModelID:        stream.ModelID,
		ThinkingEffort: stream.ThinkingEffort,
		Fast:           stream.FastMode,
		Overrides:      cloneSubagentModelOverrides(stream.SubagentModelOverrides),
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
	stream.mu.Lock()
	pendingExec.ProviderPass = stream.ProviderPassCount
	if stream.PendingExecs == nil {
		stream.PendingExecs = map[string]runtimecore.PendingExec{}
	}
	stream.PendingExecs[pendingExec.ExecID] = pendingExec
	stream.UpdatedAt = time.Now().UTC()
	stream.mu.Unlock()
	service.scheduleExecWatchdog(stream.RequestID, pendingExec)
	if started := buildStartedToolCall(invocation); started != nil {
		if subagent := original.GetSubagentArgs(); subagent != nil && strings.TrimSpace(subagent.GetModelId()) == strings.TrimSpace(parentModel.ModelID) && strings.TrimSpace(parentModel.ModelID) != "" {
			label := displayModel
			if label == "" {
				label = strings.TrimSpace(subagent.GetModelId())
			}
			ensureTaskToolCallModel(started, label)
		}
		if err := service.broker.Publish(stream.RequestID, StreamEvent{
			Message: buildToolCallStartedMessage(invocation.CallID, invocation.ModelCallID, started),
		}); err != nil {
			stream.mu.Lock()
			delete(stream.PendingExecs, pendingExec.ExecID)
			stream.mu.Unlock()
			return err
		}
	}
	if err := service.broker.Publish(stream.RequestID, StreamEvent{Message: serverMessage}); err != nil {
		stream.mu.Lock()
		delete(stream.PendingExecs, pendingExec.ExecID)
		stream.mu.Unlock()
		return err
	}
	return service.publishCheckpoint(stream.RequestID, strings.TrimSpace(stream.ConversationID))
}

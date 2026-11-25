package execbridge

import (
	"fmt"
	"strings"
	"time"

	"cursor/gen/agentv1"
	runtimecore "cursor/internal/backend/agent/core"
	"cursor/internal/modelchannel"

	"google.golang.org/protobuf/proto"
)

// NativeSubagentParent is the parent run's model selection. Cursor account
// execs arrive already resolved by the upstream agent, which turns the
// cost-aware "auto" choice into composer before the IDE launches the child.
// NativeModelParameter is one catalog parameter inherited by a subagent.
type NativeModelParameter struct {
	ID    string
	Value string
}

type NativeSubagentParent struct {
	ModelID            string
	ThinkingEffort     string
	Fast               bool
	Parameters         []NativeModelParameter
	Overrides          map[string]runtimecore.SubagentModelOverrideSelection
	ConversationID     string
	RootConversationID string
	ToolCallID         string
}

// RewriteNativeSubagentModel applies the parent Task model selection to a
// Cursor-account subagent exec. Inherit from parent replaces the upstream
// auto/composer choice with the parent catalog model and its effort/fast
// parameters. A literal auto/default/fast alias inherits the parent even when
// no override was stored. Other subagent types keep an explicit upstream model.
func RewriteNativeSubagentModel(exec *agentv1.ExecServerMessage, parent NativeSubagentParent) error {
	args := exec.GetSubagentArgs()
	if exec == nil || args == nil {
		return nil
	}
	if parentConvID := strings.TrimSpace(parent.ConversationID); parentConvID != "" {
		args.ParentConversationId = &parentConvID
		rootConvID := strings.TrimSpace(parent.RootConversationID)
		if rootConvID == "" {
			rootConvID = parentConvID
		}
		args.RootParentConversationId = &rootConvID
	}
	if strings.TrimSpace(args.GetToolCallId()) == "" && strings.TrimSpace(parent.ToolCallID) != "" {
		args.ToolCallId = strings.TrimSpace(parent.ToolCallID)
	}
	subagentType := strings.TrimSpace(args.GetSubagentType())
	current := strings.TrimSpace(args.GetModelId())
	parentModelID := strings.TrimSpace(parent.ModelID)
	modelID := current
	if modelchannel.IsMetaModelAlias(modelID) || strings.EqualFold(modelID, "inherit") {
		modelID = parentModelID
	}
	if override, _, ok := runtimecore.LookupSubagentModelOverride(parent.Overrides, subagentType); ok {
		switch strings.TrimSpace(override.Selection) {
		case "disabled":
			return fmt.Errorf("subagent type %q is disabled by model override", subagentType)
		case "model":
			if strings.TrimSpace(override.ModelID) != "" {
				modelID = strings.TrimSpace(override.ModelID)
			}
		case "inherit":
			modelID = parentModelID
		}
	}
	if modelID == "" {
		modelID = parentModelID
	}
	if modelID == "" || (modelID == current && modelID != parentModelID) {
		return nil
	}
	args.ModelId = modelID
	if modelID != parentModelID {
		args.ModelParameters = nil
		return nil
	}
	if parameters := nativeParentModelParameters(parent); len(parameters) > 0 || modelID != current {
		args.ModelParameters = parameters
	}
	return nil
}

func nativeParentModelParameters(parent NativeSubagentParent) []*agentv1.RequestedModel_ModelParameterValue {
	if inherited := nativeInheritedParameters(parent.Parameters); len(inherited) > 0 {
		return inherited
	}
	var parameters []*agentv1.RequestedModel_ModelParameterValue
	if effort := strings.ToLower(strings.TrimSpace(parent.ThinkingEffort)); effort != "" && effort != "disabled" {
		parameters = append(parameters, &agentv1.RequestedModel_ModelParameterValue{Id: "effort", Value: effort})
	}
	if parent.Fast {
		parameters = append(parameters, &agentv1.RequestedModel_ModelParameterValue{Id: "fast", Value: "true"})
	}
	return parameters
}

func nativeInheritedParameters(parameters []NativeModelParameter) []*agentv1.RequestedModel_ModelParameterValue {
	if len(parameters) == 0 {
		return nil
	}
	inherited := make([]*agentv1.RequestedModel_ModelParameterValue, 0, len(parameters))
	for _, parameter := range parameters {
		id := strings.TrimSpace(parameter.ID)
		value := strings.TrimSpace(parameter.Value)
		if id == "" || value == "" {
			continue
		}
		inherited = append(inherited, &agentv1.RequestedModel_ModelParameterValue{Id: id, Value: value})
	}
	return inherited
}

const nativeExecIDPrefix = "exec-native-"

// OpenNativeExec forwards a Cursor agent exec to the local client without
// rebuilding it through our tool-argument decoders. The installed IDE client
// already knows how to run every exec shape, including Glob sent as a
// patternless files_with_matches grep.
func (bridge *Bridge) OpenNativeExec(exec *agentv1.ExecServerMessage, toolCall runtimecore.ToolInvocation) (*agentv1.AgentServerMessage, runtimecore.PendingExec, error) {
	if bridge == nil || exec == nil || exec.GetMessage() == nil {
		return nil, runtimecore.PendingExec{}, fmt.Errorf("native exec is required")
	}
	cloned, ok := proto.Clone(exec).(*agentv1.ExecServerMessage)
	if !ok || cloned == nil {
		return nil, runtimecore.PendingExec{}, fmt.Errorf("native exec clone failed")
	}
	cloned.Id = bridge.nextID()
	cloned.ExecId = NewExecID(nativeExecIDPrefix)
	return &agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_ExecServerMessage{ExecServerMessage: cloned},
	}, runtimecore.PendingExec{
		MessageID:   cloned.GetId(),
		ExecID:      cloned.GetExecId(),
		ArgsJSON:    append([]byte(nil), toolCall.ArgsJSON...),
		ToolCallID:  strings.TrimSpace(toolCall.CallID),
		ExecKind:    nativeExecKind(cloned),
		StreamState: "opened",
		OpenedAt:    time.Now().UTC(),
	}, nil
}

func nativeExecKind(exec *agentv1.ExecServerMessage) string {
	if exec == nil {
		return "native"
	}
	switch {
	case exec.GetReadArgs() != nil || exec.GetRedactedReadArgs() != nil:
		return "read"
	case exec.GetWriteArgs() != nil:
		return "write"
	case exec.GetDeleteArgs() != nil:
		return "delete"
	case exec.GetGrepArgs() != nil:
		args := exec.GetGrepArgs()
		mode := strings.TrimSpace(args.GetOutputMode())
		if strings.TrimSpace(args.GetPattern()) == "" && strings.TrimSpace(args.GetGlob()) != "" && (mode == "" || mode == "files_with_matches") {
			return "glob"
		}
		return "grep"
	case exec.GetLsArgs() != nil:
		return "ls"
	case exec.GetDiagnosticsArgs() != nil:
		return "diagnostics"
	case exec.GetCanvasDiagnosticsArgs() != nil:
		return "canvas_diagnostics"
	case exec.GetGitDiffRequest() != nil:
		return "git_diff"
	case exec.GetMcpArgs() != nil:
		return "mcp"
	case exec.GetListMcpResourcesExecArgs() != nil:
		return "list_mcp_resources"
	case exec.GetReadMcpResourceExecArgs() != nil:
		return "read_mcp_resource"
	case exec.GetMcpStateExecArgs() != nil:
		return "mcp_state"
	case exec.GetConversationSearchArgs() != nil:
		return "conversation_search"
	case exec.GetFetchArgs() != nil:
		return "fetch"
	case exec.GetRecordScreenArgs() != nil:
		return "record_screen"
	case exec.GetComputerUseArgs() != nil:
		return "computer_use"
	case exec.GetShellArgs() != nil || exec.GetShellStreamArgs() != nil || exec.GetMiniSweAgentBashArgs() != nil:
		return "shell"
	case exec.GetBackgroundShellSpawnArgs() != nil:
		return "background_shell_spawn"
	case exec.GetWriteShellStdinArgs() != nil:
		return "write_shell_stdin"
	case exec.GetForceBackgroundShellArgs() != nil:
		return "force_background_shell"
	case exec.GetSubagentArgs() != nil:
		return "subagent"
	case exec.GetForceBackgroundSubagentArgs() != nil:
		return "force_background_subagent"
	case exec.GetSubagentAwaitArgs() != nil:
		return "subagent_await"
	case exec.GetExecuteHookArgs() != nil:
		return "execute_hook"
	default:
		return "native"
	}
}

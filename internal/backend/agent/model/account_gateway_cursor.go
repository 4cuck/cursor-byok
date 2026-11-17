package modeladapter

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cursor/gen/agentv1"
	"cursor/gen/aiserverv1"
	runtimecore "cursor/internal/backend/agent/core"
	"cursor/internal/backend/server/upstream"
	"cursor/internal/logger"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	cursorAccountBackendRootDefault = "https://api2.cursor.sh"
	cursorAccountAgentHostDefault   = "https://agentn.global.api5.cursor.sh"
	cursorAccountBidiAppendPath     = "/aiserver.v1.BidiService/BidiAppend"
	cursorAccountServerConfigPath   = "/aiserver.v1.ServerConfigService/GetServerConfig"
	cursorAccountRunPath            = "/agent.v1.AgentService/RunSSE"
)

// cursorAccountBackendRoot is api2. Tests point it at an httptest server.
var cursorAccountBackendRoot = cursorAccountBackendRootDefault

var cursorAccountHTTPClient = &http.Client{}

type cursorAccountGateway struct {
	auth CursorAccountAuthorizer
}

// OpenCursorAccountGateway returns the live Cursor agent relay.
func OpenCursorAccountGateway(auth CursorAccountAuthorizer) AccountModelGateway {
	if auth == nil {
		return unavailableAccountModelGateway{}
	}
	return newCursorAccountGateway(auth)
}

func newCursorAccountGateway(auth CursorAccountAuthorizer) *cursorAccountGateway {
	return &cursorAccountGateway{auth: auth}
}

func (gateway *cursorAccountGateway) Stream(ctx context.Context, req StreamRequest, sink func(ModelEvent) error) error {
	if gateway == nil || gateway.auth == nil {
		return ErrCursorAccountModelGatewayUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	modelID := strings.TrimSpace(req.ProviderModelID)
	if modelID == "" {
		modelID = strings.TrimSpace(req.ModelID)
	}
	if modelID == "" {
		return fmt.Errorf("cursor account model id is empty")
	}
	accountID := usableCursorAccountID(gateway.auth, req.CursorAccountID)
	authorization, err := gateway.authorize(ctx, accountID)
	if err != nil {
		return fmt.Errorf("cursor account is not signed in: %w", err)
	}
	err = gateway.streamAuthorized(ctx, req, authorization, sink)
	if err == nil {
		return nil
	}
	if cursorAccountAuthRejected(err) && cursorAccountUsesAPIKey(gateway.auth, accountID) {
		if renewed, refreshErr := cursorAccountForceRefresh(ctx, gateway.auth, accountID); refreshErr == nil {
			if retryErr := gateway.streamAuthorized(ctx, req, renewed, sink); retryErr == nil {
				return nil
			} else {
				err = retryErr
			}
		}
	}
	if cursorAccountDeviceLimited(err) {
		cursorAccountMarkLocked(gateway.auth, accountID)
		if alt := unlockedCurrentCursorAccount(gateway.auth, accountID); alt != "" {
			logger.Infof("cursor account model skipped device-locked account=%s using current=%s", accountID, alt)
			if altAuth, altErr := gateway.authorize(ctx, alt); altErr == nil {
				return gateway.streamAuthorized(ctx, req, altAuth, sink)
			}
		}
	}
	return err
}

func (gateway *cursorAccountGateway) streamAuthorized(ctx context.Context, req StreamRequest, authorization string, sink func(ModelEvent) error) error {
	modelID := strings.TrimSpace(req.ProviderModelID)
	if modelID == "" {
		modelID = strings.TrimSpace(req.ModelID)
	}
	host := cursorAccountAgentHost(ctx, authorization)
	requestID := uuid.NewString()
	run := cursorAccountRunMessage(req, modelID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(host, "/")+cursorAccountRunPath,
		bytes.NewReader(cursorAccountFrame(mustCursorProto(&aiserverv1.BidiRequestId{RequestId: requestID}))))
	if err != nil {
		return err
	}
	applyCursorAccountHeaders(httpReq, authorization, "application/connect+proto")
	resp, err := cursorAccountHTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("cursor agent stream returned HTTP %d: %s", resp.StatusCode, cursorAccountErrorText(raw))
	}

	appendErr := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		appendErr <- appendCursorAccount(ctx, authorization, requestID, 0, mustCursorProto(run))
	}()

	return readCursorAccountStream(ctx, authorization, requestID, modelID, cursorAccountEventLabel(req), req.SilentTimeout, resp.Body, appendErr, sink)
}

func (gateway *cursorAccountGateway) authorize(ctx context.Context, accountID string) (string, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID != "" {
		if owner, ok := gateway.auth.(interface {
			AuthorizationFor(context.Context, string) (string, error)
		}); ok {
			return owner.AuthorizationFor(ctx, accountID)
		}
	}
	return gateway.auth.Authorization(ctx)
}

// cursorAccountDirectory is the optional lock and API-key renewal surface.
// The live account manager implements it. Tests that only stub Authorization do not.
type cursorAccountDirectory interface {
	AccountIsLocked(accountID string) bool
	CurrentAccountID() string
	UsesAPIKey(accountID string) bool
	ForceRefresh(ctx context.Context, accountID string) (string, error)
	MarkDeviceLocked(accountID string) error
}

// usableCursorAccountID avoids a device-locked account when the current account
// can still serve the call. Synced models stay pinned to the account that
// created them, so selecting a different current account would otherwise keep
// sending traffic to the locked one.
func usableCursorAccountID(auth CursorAccountAuthorizer, requested string) string {
	requested = strings.TrimSpace(requested)
	directory, ok := auth.(cursorAccountDirectory)
	if !ok {
		return requested
	}
	if requested != "" && !directory.AccountIsLocked(requested) {
		return requested
	}
	current := strings.TrimSpace(directory.CurrentAccountID())
	if current != "" && current != requested && !directory.AccountIsLocked(current) {
		if requested != "" {
			logger.Infof("cursor account model using current account=%s instead of locked account=%s", current, requested)
		}
		return current
	}
	if requested == "" {
		return current
	}
	return requested
}

func unlockedCurrentCursorAccount(auth CursorAccountAuthorizer, used string) string {
	directory, ok := auth.(cursorAccountDirectory)
	if !ok {
		return ""
	}
	current := strings.TrimSpace(directory.CurrentAccountID())
	if current == "" || current == strings.TrimSpace(used) || directory.AccountIsLocked(current) {
		return ""
	}
	return current
}

func cursorAccountUsesAPIKey(auth CursorAccountAuthorizer, accountID string) bool {
	directory, ok := auth.(cursorAccountDirectory)
	return ok && directory.UsesAPIKey(accountID)
}

func cursorAccountForceRefresh(ctx context.Context, auth CursorAccountAuthorizer, accountID string) (string, error) {
	directory, ok := auth.(cursorAccountDirectory)
	if !ok {
		return "", fmt.Errorf("cursor account cannot refresh")
	}
	return directory.ForceRefresh(ctx, accountID)
}

func cursorAccountMarkLocked(auth CursorAccountAuthorizer, accountID string) {
	directory, ok := auth.(cursorAccountDirectory)
	if !ok || strings.TrimSpace(accountID) == "" {
		return
	}
	if err := directory.MarkDeviceLocked(accountID); err != nil {
		logger.Warnf("mark cursor account device locked failed account=%s err=%v", accountID, err)
	}
}

func cursorAccountAuthRejected(err error) bool {
	text := strings.ToLower(errString(err))
	return strings.Contains(text, "not logged in") ||
		strings.Contains(text, "not_logged_in") ||
		strings.Contains(text, "unauthenticated") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "session expired") ||
		strings.Contains(text, "session is no longer valid")
}

func cursorAccountDeviceLimited(err error) bool {
	return upstream.IsDeviceLimitError(errString(err))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func cursorAccountEventLabel(req StreamRequest) string {
	label := strings.TrimSpace(req.GroupName)
	if label == "cursor_account" || (strings.HasPrefix(label, "cursor_account") && cursorAccountSlotSuffix(label)) {
		return label
	}
	return "cursor_account"
}

func cursorAccountSlotSuffix(label string) bool {
	rest := strings.TrimPrefix(label, "cursor_account")
	if rest == "" || rest == label {
		return label == "cursor_account"
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func readCursorAccountStream(ctx context.Context, authorization, requestID, modelID, providerLabel string, silentTimeout time.Duration, body io.Reader, appendErr <-chan error, sink func(ModelEvent) error) error {
	var pending []byte
	tmp := make([]byte, 8192)
	seq := int64(1)
	thinkingActive := false
	var thinkingStarted time.Time
	toolSeen := map[string]struct{}{}
	var inputTokens, outputTokens, cacheRead, cacheWrite, reasoningTokens int64
	usagePresent := false
	finishReason := "stop"

	finishThinking := func() error {
		if !thinkingActive {
			return nil
		}
		duration := int32(time.Since(thinkingStarted).Milliseconds())
		if duration < 0 {
			duration = 0
		}
		thinkingActive = false
		return sink(ModelEvent{
			Kind:               ModelEventKindThinkingCompleted,
			OccurredAt:         time.Now().UTC(),
			Provider:           providerLabel,
			GroupName:          providerLabel,
			Model:              modelID,
			ThinkingDurationMS: duration,
		})
	}
	finishTurn := func() error {
		if err := finishThinking(); err != nil {
			return err
		}
		return sink(ModelEvent{
			Kind:             ModelEventKindTurnFinished,
			OccurredAt:       time.Now().UTC(),
			Provider:         providerLabel,
			GroupName:        providerLabel,
			Model:            modelID,
			InputTokens:      inputTokens,
			OutputTokens:     outputTokens,
			ReasoningTokens:  reasoningTokens,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
			UsagePresent:     usagePresent,
			FinishReason:     finishReason,
		})
	}

	readCh := make(chan readResult, 1)
	reading := false
	silent := time.NewTimer(time.Hour)
	if !silent.Stop() {
		select {
		case <-silent.C:
		default:
		}
	}
	if silentTimeout > 0 {
		silent.Reset(silentTimeout)
	}
	defer silent.Stop()
	sawUseful := false
	armSilent := func() {
		if silentTimeout <= 0 || sawUseful {
			return
		}
		if !silent.Stop() {
			select {
			case <-silent.C:
			default:
			}
		}
		silent.Reset(silentTimeout)
	}
	noteUseful := func() {
		sawUseful = true
		if !silent.Stop() {
			select {
			case <-silent.C:
			default:
			}
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !reading {
			reading = true
			go func() {
				n, err := body.Read(tmp)
				readCh <- readResult{n: n, err: err}
			}()
		}
		var n int
		var readErr error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-appendErr:
			appendErr = nil
			if err != nil {
				return fmt.Errorf("cursor refused the model call: %s", err.Error())
			}
			armSilent()
			continue
		case <-silent.C:
			if silentTimeout > 0 && !sawUseful {
				return fmt.Errorf("this account does not have access to %s", modelID)
			}
			continue
		case result := <-readCh:
			reading = false
			n = result.n
			readErr = result.err
		}
		if n > 0 {
			pending = append(pending, tmp[:n]...)
		}
		for len(pending) >= 5 {
			size := int(binary.BigEndian.Uint32(pending[1:5]))
			if size > 8<<20 {
				return fmt.Errorf("cursor agent frame is too large")
			}
			if len(pending) < 5+size {
				break
			}
			flags := pending[0]
			payload := append([]byte(nil), pending[5:5+size]...)
			pending = pending[5+size:]
			if denial := cursorAccountAccessDenial(payload); denial != "" {
				return fmt.Errorf("this account does not have access to %s: %s", modelID, denial)
			}
			if flags&0x02 != 0 {
				if text := cursorAccountErrorText(payload); text != "" {
					return fmt.Errorf("cursor refused the model call: %s", text)
				}
				return finishTurn()
			}
			msg := &agentv1.AgentServerMessage{}
			if err := proto.Unmarshal(payload, msg); err != nil {
				if text := cursorAccountErrorText(payload); text != "" {
					return fmt.Errorf("cursor refused the model call: %s", text)
				}
				continue
			}
			if exec := msg.GetExecServerMessage(); exec != nil {
				if exec.GetRequestContextArgs() != nil {
					if err := appendCursorAccount(ctx, authorization, requestID, seq, mustCursorProto(cursorAccountContextReply(exec.GetId(), exec.GetExecId()))); err != nil {
						return err
					}
					seq++
					continue
				}
				if reply, ok := cursorAccountExecReply(exec); ok {
					if err := appendCursorAccount(ctx, authorization, requestID, seq, mustCursorProto(reply)); err != nil {
						return err
					}
					seq++
					continue
				}
				name, callID, args, ok := cursorAccountExecTool(exec)
				if !ok {
					return fmt.Errorf("cursor requested an unsupported tool execution")
				}
				if _, seen := toolSeen[callID]; seen {
					continue
				}
				toolSeen[callID] = struct{}{}
				if err := finishThinking(); err != nil {
					return err
				}
				if err := sink(ModelEvent{
					Kind:       ModelEventKindToolLikeCompleted,
					OccurredAt: time.Now().UTC(),
					Provider:   providerLabel,
					GroupName:  providerLabel,
					Model:      modelID,
					ToolCallID: callID,
					ToolInvocation: &runtimecore.ToolInvocation{
						CallID:      callID,
						ToolName:    name,
						ArgsJSON:    args,
						ModelCallID: requestID,
					},
				}); err != nil {
					return err
				}
				finishReason = "tool_calls"
				return finishTurn()
			}
			update := msg.GetInteractionUpdate()
			if update == nil {
				continue
			}
			if text := update.GetTextDelta(); text != nil && text.GetText() != "" {
				noteUseful()
				if err := sink(ModelEvent{
					Kind:       ModelEventKindTextDelta,
					OccurredAt: time.Now().UTC(),
					Provider:   providerLabel,
					GroupName:  providerLabel,
					Model:      modelID,
					Text:       text.GetText(),
				}); err != nil {
					return err
				}
			}
			if thinking := update.GetThinkingDelta(); thinking != nil && thinking.GetText() != "" {
				noteUseful()
				if !thinkingActive {
					thinkingActive = true
					thinkingStarted = time.Now()
				}
				if err := sink(ModelEvent{
					Kind:          ModelEventKindThinkingDelta,
					OccurredAt:    time.Now().UTC(),
					Provider:      providerLabel,
					GroupName:     providerLabel,
					Model:         modelID,
					Text:          thinking.GetText(),
					ThinkingStyle: thinking.GetThinkingStyle(),
				}); err != nil {
					return err
				}
			}
			if ended := update.GetTurnEnded(); ended != nil {
				inputTokens = ended.GetInputTokens()
				outputTokens = ended.GetOutputTokens()
				cacheRead = ended.GetCacheReadTokens()
				cacheWrite = ended.GetCacheWriteTokens()
				reasoningTokens = ended.GetReasoningTokens()
				usagePresent = true
				return finishTurn()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return finishTurn()
			}
			return readErr
		}
	}
}

func cursorAccountRunMessage(req StreamRequest, modelID string) *agentv1.AgentClientMessage {
	mode := req.Mode
	if mode == agentv1.AgentMode_AGENT_MODE_UNSPECIFIED {
		mode = agentv1.AgentMode_AGENT_MODE_AGENT
	}
	conversationID := strings.TrimSpace(req.ConversationID)
	if conversationID == "" {
		conversationID = uuid.NewString()
	}
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{
			RunRequest: &agentv1.AgentRunRequest{
				ConversationState: &agentv1.ConversationStateStructure{Mode: mode.Enum()},
				Action: &agentv1.ConversationAction{
					Action: &agentv1.ConversationAction_UserMessageAction{
						UserMessageAction: &agentv1.UserMessageAction{
							UserMessage: &agentv1.UserMessage{
								Text:      cursorAccountPrompt(req.Messages),
								MessageId: uuid.NewString(),
								Mode:      mode,
							},
						},
					},
				},
				RequestedModel: &agentv1.RequestedModel{ModelId: modelID},
				ConversationId: proto.String(conversationID),
			},
		},
	}
}

func cursorAccountPrompt(messages []Message) string {
	if len(messages) == 0 {
		return "Continue."
	}
	if len(messages) == 1 {
		text := strings.TrimSpace(messages[0].Content)
		if text == "" {
			return "Continue."
		}
		return text
	}
	var builder strings.Builder
	for index, message := range messages {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		builder.WriteString("[")
		builder.WriteString(role)
		builder.WriteString("]")
		if name := strings.TrimSpace(message.Name); name != "" {
			builder.WriteString(" ")
			builder.WriteString(name)
		}
		if id := strings.TrimSpace(message.ToolCallID); id != "" {
			builder.WriteString(" ")
			builder.WriteString(id)
		}
		builder.WriteString("\n")
		builder.WriteString(message.Content)
		if message.ReasoningContent != "" && role == "assistant" {
			builder.WriteString("\n")
			builder.WriteString(message.ReasoningContent)
		}
	}
	return builder.String()
}

func cursorAccountContextReply(id uint32, execID string) *agentv1.AgentClientMessage {
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{
			ExecClientMessage: &agentv1.ExecClientMessage{
				Id:     id,
				ExecId: execID,
				Message: &agentv1.ExecClientMessage_RequestContextResult{
					RequestContextResult: &agentv1.RequestContextResult{
						Result: &agentv1.RequestContextResult_Success{
							Success: &agentv1.RequestContextSuccess{
								RequestContext: &agentv1.RequestContext{
									Env: &agentv1.RequestContextEnv{
										OsVersion: "windows",
										Shell:     "powershell",
										TimeZone:  "UTC",
									},
									EnvInfoComplete:             proto.Bool(true),
									RulesInfoComplete:           proto.Bool(true),
									RepositoryInfoComplete:      proto.Bool(true),
									McpInfoComplete:             proto.Bool(true),
									AgentSkillsInfoComplete:     proto.Bool(true),
									GitStatusInfoComplete:       proto.Bool(true),
									CustomSubagentsInfoComplete: proto.Bool(true),
								},
							},
						},
					},
				},
			},
		},
	}
}

func cursorAccountExecReply(exec *agentv1.ExecServerMessage) (*agentv1.AgentClientMessage, bool) {
	client := &agentv1.ExecClientMessage{Id: exec.GetId(), ExecId: exec.GetExecId()}
	switch {
	case exec.GetSmartModeClassifierArgs() != nil:
		client.Message = &agentv1.ExecClientMessage_SmartModeClassifierResult{
			SmartModeClassifierResult: &agentv1.SmartModeClassifierResult{
				Result: &agentv1.SmartModeClassifierResult_Success{
					Success: &agentv1.SmartModeClassifierSuccess{
						Decision: agentv1.SmartModeClassifierDecision_SMART_MODE_CLASSIFIER_DECISION_ALLOW,
					},
				},
			},
		}
	case exec.GetShellAllowlistPrecheckArgs() != nil:
		client.Message = &agentv1.ExecClientMessage_ShellAllowlistPrecheckResult{
			ShellAllowlistPrecheckResult: &agentv1.ShellAllowlistPrecheckResult{Allowlisted: true},
		}
	case exec.GetWebFetchAllowlistPrecheckArgs() != nil:
		client.Message = &agentv1.ExecClientMessage_WebFetchAllowlistPrecheckResult{
			WebFetchAllowlistPrecheckResult: &agentv1.WebFetchAllowlistPrecheckResult{Allowlisted: true},
		}
	default:
		return nil, false
	}
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: client},
	}, true
}

func cursorAccountExecTool(exec *agentv1.ExecServerMessage) (string, string, []byte, bool) {
	callID := strings.TrimSpace(exec.GetExecId())
	if callID == "" {
		callID = fmt.Sprintf("exec-%d", exec.GetId())
	}
	var (
		name string
		msg  proto.Message
	)
	switch {
	case exec.GetReadArgs() != nil || exec.GetRedactedReadArgs() != nil:
		name = "Read"
		if args := exec.GetReadArgs(); args != nil {
			msg = args
			if id := strings.TrimSpace(args.GetToolCallId()); id != "" {
				callID = id
			}
		} else {
			msg = exec.GetRedactedReadArgs()
			if id := strings.TrimSpace(exec.GetRedactedReadArgs().GetToolCallId()); id != "" {
				callID = id
			}
		}
	case exec.GetWriteArgs() != nil:
		name, msg = "Write", exec.GetWriteArgs()
	case exec.GetDeleteArgs() != nil:
		name, msg = "Delete", exec.GetDeleteArgs()
	case exec.GetGrepArgs() != nil:
		name, msg = "Grep", exec.GetGrepArgs()
		if id := strings.TrimSpace(exec.GetGrepArgs().GetToolCallId()); id != "" {
			callID = id
		}
	case exec.GetShellArgs() != nil || exec.GetShellStreamArgs() != nil:
		name = "Shell"
		if args := exec.GetShellArgs(); args != nil {
			msg = args
			if id := strings.TrimSpace(args.GetToolCallId()); id != "" {
				callID = id
			}
		} else {
			msg = exec.GetShellStreamArgs()
			if id := strings.TrimSpace(exec.GetShellStreamArgs().GetToolCallId()); id != "" {
				callID = id
			}
		}
	case exec.GetLsArgs() != nil:
		name, msg = "Ls", exec.GetLsArgs()
	case exec.GetDiagnosticsArgs() != nil:
		name, msg = "ReadLints", exec.GetDiagnosticsArgs()
	case exec.GetFetchArgs() != nil:
		name, msg = "Fetch", exec.GetFetchArgs()
	default:
		return "", "", nil, false
	}
	raw, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(msg)
	if err != nil {
		return "", "", nil, false
	}
	return name, callID, raw, true
}

func cursorAccountAgentHost(ctx context.Context, authorization string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cursorAccountBackendRoot, "/")+cursorAccountServerConfigPath,
		bytes.NewReader(mustCursorProto(&aiserverv1.GetServerConfigRequest{})))
	if err != nil {
		return cursorAccountAgentHostDefault
	}
	applyCursorAccountHeaders(req, authorization, "application/proto")
	resp, err := cursorAccountHTTPClient.Do(req)
	if err != nil {
		return cursorAccountAgentHostDefault
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cursorAccountAgentHostDefault
	}
	cfg := &aiserverv1.GetServerConfigResponse{}
	if err := proto.Unmarshal(raw, cfg); err != nil {
		return cursorAccountAgentHostDefault
	}
	host := strings.TrimSpace(cfg.GetAgentUrlConfig().GetAgentnUrl())
	if host == "" {
		host = strings.TrimSpace(cfg.GetAgentUrlConfig().GetAgentUrl())
	}
	if host == "" {
		return cursorAccountAgentHostDefault
	}
	return strings.TrimRight(host, "/")
}

func appendCursorAccount(ctx context.Context, authorization, requestID string, seq int64, payload []byte) error {
	body := mustCursorProto(&aiserverv1.BidiAppendRequest{
		Data:        hex.EncodeToString(payload),
		RequestId:   &aiserverv1.BidiRequestId{RequestId: requestID},
		AppendSeqno: seq,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cursorAccountBackendRoot, "/")+cursorAccountBidiAppendPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	applyCursorAccountHeaders(req, authorization, "application/proto")
	resp, err := cursorAccountHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cursor append returned HTTP %d: %s", resp.StatusCode, cursorAccountErrorText(raw))
	}
	return nil
}

func applyCursorAccountHeaders(req *http.Request, authorization, contentType string) {
	req.Header.Set("authorization", authorization)
	req.Header.Set("x-cursor-checksum", upstream.BuildCursorChecksum(authorization))
	req.Header.Set("x-cursor-client-type", "ide")
	req.Header.Set("x-cursor-client-version", upstream.CursorClientVersion())
	req.Header.Set("x-cursor-client-os", "win32")
	req.Header.Set("x-cursor-client-arch", "x64")
	req.Header.Set("x-cursor-client-device-type", "desktop")
	req.Header.Set("x-ghost-mode", "false")
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("content-type", contentType)
}

func cursorAccountFrame(payload []byte) []byte {
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	return append(header, payload...)
}

func mustCursorProto(msg proto.Message) []byte {
	raw, err := proto.Marshal(msg)
	if err != nil {
		panic(err)
	}
	return raw
}

type readResult struct {
	n   int
	err error
}

func cursorAccountAccessDenial(raw []byte) string {
	text := strings.ToLower(string(raw))
	markers := []string{
		"does not have access",
		"do not have access",
		"don't have access",
		"model is not available",
		"model isn't available",
		"model not available",
		"not included in your plan",
		"not available on your plan",
		"upgrade your plan",
		"requires a paid",
		"usage pricing required",
	}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			if detail := cursorAccountErrorText(raw); detail != "" {
				return detail
			}
			return marker
		}
	}
	return ""
}

func cursorAccountErrorText(raw []byte) string {
	text := string(raw)
	if detail := quotedJSONField(text, `"detail":"`); detail != "" {
		return detail
	}
	if title := quotedJSONField(text, `"title":"`); title != "" {
		return title
	}
	if code := quotedJSONField(text, `"error":"`); code != "" {
		return code
	}
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 300 {
		return trimmed[:300]
	}
	return trimmed
}

func quotedJSONField(text, marker string) string {
	index := strings.Index(text, marker)
	if index < 0 {
		return ""
	}
	rest := text[index+len(marker):]
	if end := strings.Index(rest, `"`); end >= 0 {
		return rest[:end]
	}
	return ""
}

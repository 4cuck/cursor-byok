package modeladapter

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	if env := run.GetRunRequest().GetAction().GetUserMessageAction().GetRequestContext().GetEnv(); env != nil {
		logger.Infof("cursor account environment os=%s shell=%s workspaces=%d", env.GetOsVersion(), env.GetShell(), len(env.GetWorkspacePaths()))
	}
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

	return readCursorAccountStream(ctx, authorization, requestID, strings.TrimSpace(req.ConversationID), modelID, cursorAccountEventLabel(req), req.SilentTimeout, resp.Body, appendErr, req.CursorToolResults, cursorAccountRequestContext(req), sink)
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

type cursorExecSlot struct {
	id     uint32
	execID string
}

func readCursorAccountStream(ctx context.Context, authorization, requestID, conversationID, modelID, providerLabel string, silentTimeout time.Duration, body io.Reader, appendErr <-chan error, toolResults <-chan *agentv1.ExecClientMessage, requestContext *agentv1.RequestContext, sink func(ModelEvent) error) error {
	cursorAccountResetFrameLog()
	var pending []byte
	tmp := make([]byte, 8192)
	var seqMu sync.Mutex
	seq := int64(1)
	nextSeq := func() int64 {
		seqMu.Lock()
		defer seqMu.Unlock()
		n := seq
		seq++
		return n
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var finishInline <-chan struct{}
	var toolContinued <-chan struct{}
	var pendingExecs chan cursorExecSlot
	if toolResults != nil {
		signal := make(chan struct{}, 1)
		continued := make(chan struct{}, 8)
		finishInline = signal
		toolContinued = continued
		pendingExecs = make(chan cursorExecSlot, 8)
		go serveCursorAccountToolResults(streamCtx, authorization, requestID, nextSeq, toolResults, pendingExecs, signal, continued)
	}
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
	toolSilence := time.NewTimer(time.Hour)
	if !toolSilence.Stop() {
		select {
		case <-toolSilence.C:
		default:
		}
	}
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
		if !toolSilence.Stop() {
			select {
			case <-toolSilence.C:
			default:
			}
		}
	}
	defer toolSilence.Stop()

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
		case <-finishInline:
			finishReason = "tool_calls"
			return finishTurn()
		case <-toolContinued:
			if !toolSilence.Stop() {
				select {
				case <-toolSilence.C:
				default:
				}
			}
			toolSilence.Reset(cursorAccountToolStallTimeout)
		case <-toolSilence.C:
			logger.Infof("cursor account stream stalled after a tool result request_id=%s model=%s; continuing in a fresh pass", requestID, modelID)
			finishReason = "tool_calls"
			return finishTurn()
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
			if flags&0x01 != 0 {
				decoded, err := cursorAccountGunzip(payload)
				if err != nil {
					if flags&0x02 != 0 {
						return err
					}
					continue
				}
				payload = decoded
			}
			if flags&0x02 != 0 {
				if denial := cursorAccountAccessDenial(payload); denial != "" {
					return fmt.Errorf("this account does not have access to %s: %s", modelID, denial)
				}
				if text := cursorAccountErrorText(payload); text != "" {
					return fmt.Errorf("cursor refused the model call: %s", text)
				}
				return finishTurn()
			}
			msg := &agentv1.AgentServerMessage{}
			if err := proto.Unmarshal(payload, msg); err != nil {
				if denial := cursorAccountAccessDenial(payload); denial != "" {
					return fmt.Errorf("this account does not have access to %s: %s", modelID, denial)
				}
				if text := cursorAccountErrorText(payload); text != "" {
					return fmt.Errorf("cursor refused the model call: %s", text)
				}
				cursorAccountNoteFrame(fmt.Sprintf("unmarshal_fail flags=%02x bytes=%d", flags, len(payload)))
				continue
			}
			cursorAccountNoteFrame(cursorAccountServerMessageKind(msg))
			if kv := msg.GetKvServerMessage(); kv != nil {
				if err := appendCursorAccount(ctx, authorization, requestID, nextSeq(), mustCursorProto(cursorAccountKVReply(kv))); err != nil {
					return err
				}
				if set := kv.GetSetBlobArgs(); set != nil {
					if bytes.HasPrefix(set.GetBlobData(), []byte(`{"role":`)) {
						cursorAccountRememberMessageBlob(conversationID, set.GetBlobId())
					}
					if state := cursorAccountStateFromBlob(set.GetBlobData()); state != nil {
						cursorAccountRememberState(conversationID, state)
					}
				}
				continue
			}
			if checkpoint := msg.GetConversationCheckpointUpdate(); checkpoint != nil {
				cursorAccountRememberState(conversationID, checkpoint)
				continue
			}
			if exec := msg.GetExecServerMessage(); exec != nil {
				if exec.GetRequestContextArgs() != nil {
					if err := appendCursorAccount(ctx, authorization, requestID, nextSeq(), mustCursorProto(cursorAccountContextReply(exec.GetId(), exec.GetExecId(), requestContext))); err != nil {
						return err
					}
					continue
				}
				if reply, ok := cursorAccountExecReply(exec); ok {
					if err := appendCursorAccount(ctx, authorization, requestID, nextSeq(), mustCursorProto(reply)); err != nil {
						return err
					}
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
				if toolResults != nil && pendingExecs != nil {
					select {
					case pendingExecs <- cursorExecSlot{id: exec.GetId(), execID: exec.GetExecId()}:
					default:
					}
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
						NativeExec:  mustCursorProto(exec),
						ModelCallID: requestID,
					},
				}); err != nil {
					return err
				}
				if toolResults != nil {
					continue
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
				finishReason = "stop"
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

func serveCursorAccountToolResults(ctx context.Context, authorization, requestID string, nextSeq func() int64, toolResults <-chan *agentv1.ExecClientMessage, pendingExecs <-chan cursorExecSlot, finish chan<- struct{}, continued chan<- struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-toolResults:
			if !ok || msg == nil {
				select {
				case finish <- struct{}{}:
				default:
				}
				return
			}
			cloned, _ := proto.Clone(msg).(*agentv1.ExecClientMessage)
			if cloned == nil {
				cloned = msg
			}
			if strings.TrimSpace(cloned.GetExecId()) == "" {
				select {
				case slot := <-pendingExecs:
					cloned.Id = slot.id
					cloned.ExecId = slot.execID
				default:
				}
			}
			wrapped := &agentv1.AgentClientMessage{
				Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: cloned},
			}
			if err := appendCursorAccount(ctx, authorization, requestID, nextSeq(), mustCursorProto(wrapped)); err != nil {
				logger.Warnf("cursor account tool result was not delivered: %v", err)
				select {
				case finish <- struct{}{}:
				default:
				}
				return
			}
			if id, ok := cursorAccountShellCloseID(cloned); ok {
				// The IDE closes a shell exec with stream_close after the exit
				// event. Without it the upstream tool stays open and Fable sits
				// silent until the stall timer abandons the stream.
				control := &agentv1.AgentClientMessage{
					Message: &agentv1.AgentClientMessage_ExecClientControlMessage{
						ExecClientControlMessage: &agentv1.ExecClientControlMessage{
							Message: &agentv1.ExecClientControlMessage_StreamClose{
								StreamClose: &agentv1.ExecClientStreamClose{Id: id},
							},
						},
					},
				}
				if err := appendCursorAccount(ctx, authorization, requestID, nextSeq(), mustCursorProto(control)); err != nil {
					logger.Warnf("cursor account shell stream was not closed: %v", err)
					select {
					case finish <- struct{}{}:
					default:
					}
					return
				}
			}
			if continued != nil {
				select {
				case continued <- struct{}{}:
				default:
				}
			}
		}
	}
}

func cursorAccountRequestedModel(modelID string, effort string, fast bool) *agentv1.RequestedModel {
	modelID = strings.TrimSpace(modelID)
	effort = strings.ToLower(strings.TrimSpace(effort))
	base, slugEffort, slugFast, variant := splitCursorAccountRunVariant(modelID)
	if variant {
		modelID = base
		if effort == "" {
			effort = slugEffort
		}
		fast = fast || slugFast
	}
	requested := &agentv1.RequestedModel{ModelId: modelID}
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		requested.Parameters = append(requested.Parameters, &agentv1.RequestedModel_ModelParameterValue{Id: "effort", Value: effort})
	}
	if fast {
		requested.Parameters = append(requested.Parameters, &agentv1.RequestedModel_ModelParameterValue{Id: "fast", Value: "true"})
	}
	return requested
}

func splitCursorAccountRunVariant(modelID string) (base string, effort string, fast bool, ok bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if strings.HasSuffix(id, "-fast") {
		fast = true
		id = strings.TrimSuffix(id, "-fast")
	}
	for _, suffix := range []string{"-xhigh", "-medium", "-high", "-low", "-max"} {
		if strings.HasSuffix(id, suffix) {
			return id[:len(id)-len(suffix)], strings.TrimPrefix(suffix, "-"), fast, true
		}
	}
	if fast {
		return id, "", true, true
	}
	return "", "", false, false
}

func cursorAccountRunMessage(req StreamRequest, modelID string) *agentv1.AgentClientMessage {
	mode := req.Mode
	if mode == agentv1.AgentMode_AGENT_MODE_UNSPECIFIED {
		mode = agentv1.AgentMode_AGENT_MODE_AGENT
	}
	conversationID := cursorAccountUpstreamConversationID(req)
	history, text := cursorAccountTurn(req.Messages)
	state := cursorAccountLoadState(strings.TrimSpace(req.ConversationID))
	remembered := state != nil
	if state == nil {
		state = &agentv1.ConversationStateStructure{Mode: mode.Enum()}
	} else if state.Mode == nil {
		state.Mode = mode.Enum()
	}
	action := &agentv1.UserMessageAction{
		UserMessage: &agentv1.UserMessage{
			Text:      text,
			MessageId: uuid.NewString(),
			Mode:      mode,
		},
		RequestContext: cursorAccountRequestContext(req),
	}
	// A saved checkpoint is the conversation. The IDE follow-up sends that
	// state plus the new user message, not a second copy of the transcript.
	if !remembered && len(history) > 0 {
		action.ConversationHistory = &agentv1.ConversationHistory{Messages: history}
	}
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{
			RunRequest: &agentv1.AgentRunRequest{
				ConversationState: state,
				Action: &agentv1.ConversationAction{
					Action: &agentv1.ConversationAction_UserMessageAction{UserMessageAction: action},
				},
				RequestedModel: cursorAccountRequestedModel(modelID, req.ThinkingEffort, req.FastMode),
				ConversationId: proto.String(conversationID),
			},
		},
	}
}

// cursorAccountToolTextLimit caps one non-shell tool result inside the flattened
// follow-up prompt. Shell output uses Cursor's own 10_000-byte head/tail cap.
const cursorAccountToolTextLimit = 16 * 1024

// Cursor keeps a shell result whole until 10_000 bytes, then shows the first
// and last 5_000. A longer raw listing makes the open Fable stream go silent.
const (
	cursorShellOutputLimit = 10_000
	cursorShellOutputEdge  = 5_000
)

// cursorAccountToolStallTimeout is how long the open Cursor stream may sit
// quiet after a tool result before this process gives up. Fable often thinks
// for well over ten seconds on a large turn; cutting there closed the stream
// and the next pass resent the same transcript.
const cursorAccountToolStallTimeout = 3 * time.Minute

func cursorAccountUpstreamConversationID(req StreamRequest) string {
	// The tool stream is closed before the local shell result exists. Reusing
	// that conversation id makes the next RunSSE wait on the unfinished exec.
	if cursorAccountFollowsToolResult(req.Messages) {
		fresh := uuid.NewString()
		logger.Infof("cursor account model follow-up uses a fresh upstream conversation after tool result local=%s upstream=%s", strings.TrimSpace(req.ConversationID), fresh)
		return fresh
	}
	if id := strings.TrimSpace(req.ConversationID); id != "" {
		return id
	}
	return uuid.NewString()
}

func cursorAccountFollowsToolResult(messages []Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "" {
			continue
		}
		if role == "tool" || strings.TrimSpace(message.ToolCallID) != "" {
			return true
		}
		if role == "user" || role == "system" {
			return false
		}
	}
	return false
}

// cursorAccountTurn splits a provider transcript the way the IDE's
// UserMessageAction does: the newest user text is the action, and earlier
// turns go in conversation_history. The agent server does not keep those
// turns when the run is opened with only the latest sentence.
func cursorAccountTurn(messages []Message) ([]*agentv1.ConversationHistoryMessage, string) {
	messages = cursorAccountPromptMessages(messages)
	if len(messages) == 0 {
		return nil, "Continue."
	}
	split := len(messages)
	last := messages[len(messages)-1]
	if !cursorAccountIsToolMessage(last) && !strings.EqualFold(strings.TrimSpace(last.Role), "assistant") {
		split = len(messages) - 1
	}
	history := make([]*agentv1.ConversationHistoryMessage, 0, split)
	for _, message := range messages[:split] {
		if item := cursorAccountHistoryMessage(message); item != nil {
			history = append(history, item)
		}
	}
	if split == len(messages) {
		return history, "Continue."
	}
	text := strings.TrimSpace(cursorAccountMessageText(messages[split]))
	if text == "" {
		text = "Continue."
	}
	return history, text
}

func cursorAccountHistoryMessage(message Message) *agentv1.ConversationHistoryMessage {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	switch {
	case role == "assistant":
		var parts []*agentv1.ConversationHistoryAssistantContent
		if text := strings.TrimSpace(message.Content); text != "" {
			parts = append(parts, &agentv1.ConversationHistoryAssistantContent{
				Content: &agentv1.ConversationHistoryAssistantContent_Text{
					Text: &agentv1.ConversationHistoryTextContent{Text: text},
				},
			})
		}
		for _, call := range message.ToolCalls {
			parts = append(parts, &agentv1.ConversationHistoryAssistantContent{
				Content: &agentv1.ConversationHistoryAssistantContent_ToolCall{
					ToolCall: &agentv1.ConversationHistoryToolCall{
						ToolCallId: call.ID,
						ToolName:   call.Function.Name,
						ArgsJson:   call.Function.Arguments,
					},
				},
			})
		}
		if len(parts) == 0 {
			return nil
		}
		return &agentv1.ConversationHistoryMessage{
			Message: &agentv1.ConversationHistoryMessage_Assistant{
				Assistant: &agentv1.ConversationHistoryAssistantMessage{Content: parts},
			},
		}
	case cursorAccountIsToolMessage(message):
		return &agentv1.ConversationHistoryMessage{
			Message: &agentv1.ConversationHistoryMessage_Tool{
				Tool: &agentv1.ConversationHistoryToolMessage{
					ToolCallId: message.ToolCallID,
					ToolName:   message.Name,
					Content: []*agentv1.ConversationHistoryToolResultContent{{
						Content: &agentv1.ConversationHistoryToolResultContent_Text{
							Text: &agentv1.ConversationHistoryTextContent{Text: cursorAccountToolText(message)},
						},
					}},
				},
			},
		}
	default:
		text := strings.TrimSpace(message.Content)
		if text == "" {
			return nil
		}
		return &agentv1.ConversationHistoryMessage{
			Message: &agentv1.ConversationHistoryMessage_User{
				User: &agentv1.ConversationHistoryUserMessage{
					Content: []*agentv1.ConversationHistoryUserContent{{
						Content: &agentv1.ConversationHistoryUserContent_Text{
							Text: &agentv1.ConversationHistoryTextContent{Text: text},
						},
					}},
				},
			},
		}
	}
}

// cursorAccountPromptMessages drops the local system prompt and the compiled
// environment sections. Cursor's agent server builds that prompt from the
// request context. Sending both makes the next Fable turn sit silent until
// the upstream aborts, which is the freeze after the first tool call.
func cursorAccountPromptMessages(messages []Message) []Message {
	filtered := make([]Message, 0, len(messages))
	for _, message := range messages {
		if strings.EqualFold(strings.TrimSpace(message.Role), "system") || cursorAccountScaffoldingMessage(message) {
			continue
		}
		filtered = append(filtered, message)
	}
	return filtered
}

func cursorAccountScaffoldingMessage(message Message) bool {
	if cursorAccountIsToolMessage(message) {
		return false
	}
	role := strings.ToLower(strings.TrimSpace(message.Role))
	if role == "assistant" || role == "tool" {
		return false
	}
	text := strings.TrimSpace(message.Content)
	if text == "" {
		return false
	}
	for _, tag := range []string{
		"<user_info>",
		"<rules>",
		"<agent_skills>",
		"<agent_transcripts>",
		"<mcp_file_system>",
		"<custom_subagents>",
		"<system_reminder>",
		"<current_user_request>",
	} {
		if strings.HasPrefix(text, tag) {
			return true
		}
	}
	// A compiled prompt pasted into one user message still starts the stall,
	// even when a short question is buried under the environment dump.
	if len(text) > 24*1024 && (strings.Contains(text, "<user_info>") || strings.Contains(text, "<agent_skills>") || strings.Contains(text, "<agent_skill ")) {
		return true
	}
	return false
}

func cursorAccountIsToolMessage(message Message) bool {
	return strings.EqualFold(strings.TrimSpace(message.Role), "tool") || strings.TrimSpace(message.ToolCallID) != ""
}

func cursorAccountMessageText(message Message) string {
	if !cursorAccountIsToolMessage(message) {
		return message.Content
	}
	return cursorAccountToolText(message)
}

func cursorAccountToolText(message Message) string {
	if strings.EqualFold(strings.TrimSpace(message.Name), "Shell") {
		return CursorAccountShellOutput(message.Content)
	}
	return cursorAccountTruncateMiddle(message.Content, cursorAccountToolTextLimit)
}

// CursorAccountShellOutput is the shell text the installed Cursor client puts
// in front of the model. The terminal UI keeps the full output. The model sees
// at most 10_000 bytes, split into the head and tail once it grows past that.
func CursorAccountShellOutput(text string) string {
	if len(text) <= cursorShellOutputLimit {
		return text
	}
	head := cursorAccountUTF8Prefix(text, cursorShellOutputEdge)
	tail := cursorAccountUTF8Suffix(text, cursorShellOutputEdge)
	elided := len(text) - len(head) - len(tail)
	if elided < 0 {
		elided = 0
	}
	return head + "\n\n[Output too long. " + strconv.Itoa(elided) + " chars elided]\n\n" + tail
}

func cursorAccountUTF8Prefix(text string, limit int) string {
	if limit <= 0 || text == "" {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	prefix := text[:limit]
	for !utf8.ValidString(prefix) && prefix != "" {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix
}

func cursorAccountUTF8Suffix(text string, limit int) string {
	if limit <= 0 || text == "" {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	suffix := text[len(text)-limit:]
	for !utf8.ValidString(suffix) && suffix != "" {
		suffix = suffix[1:]
	}
	return suffix
}

func cursorAccountShellCloseID(message *agentv1.ExecClientMessage) (uint32, bool) {
	stream := message.GetShellStream()
	if stream == nil {
		return 0, false
	}
	switch stream.GetEvent().(type) {
	case *agentv1.ShellStream_Exit,
		*agentv1.ShellStream_Rejected,
		*agentv1.ShellStream_PermissionDenied,
		*agentv1.ShellStream_Backgrounded,
		*agentv1.ShellStream_SandboxUnsupported:
		return message.GetId(), true
	default:
		return 0, false
	}
}

func cursorAccountTruncateMiddle(text string, limit int) string {
	if len(text) <= limit || limit < 64 {
		return text
	}
	const marker = "\n…[terminal output truncated]…\n"
	keep := limit - len(marker)
	head := keep / 2
	tail := keep - head
	return text[:head] + marker + text[len(text)-tail:]
}

func cursorAccountKVReply(msg *agentv1.KvServerMessage) *agentv1.AgentClientMessage {
	client := &agentv1.KvClientMessage{Id: msg.GetId()}
	if set := msg.GetSetBlobArgs(); set != nil {
		cursorAccountRememberBlob(set.GetBlobId(), set.GetBlobData())
		client.Message = &agentv1.KvClientMessage_SetBlobResult{SetBlobResult: &agentv1.SetBlobResult{}}
	} else {
		result := &agentv1.GetBlobResult{}
		if data, ok := cursorAccountBlob(msg.GetGetBlobArgs().GetBlobId()); ok {
			result.BlobData = data
		}
		client.Message = &agentv1.KvClientMessage_GetBlobResult{GetBlobResult: result}
	}
	return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_KvClientMessage{KvClientMessage: client}}
}

func cursorAccountContextReply(id uint32, execID string, requestContext *agentv1.RequestContext) *agentv1.AgentClientMessage {
	if requestContext == nil {
		requestContext = cursorAccountRequestContext(StreamRequest{})
	}
	cloned, _ := proto.Clone(requestContext).(*agentv1.RequestContext)
	if cloned == nil {
		cloned = cursorAccountRequestContext(StreamRequest{})
	}
	cursorAccountMarkContextComplete(cloned)
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{
			ExecClientMessage: &agentv1.ExecClientMessage{
				Id:     id,
				ExecId: execID,
				Message: &agentv1.ExecClientMessage_RequestContextResult{
					RequestContextResult: &agentv1.RequestContextResult{
						Result: &agentv1.RequestContextResult_Success{
							Success: &agentv1.RequestContextSuccess{RequestContext: cloned},
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
		args := exec.GetGrepArgs()
		name, msg = cursorAccountGrepToolName(args), args
		if id := strings.TrimSpace(args.GetToolCallId()); id != "" {
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
	case exec.GetMcpArgs() != nil:
		name, msg = "CallMcpTool", exec.GetMcpArgs()
	case exec.GetListMcpResourcesExecArgs() != nil:
		name, msg = "ListMcpResources", exec.GetListMcpResourcesExecArgs()
	case exec.GetReadMcpResourceExecArgs() != nil:
		name, msg = "FetchMcpResource", exec.GetReadMcpResourceExecArgs()
	case exec.GetMcpStateExecArgs() != nil:
		name, msg = "GetMcpTools", exec.GetMcpStateExecArgs()
	case exec.GetRecordScreenArgs() != nil:
		name, msg = "RecordScreen", exec.GetRecordScreenArgs()
	case exec.GetComputerUseArgs() != nil:
		name, msg = "ComputerUse", exec.GetComputerUseArgs()
	case exec.GetWriteShellStdinArgs() != nil:
		name, msg = "WriteShellStdin", exec.GetWriteShellStdinArgs()
	case exec.GetForceBackgroundShellArgs() != nil:
		name, msg = "ForceBackgroundShell", exec.GetForceBackgroundShellArgs()
	case exec.GetSubagentArgs() != nil:
		args := exec.GetSubagentArgs()
		name, msg = "Task", args
		if id := strings.TrimSpace(args.GetToolCallId()); id != "" {
			callID = id
		}
	case exec.GetForceBackgroundSubagentArgs() != nil:
		name, msg = "ForceBackgroundSubagent", exec.GetForceBackgroundSubagentArgs()
	case exec.GetSubagentAwaitArgs() != nil:
		name, msg = "SubagentAwait", exec.GetSubagentAwaitArgs()
	case exec.GetExecuteHookArgs() != nil:
		name, msg = "ExecuteHook", exec.GetExecuteHookArgs()
	case exec.GetCanvasDiagnosticsArgs() != nil:
		name, msg = "CanvasDiagnostics", exec.GetCanvasDiagnosticsArgs()
	case exec.GetGitDiffRequest() != nil:
		name, msg = "GitDiff", exec.GetGitDiffRequest()
	case exec.GetConversationSearchArgs() != nil:
		name, msg = "SearchConversations", exec.GetConversationSearchArgs()
	case exec.GetBackgroundShellSpawnArgs() != nil:
		name, msg = "Shell", exec.GetBackgroundShellSpawnArgs()
	case exec.GetPiReadArgs() != nil:
		name, msg = "Read", exec.GetPiReadArgs()
	case exec.GetPiWriteArgs() != nil:
		name, msg = "Write", exec.GetPiWriteArgs()
	case exec.GetPiEditArgs() != nil:
		name, msg = "Edit", exec.GetPiEditArgs()
	case exec.GetPiBashArgs() != nil:
		name, msg = "Shell", exec.GetPiBashArgs()
	case exec.GetPiGrepArgs() != nil:
		name, msg = "Grep", exec.GetPiGrepArgs()
	case exec.GetPiFindArgs() != nil:
		name, msg = "Glob", exec.GetPiFindArgs()
	case exec.GetPiLsArgs() != nil:
		name, msg = "Ls", exec.GetPiLsArgs()
	case exec.GetMiniSweAgentBashArgs() != nil:
		name, msg = "Shell", exec.GetMiniSweAgentBashArgs()
	case exec.GetMessage() != nil:
		name, msg = "Exec", exec
	default:
		return "", "", nil, false
	}
	raw, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(msg)
	if err != nil {
		return "", "", nil, false
	}
	return name, callID, raw, true
}

// cursorAccountGrepToolName matches the installed IDE: a patternless
// files_with_matches grep is the Glob tool's exec, not a content search.
func cursorAccountGrepToolName(args *agentv1.GrepArgs) string {
	if args == nil || strings.TrimSpace(args.GetPattern()) != "" || strings.TrimSpace(args.GetGlob()) == "" {
		return "Grep"
	}
	mode := strings.TrimSpace(args.GetOutputMode())
	if mode == "" || mode == "files_with_matches" {
		return "Glob"
	}
	return "Grep"
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

func cursorAccountGunzip(payload []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, 8<<20))
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
	detail := strings.TrimSpace(cursorAccountErrorText(raw))
	if detail == "" {
		return ""
	}
	lowered := strings.ToLower(detail)
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
		if strings.Contains(lowered, marker) {
			return detail
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

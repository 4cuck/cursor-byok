package modeladapter

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"cursor/internal/modelcontext"

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
	// agentHosts remembers where each account's agent stream lives.
	agentHosts cursorAccountAgentHostCache
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

func (gateway *cursorAccountGateway) Stream(ctx context.Context, req StreamRequest, sink func(ModelEvent) error) (streamErr error) {
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
	if !cursorAccountSideCall(req) {
		// The previous stream of this run was lost: this call resumes the
		// turn. The record is taken once, so every attempt below builds the
		// same run message.
		if recovery, ok := cursorAccountTakeRecovery(req.RunID); ok {
			req.cursorRecovery = &recovery
			defer func() {
				if streamErr != nil {
					// The call that was to resume the turn failed, perhaps
					// before it reached Cursor. Whoever makes the call again
					// still has to resume, not start from the transcript.
					cursorAccountRestoreRecovery(req.RunID)
				}
			}()
			from := "the request it had sent"
			if recovery.checkpointed {
				from = "its last checkpoint"
			}
			logger.Infof("cursor account resuming an interrupted turn conversation=%s from %s lost_streams=%d", strings.TrimSpace(req.ConversationID), from, recovery.recoveries)
			if err := cursorAccountWaitBeforeRecovery(ctx, recovery.recoveries); err != nil {
				return err
			}
		}
	}
	candidates := candidateCursorAccountsForModel(gateway.auth, req.CursorAccountID, modelID)
	requested := strings.TrimSpace(req.CursorAccountID)
	if len(candidates) > 0 && requested != "" && candidates[0] != "" && candidates[0] != requested {
		logger.Infof("cursor account model using account=%s instead of unavailable account=%s for model=%s", candidates[0], requested, modelID)
	}

	output := &cursorAccountOutput{sink: sink}
	var lastErr error
	for idx, accountID := range candidates {
		hasMore := idx+1 < len(candidates)
		authorization, authErr := gateway.authorize(ctx, accountID)
		if authErr != nil {
			lastErr = fmt.Errorf("cursor account is not signed in: %w", authErr)
			if hasMore && !output.forwarded {
				logger.Warnf("cursor account authorize failed account=%s err=%v; trying next account=%s", accountID, authErr, candidates[idx+1])
				continue
			}
			return lastErr
		}

		err := gateway.streamAccountAttempt(ctx, req, accountID, authorization, modelID, output)
		if err == nil {
			// A served turn proves this account has usage left for this model again.
			ClearCursorAccountModelExhausted(accountID, modelID)
			return nil
		}

		lastErr = err
		if output.forwarded || ctx.Err() != nil {
			return lastErr
		}

		if cursorAccountDeviceLimited(err) {
			cursorAccountMarkLocked(gateway.auth, accountID)
			if hasMore {
				logger.Infof("cursor account model skipped device-locked account=%s; retrying with account=%s", accountID, candidates[idx+1])
				continue
			}
			// Check if current account unlocked wasn't already in candidates (e.g. when requested was the only initial fallback).
			if alt := unlockedCurrentCursorAccount(gateway.auth, accountID); alt != "" {
				logger.Infof("cursor account model skipped device-locked account=%s using current=%s", accountID, alt)
				if altAuth, altErr := gateway.authorize(ctx, alt); altErr == nil {
					if retryErr := gateway.streamAccountAttempt(ctx, req, alt, altAuth, modelID, output); retryErr == nil {
						ClearCursorAccountModelExhausted(alt, modelID)
						return nil
					}
				}
			}
			return lastErr
		}

		if cursorAccountRefusesModelText(err.Error()) {
			// Only usage/plan wording becomes a cooldown; "does not have access" can
			// also mean a silent stream, which resolves on its own.
			if CursorAccountQuotaRefusalText(err.Error()) {
				MarkCursorAccountModelExhaustedWithReason(accountID, modelID, err.Error())
			}
			if hasMore {
				logger.Infof("cursor account model refused account=%s model=%s; retrying with account=%s", accountID, modelID, candidates[idx+1])
				continue
			}
			if alt := alternateCursorAccountForModel(gateway.auth, accountID, modelID); alt != "" {
				logger.Infof("cursor account model refused account=%s model=%s; retrying with account=%s", accountID, modelID, alt)
				if altAuth, altErr := gateway.authorize(ctx, alt); altErr == nil {
					if retryErr := gateway.streamAccountAttempt(ctx, req, alt, altAuth, modelID, output); retryErr == nil {
						ClearCursorAccountModelExhausted(alt, modelID)
						return nil
					}
				}
			}
			return lastErr
		}

		if cursorAccountAuthRejected(err) && hasMore {
			logger.Warnf("cursor account auth rejected account=%s; retrying with account=%s", accountID, candidates[idx+1])
			continue
		}

		return lastErr
	}
	return lastErr
}

// streamAccountAttempt runs one turn on a single Cursor account with in-place
// recovery for expired API-key JWTs and unacknowledged model data-retention
// policies (such as Claude Fable 5 / 5.1 / 5.2 under Privacy Mode).
func (gateway *cursorAccountGateway) streamAccountAttempt(ctx context.Context, req StreamRequest, accountID, authorization, modelID string, output *cursorAccountOutput) error {
	err := gateway.streamAuthorizedForAccount(ctx, req, accountID, authorization, output)
	if err == nil {
		return nil
	}

	// A turn that already streamed text or a tool call is not replayed:
	// the caller would see that output twice.
	if !output.forwarded && cursorAccountAuthRejected(err) && cursorAccountUsesAPIKey(gateway.auth, accountID) {
		if renewed, refreshErr := cursorAccountForceRefresh(ctx, gateway.auth, accountID); refreshErr == nil {
			authorization = renewed
			if retryErr := gateway.streamAuthorizedForAccount(ctx, req, accountID, renewed, output); retryErr == nil {
				return nil
			} else {
				err = retryErr
			}
		}
	}

	if !output.forwarded && cursorAccountNeedsDataRetentionConsent(err) {
		if recovered, retryErr := gateway.retryAfterDataRetentionConsent(ctx, req, accountID, authorization, modelID, output, err); recovered {
			return nil
		} else if retryErr != nil {
			err = retryErr
		}
	}

	return err
}

// cursorAccountOutput forwards model events and records whether the caller
// has already received visible output for the turn.
type cursorAccountOutput struct {
	sink      func(ModelEvent) error
	forwarded bool
}

func (output *cursorAccountOutput) emit(event ModelEvent) error {
	switch event.Kind {
	case ModelEventKindTextDelta, ModelEventKindThinkingDelta, ModelEventKindToolLikeCompleted,
		ModelEventKindPartialToolCall, ModelEventKindToolCallDelta,
		ModelEventKindCursorWebResult, ModelEventKindCursorEditStarted, ModelEventKindCursorEditCompleted,
		ModelEventKindCursorAwaitStarted, ModelEventKindCursorAwaitCompleted:
		output.forwarded = true
	}
	return output.sink(event)
}

var errCursorAccountEmptyEndOfImage = errors.New("cursor model returned only end_of_image token")

func sanitizeCursorAccountTextDelta(text string) string {
	if text == "" {
		return ""
	}
	if !strings.Contains(text, "<end_of_image>") && !strings.Contains(text, "<start_of_image>") {
		return text
	}
	cleaned := strings.ReplaceAll(text, "<end_of_image>", "")
	cleaned = strings.ReplaceAll(cleaned, "<start_of_image>", "")
	return cleaned
}

// streamAuthorizedForAccount runs one turn. When Cursor refuses a saved
// checkpoint before any output, the checkpoint is dropped and the turn is
// sent again with the newest transcript as history. Without that, every
// "continue" reloads the same refused state and fails the same way.
func (gateway *cursorAccountGateway) streamAuthorizedForAccount(ctx context.Context, req StreamRequest, accountID, authorization string, output *cursorAccountOutput) error {
	stateConversationID := cursorAccountStateConversationID(req)
	usedSavedState := stateConversationID != "" && HasCursorAccountState(stateConversationID)
	err := gateway.streamAuthorizedOnce(ctx, req, accountID, authorization, stateConversationID, 0, output.emit)
	if errors.Is(err, errCursorAccountEmptyEndOfImage) && !output.forwarded && ctx.Err() == nil {
		logger.Warnf("cursor account model emitted only <end_of_image> for conversation=%s; retrying turn", stateConversationID)
		err = gateway.streamAuthorizedOnce(ctx, req, accountID, authorization, stateConversationID, 0, output.emit)
		if errors.Is(err, errCursorAccountEmptyEndOfImage) && usedSavedState && !output.forwarded && ctx.Err() == nil {
			logger.Warnf("cursor account model repeated <end_of_image> with saved checkpoint for conversation=%s; clearing checkpoint and retrying with bounded history", stateConversationID)
			ClearCursorAccountState(stateConversationID)
			return gateway.streamAuthorizedOnce(ctx, req, accountID, authorization, stateConversationID, cursorAccountHistoryBudget(req.ResolvedContextWindowTokens), output.emit)
		}
	}
	if err == nil || !usedSavedState || output.forwarded || ctx.Err() != nil || !cursorAccountShouldResetSavedState(err) {
		return err
	}
	logger.Warnf("cursor account saved checkpoint rejected for conversation=%s; clearing checkpoint and retrying with bounded history: %v", stateConversationID, err)
	ClearCursorAccountState(stateConversationID)
	return gateway.streamAuthorizedOnce(ctx, req, accountID, authorization, stateConversationID, cursorAccountHistoryBudget(req.ResolvedContextWindowTokens), output.emit)
}

// streamAuthorizedOnce opens one RunSSE stream. stateConversationID names the
// checkpoint to load and update ("" for none). historyBudget caps the history
// bytes sent without a checkpoint (0 sends the whole transcript).
func (gateway *cursorAccountGateway) streamAuthorizedOnce(ctx context.Context, req StreamRequest, accountID, authorization, stateConversationID string, historyBudget int, sink func(ModelEvent) error) error {
	modelID := strings.TrimSpace(req.ProviderModelID)
	if modelID == "" {
		modelID = strings.TrimSpace(req.ModelID)
	}
	host := gateway.agentHosts.host(ctx, authorization)
	requestID := uuid.NewString()
	run := cursorAccountRunMessageFor(req, modelID, stateConversationID, historyBudget)
	// A pass that follows a lost stream resumes the turn instead of sending
	// what the transcript ends with.
	cursorAccountApplyRecovery(req, run, stateConversationID)
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
		// The remembered host may be the reason; ask again next time.
		gateway.agentHosts.forget(authorization)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		gateway.agentHosts.forget(authorization)
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return newCursorAccountRefusalError(fmt.Sprintf("cursor agent stream returned HTTP %d: ", resp.StatusCode), raw)
	}

	appendFrame := gateway.newStreamAppender(accountID, requestID, authorization).append
	appendErr := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		appendErr <- appendFrame(ctx, 0, mustCursorProto(run))
	}()

	emitted := false
	err = readCursorAccountStream(ctx, appendFrame, requestID, stateConversationID, modelID, cursorAccountEventLabel(req), req.SilentTimeout, resp.Body, appendErr, req.CursorToolResults, req.CursorInteractionReplies, cursorAccountRequestContext(req), func(event ModelEvent) error {
		emitted = true
		return sink(event)
	})
	var lost *cursorAccountStreamLostError
	if !errors.As(err, &lost) || ctx.Err() != nil || cursorAccountSideCall(req) {
		return err
	}
	if !emitted && !lost.checkpointed {
		// Nothing of this attempt reached the caller or was kept upstream:
		// the call can simply be made again, which the router does.
		return err
	}
	recoveries, resumable := cursorAccountNoteStreamLost(req.RunID, lost.checkpointed, run)
	if !resumable {
		logger.Warnf("cursor account stream was lost mid-turn and is not resumed request_id=%s conversation=%s lost_streams=%d: %v", requestID, stateConversationID, recoveries, lost.cause)
		return err
	}
	logger.Warnf("cursor account stream was lost mid-turn; the turn resumes in a fresh pass request_id=%s conversation=%s checkpointed=%t lost_streams=%d: %v", requestID, stateConversationID, lost.checkpointed, recoveries, lost.cause)
	label := cursorAccountEventLabel(req)
	return sink(ModelEvent{
		Kind:         ModelEventKindTurnFinished,
		OccurredAt:   time.Now().UTC(),
		Provider:     label,
		GroupName:    label,
		Model:        modelID,
		FinishReason: FinishReasonCursorStreamInterrupted,
	})
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
	return usableCursorAccountIDForModel(auth, requested, "")
}

// usableCursorAccountIDForModel additionally skips an account that Cursor
// already refused for this specific model, consulting both CurrentAccountID()
// and the full saved Cursor account pool.
func usableCursorAccountIDForModel(auth CursorAccountAuthorizer, requested, modelID string) string {
	candidates := candidateCursorAccountsForModel(auth, requested, modelID)
	if len(candidates) == 0 {
		return strings.TrimSpace(requested)
	}
	chosen := candidates[0]
	if trimmedReq := strings.TrimSpace(requested); trimmedReq != "" && chosen != "" && chosen != trimmedReq {
		logger.Infof("cursor account model using account=%s instead of account=%s", chosen, trimmedReq)
	}
	return chosen
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

// cursorAccountAuthRejected matches Cursor's sign-in failures. An expired
// access token on BidiAppend comes back as HTTP 401 with "If you are logged
// in, try logging out and back in." rather than a Connect code.
func cursorAccountAuthRejected(err error) bool {
	text := strings.ToLower(errString(err))
	return strings.Contains(text, "not logged in") ||
		strings.Contains(text, "not_logged_in") ||
		strings.Contains(text, "unauthenticated") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "session expired") ||
		strings.Contains(text, "session is no longer valid") ||
		strings.Contains(text, "http 401") ||
		strings.Contains(text, "logging out and back in")
}

// cursorAccountShouldResetSavedState reports whether a refused turn points at
// the saved checkpoint. Cancellation, sign-in, plan access, rate limits and
// transport failures do not, and clearing the checkpoint for them would only
// lose context.
func cursorAccountShouldResetSavedState(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if cursorAccountAuthRejected(err) || cursorAccountDeviceLimited(err) || cursorAccountNeedsDataRetentionConsent(err) {
		return false
	}
	var appendErr *cursorAccountAppendError
	if errors.As(err, &appendErr) {
		return cursorAccountAppendRefusedPayload(appendErr.status)
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"context canceled",
		"deadline exceeded",
		"does not have access",
		"rate limit",
		"too many requests",
		"resource_exhausted",
		"usage limit",
		"quota",
		"unavailable",
		"overloaded",
		"invalid parameters for registry model",
		"refused to serve this request based on the content",
	} {
		if strings.Contains(text, marker) {
			return false
		}
	}
	return strings.Contains(text, "cursor refused the model call") || strings.Contains(text, "cursor agent frame is too large")
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

// Exec and interaction IDs have separate wire namespaces. The forwarder, on the
// other hand, keys pending work and history by CallID. Only disambiguate a
// display ID when another invocation on this run has already claimed it.
type cursorDispatchIDs map[string]bool

func (used cursorDispatchIDs) claim(display string) string {
	id := display
	for n := len(used); used[id]; n++ {
		id = fmt.Sprintf("%s-%d", display, n)
	}
	used[id] = true
	return id
}

// The reader registers requests before dispatch; the reply pump retires them.
// Keep closed identities for this run so retransmissions cannot execute twice.
type cursorExecRoutes struct {
	mu       sync.Mutex
	open     map[cursorExecSlot]bool
	openByID map[uint32]int // stream_close carries only the numeric ID
}

func newCursorExecRoutes() *cursorExecRoutes {
	return &cursorExecRoutes{open: make(map[cursorExecSlot]bool), openByID: make(map[uint32]int)}
}

func (routes *cursorExecRoutes) register(slot cursorExecSlot) bool {
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if _, seen := routes.open[slot]; seen {
		return false
	}
	routes.open[slot] = true
	routes.openByID[slot.id]++
	return true
}

// correlate returns nil rather than appending a result to an unknown or
// ambiguous exec. A numeric-only identity is a complete wire key in its own
// right. Old callers omitting IDs may fill them only from an unambiguous slot.
func (routes *cursorExecRoutes) correlate(msg *agentv1.ExecClientMessage) (*agentv1.ExecClientMessage, bool) {
	routes.mu.Lock()
	defer routes.mu.Unlock()
	key := cursorExecSlot{msg.GetId(), msg.GetExecId()}
	if active, known := routes.open[key]; known && (key != (cursorExecSlot{}) || active) {
		if !active { // A duplicate or late reply to an already retired exec.
			return nil, false
		}
	} else {
		var found bool
		for slot, active := range routes.open {
			if (msg.Id != 0 && msg.Id != slot.id) || (msg.ExecId != "" && msg.ExecId != slot.execID) {
				continue
			}
			if !active {
				// A partial ID matching an old slot could be a late reply to it.
				if msg.Id != 0 || msg.ExecId != "" {
					return nil, false
				}
				continue
			}
			if found {
				return nil, false
			}
			key, found = slot, true
		}
		if !found {
			return nil, false
		}
		msg = proto.CloneOf(msg)
		msg.Id, msg.ExecId = key.id, key.execID
	}
	var terminal bool
	if msg.GetShellStream() != nil {
		_, terminal = cursorAccountShellCloseID(msg)
	} else {
		terminal = msg.GetMessage() != nil && msg.GetSubagentAwaitResult().GetStillRunning() == nil
	}
	if !terminal {
		return msg, false
	}
	routes.open[key] = false
	routes.openByID[key.id]--
	_, closeStream := cursorAccountTerminalExecCloseID(msg)
	return msg, closeStream && routes.openByID[key.id] == 0
}

// cursorAccountReplyFailure is a frame the stream reader answered with that
// could not be delivered.
type cursorAccountReplyFailure struct {
	err error
	// blobAck marks a blob acknowledgement: losing one after the turn ended
	// only costs the trailing checkpoint, not the turn.
	blobAck bool
}

func readCursorAccountStream(ctx context.Context, appendFrame cursorAccountAppendFunc, requestID, conversationID, modelID, providerLabel string, silentTimeout time.Duration, body io.Reader, appendErr <-chan error, toolResults <-chan *agentv1.ExecClientMessage, interactionReplies <-chan *agentv1.InteractionResponse, requestContext *agentv1.RequestContext, sink func(ModelEvent) error) error {
	sender := newCursorAccountFrameSender(appendFrame)
	// Frames are sent under streamCtx so that they outlive the read loop: a
	// stream that ended cleanly gives the frames still on their way a moment
	// to arrive (the last blob acknowledgements of a turn are among them), and
	// only then cancels whatever is left.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	err := readCursorAccountFrames(ctx, streamCtx, sender, requestID, conversationID, modelID, providerLabel, silentTimeout, body, appendErr, toolResults, interactionReplies, requestContext, sink)
	if err == nil {
		sender.drain(cursorAccountAppendDrain)
	}
	return err
}

func readCursorAccountFrames(ctx context.Context, streamCtx context.Context, sender *cursorAccountFrameSender, requestID, conversationID, modelID, providerLabel string, silentTimeout time.Duration, body io.Reader, appendErr <-chan error, toolResults <-chan *agentv1.ExecClientMessage, interactionReplies <-chan *agentv1.InteractionResponse, requestContext *agentv1.RequestContext, sink func(ModelEvent) error) error {
	cursorAccountResetFrameLog()
	var pending []byte
	tmp := make([]byte, 8192)
	// reply sends frames the reader itself answers with: blob
	// acknowledgements, request context, allowlist prechecks. The reader does
	// not wait for them; a frame that could not be delivered comes back through
	// replyFailed and ends the stream as it did when it was sent inline.
	replyFailed := make(chan cursorAccountReplyFailure, 1)
	reply := func(blobAck bool, payloads ...[]byte) {
		sender.send(streamCtx, func(err error) {
			if !cursorAccountFrameWasLost(streamCtx, err) {
				return
			}
			select {
			case replyFailed <- cursorAccountReplyFailure{err: err, blobAck: blobAck}:
			default:
			}
		}, payloads...)
	}
	var finishInline <-chan struct{}
	var toolContinued <-chan struct{}
	execRoutes := newCursorExecRoutes()
	if toolResults != nil || interactionReplies != nil {
		signal := make(chan struct{}, 1)
		finishInline = signal
		continued := make(chan struct{}, 8)
		toolContinued = continued
		if toolResults != nil {
			go serveCursorAccountToolResults(streamCtx, sender, toolResults, execRoutes, signal, continued)
		}
		if interactionReplies != nil {
			go serveCursorAccountInteractionReplies(streamCtx, sender, interactionReplies, signal, continued)
		}
	}
	thinkingActive := false
	var thinkingStarted time.Time
	querySeen := make(map[uint32]bool)
	cursorEditDisplayIDs := make(map[string]string)
	// cursorAwaitDisplayIDs holds the upstream AwaitShell calls of this run,
	// each with the id its card is shown under ("" until it has a card).
	cursorAwaitDisplayIDs := make(map[string]string)
	// polledToolCalls holds the tool calls that have already read a file.
	polledToolCalls := make(map[string]bool)
	// silentPolls counts, per tool call, the reads answered without a card.
	silentPolls := make(map[string]int)
	dispatchIDs := cursorDispatchIDs{}
	var inputTokens, outputTokens, cacheRead, cacheWrite, reasoningTokens int64
	usagePresent := false
	finishReason := "stop"
	sawEndOfImageToken := false
	sawVisibleTextAfterLastTool := false
	sawToolCall := false
	sawThinking := false
	// sawCheckpoint reports that Cursor sent a checkpoint of the conversation
	// during this stream.
	sawCheckpoint := false

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
		if finishReason == "stop" && sawEndOfImageToken && !sawVisibleTextAfterLastTool {
			switch {
			case sawToolCall:
				finishReason = "tool_calls"
			case sawThinking:
				finishReason = "incomplete"
			default:
				return errCursorAccountEmptyEndOfImage
			}
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
	toolSilenceActive := false
	toolSilence := time.NewTimer(time.Hour)
	if !toolSilence.Stop() {
		select {
		case <-toolSilence.C:
		default:
		}
	}
	defer toolSilence.Stop()
	resetToolSilence := func() {
		if !toolSilence.Stop() {
			select {
			case <-toolSilence.C:
			default:
			}
		}
		toolSilenceActive = true
		toolSilence.Reset(cursorAccountToolStallTimeout)
	}
	stopToolSilence := func() {
		toolSilenceActive = false
		if !toolSilence.Stop() {
			select {
			case <-toolSilence.C:
			default:
			}
		}
	}

	// Cursor's agent server (including Grok 4.7) flushes SetBlob writes for the
	// user/assistant messages and the terminal conversationCheckpointUpdate AFTER
	// emitting interactionUpdate.turnEnded, waiting for KvClientMessage acks
	// before closing the RunSSE stream. Exiting on turnEnded dropped that final
	// checkpoint and left conversations stuck on the pre-turn turns=0 stub.
	sawTurnEnded := false
	turnEndDrain := time.NewTimer(time.Hour)
	if !turnEndDrain.Stop() {
		select {
		case <-turnEndDrain.C:
		default:
		}
	}
	defer turnEndDrain.Stop()
	resetTurnEndDrain := func(d time.Duration) {
		if !turnEndDrain.Stop() {
			select {
			case <-turnEndDrain.C:
			default:
			}
		}
		turnEndDrain.Reset(d)
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
		stopToolSilence()
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
		case <-finishInline:
			finishReason = "tool_calls"
			return finishTurn()
		case <-toolContinued:
			resetToolSilence()
		case <-toolSilence.C:
			toolSilenceActive = false
			logger.Infof("cursor account stream stalled after a tool result request_id=%s model=%s; continuing in a fresh pass", requestID, modelID)
			finishReason = "tool_calls"
			return finishTurn()
		case <-turnEndDrain.C:
			return finishTurn()
		case failure := <-replyFailed:
			if failure.blobAck && sawTurnEnded {
				return finishTurn()
			}
			return failure.err
		case err := <-appendErr:
			appendErr = nil
			if err != nil {
				return fmt.Errorf("cursor refused the model call: %w", err)
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
				if sawTurnEnded {
					return finishTurn()
				}
				if denial := cursorAccountAccessDenial(payload); denial != "" {
					return &cursorAccountRefusalError{
						message:          fmt.Sprintf("this account does not have access to %s: %s", modelID, denial),
						dashboardActions: extractCursorDashboardActions(payload),
					}
				}
				if text := cursorAccountErrorText(payload); text != "" {
					return newCursorAccountRefusalError("cursor refused the model call: ", payload)
				}
				return finishTurn()
			}
			msg := &agentv1.AgentServerMessage{}
			if err := proto.Unmarshal(payload, msg); err != nil {
				if !sawTurnEnded {
					if denial := cursorAccountAccessDenial(payload); denial != "" {
						return &cursorAccountRefusalError{
							message:          fmt.Sprintf("this account does not have access to %s: %s", modelID, denial),
							dashboardActions: extractCursorDashboardActions(payload),
						}
					}
					if text := cursorAccountErrorText(payload); text != "" {
						return newCursorAccountRefusalError("cursor refused the model call: ", payload)
					}
				}
				cursorAccountNoteFrame(fmt.Sprintf("unmarshal_fail flags=%02x bytes=%d", flags, len(payload)))
				continue
			}
			cursorAccountNoteFrame(cursorAccountServerMessageKind(msg))
			if kv := msg.GetKvServerMessage(); kv != nil {
				armSilent()
				reply(true, mustCursorProto(cursorAccountKVReply(kv)))
				// The IDE's KV store only records (blob_id, blob_data). The prompt
				// root list comes from conversationCheckpointUpdate alone; adding
				// message blobs here re-inflated compacted turns.
				if set := kv.GetSetBlobArgs(); set != nil {
					if state := cursorAccountStateFromBlob(set.GetBlobData()); state != nil {
						cursorAccountRememberState(conversationID, state)
					}
				}
				if sawTurnEnded {
					resetTurnEndDrain(cursorAccountPostTurnKVTimeout)
				}
				continue
			}
			if query := msg.GetInteractionQuery(); query != nil {
				noteUseful()
				name, callID, args, ok := cursorAccountInteractionTool(query)
				if !ok {
					logger.Infof("cursor account ignored unsupported interaction query id=%d request_id=%s", query.GetId(), requestID)
					continue
				}
				if querySeen[query.GetId()] {
					continue
				}
				querySeen[query.GetId()] = true
				callID = dispatchIDs.claim(callID)
				sawToolCall = true
				sawVisibleTextAfterLastTool = false
				if err := finishThinking(); err != nil {
					return err
				}
				raw, err := proto.Marshal(query)
				if err != nil {
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
						CallID:            callID,
						ToolName:          name,
						ArgsJSON:          args,
						NativeInteraction: raw,
						ModelCallID:       requestID,
					},
				}); err != nil {
					return err
				}
				continue
			}
			if checkpoint := msg.GetConversationCheckpointUpdate(); checkpoint != nil {
				cursorAccountRememberState(conversationID, checkpoint)
				if strings.TrimSpace(conversationID) != "" && cursorAccountStateHasConversation(checkpoint) {
					// From here the saved state holds this turn: a lost stream
					// resumes from it.
					sawCheckpoint = true
				}
				if sawTurnEnded {
					// Terminal checkpoint received; allow brief window for the trailing
					// Connect end-stream frame, then finish cleanly.
					resetTurnEndDrain(cursorAccountPostCheckpointGrace)
				}
				continue
			}
			if exec := msg.GetExecServerMessage(); exec != nil {
				if exec.GetRequestContextArgs() != nil {
					reply(false,
						mustCursorProto(cursorAccountContextReply(exec.GetId(), exec.GetExecId(), requestContext)),
						cursorAccountStreamClosePayload(exec.GetId()))
					continue
				}
				if answer, ok := cursorAccountExecReply(exec); ok {
					reply(false, mustCursorProto(answer), cursorAccountStreamClosePayload(exec.GetId()))
					continue
				}
				name, callID, args, ok := cursorAccountExecTool(exec)
				if !ok {
					return fmt.Errorf("cursor requested an unsupported tool execution")
				}
				if !execRoutes.register(cursorExecSlot{id: exec.GetId(), execID: exec.GetExecId()}) {
					continue
				}
				noteUseful()
				// Execs the upstream agent makes for a tool call of its own (the
				// reads and the write behind an edit, the reads a wait polls
				// with) are answered without a card. They are not calls the
				// model made, so they also stay out of the forwarder's
				// tool-call accounting.
				silentExec := cursorExecBelongsToUpstreamEdit(exec, cursorEditDisplayIDs)
				if !silentExec && cursorExecIsInternalPoll(exec, cursorAwaitDisplayIDs, polledToolCalls) {
					silentExec = true
					polling := cursorExecReadToolCallID(exec)
					silentPolls[polling]++
					if _, announced := cursorAwaitDisplayIDs[polling]; !announced && silentPolls[polling] == 1 {
						logger.Infof("cursor account tool call reads a file again without a wait card; answering its reads silently request_id=%s call_id=%s", requestID, polling)
					}
				}
				if silentExec {
					callID = cursorAccountExecWireCallID(exec)
				}
				callID = dispatchIDs.claim(callID)
				sawToolCall = true
				if !silentExec {
					sawVisibleTextAfterLastTool = false
					if err := finishThinking(); err != nil {
						return err
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
						SilentUI:    silentExec,
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
			if appended := update.GetUserMessageAppended(); appended != nil {
				// The server echoes the user's own message this way; the
				// client already shows that one. The notice that a background
				// task finished is written by the server, and the client can
				// only show it if it is passed on.
				if notice := appended.GetUserMessage(); cursorAccountIsTaskNotice(notice) {
					noteUseful()
					if err := sink(ModelEvent{
						Kind:              ModelEventKindCursorUserMessageAppended,
						OccurredAt:        time.Now().UTC(),
						Provider:          providerLabel,
						GroupName:         providerLabel,
						Model:             modelID,
						CursorUserMessage: proto.CloneOf(notice),
					}); err != nil {
						return err
					}
				}
				continue
			}
			if partial := update.GetPartialToolCall(); partial != nil {
				if id := CursorEditPartialCallID(partial); id != "" {
					noteUseful()
					displayID := claimCursorCardDisplayID(cursorEditDisplayIDs, dispatchIDs, id, partial.GetCallId())
					card := proto.CloneOf(partial.GetToolCall())
					if displayID != id && card != nil && card.ToolCallId != nil {
						card.ToolCallId = proto.String(displayID)
					}
					sawToolCall = true
					sawVisibleTextAfterLastTool = false
					if err := finishThinking(); err != nil {
						return err
					}
					if err := sink(ModelEvent{
						Kind:          ModelEventKindPartialToolCall,
						OccurredAt:    time.Now().UTC(),
						Provider:      providerLabel,
						GroupName:     providerLabel,
						Model:         modelID,
						ToolCallID:    displayID,
						ToolCall:      card,
						ArgsTextDelta: partial.GetArgsTextDelta(),
					}); err != nil {
						return err
					}
					continue
				}
				noteCursorAwaitCall(cursorAwaitDisplayIDs, partial.GetToolCall(), partial.GetCallId())
			}
			if delta := update.GetToolCallDelta(); delta != nil && delta.GetToolCallDelta().GetEditToolCallDelta() != nil {
				if id := strings.TrimSpace(delta.GetCallId()); id != "" {
					noteUseful()
					displayID := claimCursorCardDisplayID(cursorEditDisplayIDs, dispatchIDs, id)
					if err := sink(ModelEvent{
						Kind:          ModelEventKindToolCallDelta,
						OccurredAt:    time.Now().UTC(),
						Provider:      providerLabel,
						GroupName:     providerLabel,
						Model:         modelID,
						ToolCallID:    displayID,
						ToolCallDelta: proto.CloneOf(delta.GetToolCallDelta()),
					}); err != nil {
						return err
					}
					continue
				}
			}
			if started := update.GetToolCallStarted(); started != nil {
				if id := CursorEditStartedCallID(started); id != "" {
					noteUseful()
					displayID := claimCursorCardDisplayID(cursorEditDisplayIDs, dispatchIDs, id, started.GetCallId())
					cloned := proto.CloneOf(started)
					if displayID != id {
						cloned.CallId = displayID
						if cloned.ToolCall != nil && cloned.ToolCall.ToolCallId != nil {
							cloned.ToolCall.ToolCallId = proto.String(displayID)
						}
					}
					sawToolCall = true
					sawVisibleTextAfterLastTool = false
					if err := finishThinking(); err != nil {
						return err
					}
					if err := sink(ModelEvent{
						Kind:              ModelEventKindCursorEditStarted,
						OccurredAt:        time.Now().UTC(),
						Provider:          providerLabel,
						GroupName:         providerLabel,
						Model:             modelID,
						ToolCallID:        displayID,
						CursorEditStarted: cloned,
					}); err != nil {
						return err
					}
					continue
				}
				if id := CursorAwaitStartedCallID(started); id != "" {
					// A wait may leave the stream quiet for as long as it blocks;
					// that is not a stalled stream.
					noteUseful()
					displayID := claimCursorCardDisplayID(cursorAwaitDisplayIDs, dispatchIDs, id, started.GetCallId())
					cloned := proto.CloneOf(started)
					if displayID != id {
						cloned.CallId = displayID
						if cloned.ToolCall != nil && cloned.ToolCall.ToolCallId != nil {
							cloned.ToolCall.ToolCallId = proto.String(displayID)
						}
					}
					sawToolCall = true
					sawVisibleTextAfterLastTool = false
					if err := finishThinking(); err != nil {
						return err
					}
					if err := sink(ModelEvent{
						Kind:               ModelEventKindCursorAwaitStarted,
						OccurredAt:         time.Now().UTC(),
						Provider:           providerLabel,
						GroupName:          providerLabel,
						Model:              modelID,
						ToolCallID:         displayID,
						CursorAwaitStarted: cloned,
					}); err != nil {
						return err
					}
					continue
				}
			}
			if completion := update.GetToolCallCompleted(); completion != nil {
				if id := CursorWebResultCallID(completion); id != "" {
					noteUseful()
					// A visible terminal card is not an empty/end-of-image-only turn.
					sawVisibleTextAfterLastTool = true
					if err := sink(ModelEvent{Kind: ModelEventKindCursorWebResult, OccurredAt: time.Now().UTC(),
						Provider: providerLabel, GroupName: providerLabel, Model: modelID, ToolCallID: id, CursorWebResult: completion}); err != nil {
						return err
					}
					continue
				}
				if id := CursorEditCompletedCallID(completion); id != "" {
					noteUseful()
					displayID := claimCursorCardDisplayID(cursorEditDisplayIDs, dispatchIDs, id, completion.GetCallId())
					cloned := proto.CloneOf(completion)
					if displayID != id {
						cloned.CallId = displayID
						if cloned.ToolCall != nil && cloned.ToolCall.ToolCallId != nil {
							cloned.ToolCall.ToolCallId = proto.String(displayID)
						}
					}
					sawVisibleTextAfterLastTool = true
					if err := finishThinking(); err != nil {
						return err
					}
					if err := sink(ModelEvent{
						Kind:                ModelEventKindCursorEditCompleted,
						OccurredAt:          time.Now().UTC(),
						Provider:            providerLabel,
						GroupName:           providerLabel,
						Model:               modelID,
						ToolCallID:          displayID,
						CursorEditCompleted: cloned,
					}); err != nil {
						return err
					}
				} else if id := CursorAwaitCompletedCallID(completion); id != "" {
					displayID := claimCursorCardDisplayID(cursorAwaitDisplayIDs, dispatchIDs, id, completion.GetCallId())
					cloned := proto.CloneOf(completion)
					if displayID != id {
						cloned.CallId = displayID
						if cloned.ToolCall != nil && cloned.ToolCall.ToolCallId != nil {
							cloned.ToolCall.ToolCallId = proto.String(displayID)
						}
					}
					sawToolCall = true
					sawVisibleTextAfterLastTool = false
					if err := finishThinking(); err != nil {
						return err
					}
					if err := sink(ModelEvent{
						Kind:                 ModelEventKindCursorAwaitCompleted,
						OccurredAt:           time.Now().UTC(),
						Provider:             providerLabel,
						GroupName:            providerLabel,
						Model:                modelID,
						ToolCallID:           displayID,
						CursorAwaitCompleted: cloned,
					}); err != nil {
						return err
					}
					logger.Infof("cursor account wait finished request_id=%s call_id=%s silent_polls=%d", requestID, displayID, silentPolls[id])
					// The model has the result of its wait now, as it has after
					// any tool result: from here a quiet stream is a stalled one.
					resetToolSilence()
				}
				continue
			}
			if update.GetSummaryStarted() != nil || update.GetSummary() != nil || update.GetSummaryCompleted() != nil {
				noteUseful()
			}
			if toolSilenceActive && (update.GetHeartbeat() != nil || update.GetToolCallStarted() != nil || update.GetPartialToolCall() != nil || update.GetToolCallDelta() != nil || update.GetStepStarted() != nil || update.GetStepCompleted() != nil || update.GetTokenDelta() != nil) {
				resetToolSilence()
			}
			if text := update.GetTextDelta(); text != nil && text.GetText() != "" {
				rawText := text.GetText()
				if strings.Contains(rawText, "<end_of_image>") || strings.Contains(rawText, "<start_of_image>") {
					sawEndOfImageToken = true
				}
				cleanedText := sanitizeCursorAccountTextDelta(rawText)
				if cleanedText == "" {
					continue
				}
				// A space or newline often arrives as its own delta, especially
				// before a number ("until 10,000"). Dropping that chunk glues
				// the number to the previous word.
				if strings.TrimSpace(cleanedText) != "" {
					noteUseful()
					sawVisibleTextAfterLastTool = true
				}
				if err := sink(ModelEvent{
					Kind:       ModelEventKindTextDelta,
					OccurredAt: time.Now().UTC(),
					Provider:   providerLabel,
					GroupName:  providerLabel,
					Model:      modelID,
					Text:       cleanedText,
				}); err != nil {
					return err
				}
			}
			if thinking := update.GetThinkingDelta(); thinking != nil && thinking.GetText() != "" {
				noteUseful()
				sawThinking = true
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
				inputTokens = cursorAccountUncachedInputTokens(ended)
				outputTokens = ended.GetOutputTokens()
				cacheRead = ended.GetCacheReadTokens()
				cacheWrite = ended.GetCacheWriteTokens()
				reasoningTokens = ended.GetReasoningTokens()
				usagePresent = true
				finishReason = "stop"
				sawTurnEnded = true
				if err := finishThinking(); err != nil {
					return err
				}
				resetTurnEndDrain(cursorAccountPostTurnDrainTimeout)
				continue
			}
		}
		if readErr != nil {
			if readErr == io.EOF || sawTurnEnded {
				return finishTurn()
			}
			if ctx.Err() != nil {
				return readErr
			}
			// The stream was lost before the turn ended. The turn itself is
			// not: the caller resumes it from the last checkpoint.
			if err := finishThinking(); err != nil {
				return err
			}
			return &cursorAccountStreamLostError{cause: readErr, checkpointed: sawCheckpoint}
		}
	}
}

// cursorAccountUncachedInputTokens returns the part of a turn's prompt that
// was neither read from the prompt cache nor written to it.
//
// Cursor reports input_tokens as the whole prompt with both cache figures
// inside it. ModelEvent.InputTokens means input without them, as the other
// provider adapters report it; totals, cache hit rate and cost add the cache
// figures on top. An input smaller than the cache figures cannot contain them
// and is returned as reported.
func cursorAccountUncachedInputTokens(ended *agentv1.TurnEndedUpdate) int64 {
	input := ended.GetInputTokens()
	cached := ended.GetCacheReadTokens() + ended.GetCacheWriteTokens()
	if cached <= 0 || input < cached {
		return input
	}
	return input - cached
}

// serveCursorAccountInteractionReplies writes the answers to interaction
// cards back to the open stream. An answer that cannot be delivered ends the
// pass: the server is still waiting for it, so the turn resumes in a fresh
// pass rather than waiting for a frame that will not come.
func serveCursorAccountInteractionReplies(ctx context.Context, sender *cursorAccountFrameSender, replies <-chan *agentv1.InteractionResponse, finish, continued chan<- struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-replies:
			if !ok || msg == nil {
				return
			}
			wrapped := &agentv1.AgentClientMessage{
				Message: &agentv1.AgentClientMessage_InteractionResponse{InteractionResponse: msg},
			}
			sender.send(ctx, func(err error) {
				if err == nil {
					signalCursorAccountPass(continued)
					return
				}
				if cursorAccountFrameWasLost(ctx, err) {
					logger.Warnf("cursor account interaction reply was not delivered: %v", err)
					signalCursorAccountPass(finish)
				}
			}, mustCursorProto(wrapped))
		}
	}
}

// serveCursorAccountToolResults writes tool results back to the open stream.
// Each result is handed to the sender as soon as it arrives, together with
// the stream_close that ends its exec, and the next result is taken without
// waiting for the round trip: the results of one batch of tool calls travel
// together, as they do from the IDE.
func serveCursorAccountToolResults(ctx context.Context, sender *cursorAccountFrameSender, toolResults <-chan *agentv1.ExecClientMessage, routes *cursorExecRoutes, finish chan<- struct{}, continued chan<- struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-toolResults:
			if !ok || msg == nil {
				signalCursorAccountPass(finish)
				return
			}
			reply, closeStream := routes.correlate(msg)
			if reply == nil {
				logger.Warnf("cursor account ignored tool result without an unambiguous outstanding exec id=%d", msg.GetId())
				continue
			}
			payloads := [][]byte{mustCursorProto(&agentv1.AgentClientMessage{
				Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: reply},
			})}
			if closeStream {
				// The IDE closes a finished exec with stream_close. Without it
				// the upstream tool stays open and the model sits silent after
				// a Read, Grep, or background shell spawn. It is numbered right
				// after its result.
				payloads = append(payloads, cursorAccountStreamClosePayload(reply.GetId()))
			}
			sender.send(ctx, func(err error) {
				if err == nil {
					signalCursorAccountPass(continued)
					return
				}
				if cursorAccountFrameWasLost(ctx, err) {
					logger.Warnf("cursor account tool result was not delivered: %v", err)
					signalCursorAccountPass(finish)
				}
			}, payloads...)
		}
	}
}

// cursorAccountFrameWasLost reports whether err is a frame of a live stream
// that the server did not get. Two outcomes are not: a frame that was never
// sent because an earlier one was already lost (that loss is reported by its
// own frame), and a frame abandoned because the stream is over.
func cursorAccountFrameWasLost(streamCtx context.Context, err error) bool {
	return err != nil && !errors.Is(err, errCursorAccountFrameNotSent) && streamCtx.Err() == nil
}

// signalCursorAccountPass wakes the stream reader without ever blocking the
// goroutine that reports.
func signalCursorAccountPass(signal chan<- struct{}) {
	if signal == nil {
		return
	}
	select {
	case signal <- struct{}{}:
	default:
	}
}

// contextWindowTokens supplies the fallback Cursor window when the client did
// not send an explicit context parameter; IDE context selections take precedence.
func cursorAccountRequestedModel(modelID string, effort string, fast bool, maxMode bool, parameters []ModelParameter, specJSON string, contextWindowTokens int) *agentv1.RequestedModel {
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
	// AvailableModels records the parameter ids for this family. Fill every
	// registered parameter from that catalog so a model that calls effort
	// "reasoning" or "reasoning_effort" is not sent the wrong id.
	if catalog, ok := parseCursorRegistryCatalog(specJSON); ok {
		rawClientContext := clientCursorContextValue(parameters)
		window, matchedClientWindow := matchOfferedCursorContextWindow(specJSON, rawClientContext)
		targetContext := ""
		effectiveMaxMode := maxMode
		hasWindow := false
		if matchedClientWindow {
			hasWindow = true
			targetContext = window.Value
			if targetContext != "" {
				parameters = withCursorContextValue(parameters, targetContext)
			} else {
				parameters = withoutCursorContextValue(parameters)
			}
			effectiveMaxMode = window.MaxMode
		} else {
			if rawClientContext != "" {
				parameters = withoutCursorContextValue(parameters)
			}
			if resolvedWindow, ok := modelcontext.ResolveRuntimeCursorContextWindow(specJSON, contextWindowTokens, "", maxMode); ok {
				window = resolvedWindow
				hasWindow = true
				targetContext = window.Value
				if targetContext != "" {
					parameters = withCursorContextValue(parameters, targetContext)
				}
				effectiveMaxMode = maxMode || window.MaxMode
			}
		}
		snapContext := ""
		if hasWindow && window.MaxMode {
			snapContext = targetContext
		}
		completed, variantMax := completeCursorRegistryParameters(catalog, parameters, effort, fast, effectiveMaxMode, snapContext)
		for _, parameter := range completed {
			requested.Parameters = append(requested.Parameters, &agentv1.RequestedModel_ModelParameterValue{Id: parameter.ID, Value: parameter.Value})
		}
		// Long-context variants are rejected unless max_mode is set on the
		// same request. The IDE selects those variants whenever max mode is on.
		requested.MaxMode = variantMax || (hasWindow && window.MaxMode)
		return requested
	}
	if forwarded := cursorAccountForwardParameters(parameters); len(forwarded) > 0 {
		requested.Parameters = forwarded
		requested.MaxMode = maxMode
		return requested
	}
	requested.MaxMode = maxMode
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		requested.Parameters = append(requested.Parameters, &agentv1.RequestedModel_ModelParameterValue{Id: "effort", Value: effort})
	}
	if fast {
		requested.Parameters = append(requested.Parameters, &agentv1.RequestedModel_ModelParameterValue{Id: "fast", Value: "true"})
	}
	return requested
}

func cursorAccountForwardParameters(parameters []ModelParameter) []*agentv1.RequestedModel_ModelParameterValue {
	if len(parameters) == 0 {
		return nil
	}
	forwarded := make([]*agentv1.RequestedModel_ModelParameterValue, 0, len(parameters))
	for _, parameter := range parameters {
		id := strings.TrimSpace(parameter.ID)
		value := strings.TrimSpace(parameter.Value)
		if id == "" || value == "" {
			continue
		}
		forwarded = append(forwarded, &agentv1.RequestedModel_ModelParameterValue{Id: id, Value: value})
	}
	return forwarded
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
	return cursorAccountRunMessageFor(req, modelID, cursorAccountStateConversationID(req), 0)
}

func cursorAccountRunMessageFor(req StreamRequest, modelID, stateConversationID string, historyBudget int) *agentv1.AgentClientMessage {
	mode := req.Mode
	if mode == agentv1.AgentMode_AGENT_MODE_UNSPECIFIED {
		mode = agentv1.AgentMode_AGENT_MODE_AGENT
	}
	conversationID := cursorAccountUpstreamConversationID(req)
	var historyMsgs []Message
	var text string
	if cursorAccountSideCall(req) {
		historyMsgs, text = cursorAccountSideCallTurnMessages(req.Messages)
	} else {
		historyMsgs, text = cursorAccountTurnMessages(req.Messages)
	}
	state := cursorAccountLoadState(stateConversationID)
	remembered := cursorAccountStateHasConversation(state)
	if !remembered {
		if state != nil && stateConversationID != "" {
			logger.Infof("cursor account discarding incomplete or stale checkpoint for conversation=%s; replaying history (%d items)", stateConversationID, len(historyMsgs))
			ClearCursorAccountState(stateConversationID)
		}
		state = &agentv1.ConversationStateStructure{Mode: mode.Enum()}
		// Cursor's cloud agent server (agentn) ignores UserMessageAction.conversation_history
		// on external RunSSE calls and only restores prior messages from
		// ConversationStateStructure.root_prompt_messages_json via KV GetBlob.
		// Synthesize a valid root_prompt_messages_json list backed by KV blobs
		// whenever no saved upstream checkpoint is available (e.g. after rewind,
		// compaction, model switch from third-party BYOK, or checkpoint rejection).
		if len(historyMsgs) > 0 {
			effectiveBudget := historyBudget
			if effectiveBudget <= 0 && req.ResolvedContextWindowTokens > 0 {
				effectiveBudget = cursorAccountHistoryBudget(req.ResolvedContextWindowTokens)
			}
			if rootIDs, keptCount, keptBytes := cursorAccountSynthesizeRootPromptMessages(historyMsgs, effectiveBudget); len(rootIDs) > 0 {
				state.RootPromptMessagesJson = rootIDs
				logger.Infof("cursor account synthesized checkpoint from history conversation=%s blobs=%d bytes=%d budget=%d", stateConversationID, keptCount, keptBytes, effectiveBudget)
			}
		}
	} else if state.Mode == nil {
		state.Mode = mode.Enum()
	}
	selectedContext, preFetchedBlobs := cursorAccountSelectedImagesForTurn(req.Messages)
	requestContext := cursorAccountRequestContext(req)
	// Cursor's own client sends ResumeAction when the user did not type a new
	// message. A tool result or a finished mode switch is already in the
	// checkpoint; inventing a user message of "Continue." makes the model
	// treat that word as the request.
	var action *agentv1.ConversationAction
	if strings.TrimSpace(text) == "" && selectedContext == nil {
		action = &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_ResumeAction{
				ResumeAction: &agentv1.ResumeAction{RequestContext: requestContext},
			},
		}
	} else {
		userMsg := &agentv1.UserMessage{
			Text:      text,
			MessageId: uuid.NewString(),
			Mode:      mode,
		}
		if selectedContext != nil {
			userMsg.SelectedContext = selectedContext
		}
		action = &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_UserMessageAction{
				UserMessageAction: &agentv1.UserMessageAction{
					UserMessage:    userMsg,
					RequestContext: requestContext,
				},
			},
		}
	}
	if relayed := req.CursorConversationAction; relayed != nil && !cursorAccountSideCall(req) {
		// The IDE started this run with an action of its own that Cursor's
		// server understands as it is: a background task finished. Sending a
		// resume in its place starts a turn in which the model is told
		// nothing; the server writes the notice the model reads from this
		// action.
		action = proto.CloneOf(relayed)
	}
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{
			RunRequest: &agentv1.AgentRunRequest{
				ConversationState: state,
				Action:            action,
				RequestedModel:    cursorAccountRequestedModel(modelID, req.ThinkingEffort, req.FastMode, req.MaxMode, req.ModelParameters, req.CursorModelSpec, req.ResolvedContextWindowTokens),
				ConversationId:    proto.String(conversationID),
				PreFetchedBlobs:   preFetchedBlobs,
			},
		},
	}
}

func cursorAccountSelectedImagesForTurn(messages []Message) (*agentv1.SelectedContext, []*agentv1.PreFetchedBlob) {
	if len(messages) == 0 {
		return nil, nil
	}
	var images []*agentv1.SelectedImage
	var blobs []*agentv1.PreFetchedBlob
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if cursorAccountIsToolMessage(msg) || strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			break
		}
		if strings.EqualFold(strings.TrimSpace(msg.Role), "system") {
			continue
		}
		for _, part := range msg.ContentParts {
			if !strings.EqualFold(strings.TrimSpace(part.Type), "image") || part.Image == nil || len(part.Image.Data) == 0 {
				continue
			}
			sum := sha256.Sum256(part.Image.Data)
			blobID := sum[:]
			mimeType := strings.TrimSpace(part.Image.MIMEType)
			if mimeType == "" {
				mimeType = "image/png"
			}
			images = append(images, &agentv1.SelectedImage{
				Uuid:     uuid.NewString(),
				Path:     strings.TrimSpace(part.Image.Path),
				MimeType: mimeType,
				DataOrBlobId: &agentv1.SelectedImage_Data{
					Data: append([]byte(nil), part.Image.Data...),
				},
			})
			blobs = append(blobs, &agentv1.PreFetchedBlob{
				Id:    append([]byte(nil), blobID...),
				Value: append([]byte(nil), part.Image.Data...),
			})
		}
		if len(images) > 0 {
			break
		}
	}
	if len(images) == 0 {
		return nil, nil
	}
	return &agentv1.SelectedContext{SelectedImages: images}, blobs
}

// cursorAccountToolTextLimit caps one non-shell tool result inside the flattened
// follow-up prompt. Shell output uses Cursor's own 10_000-byte head/tail cap.
const cursorAccountToolTextLimit = 16 * 1024

// Cursor keeps a shell result whole until 10_000 characters, then keeps the
// first and last 5_000. A longer raw listing makes the open Fable stream go silent.
const (
	cursorShellOutputLimit = 10_000
	cursorShellOutputEdge  = 5_000
)

// cursorAccountToolStallTimeout is how long the open Cursor stream may sit
// quiet after a tool result before this process gives up. Fable often thinks
// for well over ten seconds on a large turn; cutting there closed the stream
// and the next pass resent the same transcript.
const cursorAccountToolStallTimeout = 3 * time.Minute

// Post-turnEnded drain windows: Cursor's agent harness (especially Grok 4.7)
// sends SetBlob writes (~65ms per round-trip) and the terminal
// conversationCheckpointUpdate AFTER interactionUpdate.turnEnded before closing
// the Connect stream.
var (
	cursorAccountPostTurnDrainTimeout = 1500 * time.Millisecond
	cursorAccountPostTurnKVTimeout    = 1000 * time.Millisecond
	cursorAccountPostCheckpointGrace  = 80 * time.Millisecond
)

// cursorAccountSideCallPrefixes are the CompileSummary prefixes of one-shot
// calls made beside a conversation: summaries, titles, commit messages,
// reviews and image descriptions.
var cursorAccountSideCallPrefixes = []string{
	"compaction ",
	"delegated compaction summary",
	"generate task title",
	"generate commit message",
	"delegation_supervisor_review",
	"vision proxy describe image",
}

func cursorAccountSideCall(req StreamRequest) bool {
	summary := strings.TrimSpace(req.CompileSummary)
	for _, prefix := range cursorAccountSideCallPrefixes {
		if strings.HasPrefix(summary, prefix) {
			return true
		}
	}
	return false
}

// cursorAccountStateConversationID names the conversation whose Cursor
// checkpoint a request loads and updates. A compaction summary carries the
// conversation id but is not a turn of it: loading the checkpoint fed the
// summarizer the very context it had to shrink, and its own writes replaced
// the conversation's checkpoint.
func cursorAccountStateConversationID(req StreamRequest) string {
	if cursorAccountSideCall(req) {
		return ""
	}
	return strings.TrimSpace(req.ConversationID)
}

// cursorAccountHistoryBudget is the history size, in bytes, sent after a
// refused checkpoint: the context window less room for Cursor's prompt,
// tools and the reply, at about three bytes per token.
func cursorAccountHistoryBudget(windowTokens int) int {
	const (
		defaultWindowTokens  = 200_000
		reservedWindowTokens = 64_000
	)
	if windowTokens <= 0 {
		windowTokens = defaultWindowTokens
	}
	tokens := windowTokens - reservedWindowTokens
	if half := windowTokens / 2; tokens < half {
		tokens = half
	}
	return tokens * 3
}

type cursorAccountRootBlob struct {
	role      string
	isSummary bool
	id        []byte
	data      []byte
}

func isCursorAccountSummaryText(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "<conversation_summary>") ||
		strings.HasPrefix(trimmed, "[Previous conversation summary]") ||
		strings.HasPrefix(trimmed, "<summary>")
}

func cursorAccountMarshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func cursorAccountUserInfoPlaceholderBlob() cursorAccountRootBlob {
	data, err := cursorAccountMarshalJSON(map[string]any{
		"role":    "user",
		"content": "<user_info>\n</user_info>",
		"providerOptions": map[string]any{
			"cursor": map[string]any{
				"requestContextCompleteness": map[string]any{
					"rules":           false,
					"env":             false,
					"repositoryInfo":  false,
					"customSubagents": false,
					"agentSkills":     false,
					"gitRepos":        false,
					"gitStatus":       false,
					"mcp":             false,
					"mcpFileSystem":   false,
				},
				"userInfoSummarizationEpoch": 0,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return cursorAccountRootBlob{
		role: "user",
		id:   append([]byte(nil), sum[:]...),
		data: data,
	}
}

func cursorAccountToolCallArgsObject(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil && obj != nil {
			return obj
		}
	}
	return map[string]any{}
}

func cursorAccountMessagePlainText(message Message) string {
	if strings.TrimSpace(message.Content) != "" {
		return message.Content
	}
	if len(message.ContentParts) == 0 {
		return ""
	}
	var parts []string
	for _, part := range message.ContentParts {
		kind := strings.ToLower(strings.TrimSpace(part.Type))
		if kind != "" && kind != "text" {
			continue
		}
		if text := strings.TrimSpace(part.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func cursorAccountOrphanToolAsUserMessage(message Message) (Message, bool) {
	output := strings.TrimSpace(cursorAccountToolText(message))
	if output == "" {
		return Message{}, false
	}
	name := strings.TrimSpace(message.Name)
	if name == "" {
		name = "tool"
	}
	return Message{
		Role:    "user",
		Content: fmt.Sprintf("[Previous %s tool output]\n%s", name, output),
	}, true
}

// sanitizeCursorAccountHistoryMessages normalizes compiled transcript messages
// into valid assistant(tool-call) -> tool(tool-result) sequences for Cursor's
// root_prompt_messages_json store. Router skips sanitizeProviderMessages for
// Cursor account models, so parallel tool calls stored as adjacent assistant
// entries must be merged and dangling/orphan tool entries repaired here.
func sanitizeCursorAccountHistoryMessages(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}
	nonEmpty := make([]Message, 0, len(messages))
	for _, message := range messages {
		if strings.EqualFold(strings.TrimSpace(message.Role), "assistant") &&
			len(message.ToolCalls) == 0 &&
			strings.TrimSpace(cursorAccountMessagePlainText(message)) == "" {
			continue
		}
		nonEmpty = append(nonEmpty, message)
	}
	merged := mergeAdjacentAssistantToolCallMessages(nonEmpty)
	out := make([]Message, 0, len(merged))
	for index := 0; index < len(merged); index++ {
		message := cloneProviderMessage(merged[index])
		if strings.EqualFold(strings.TrimSpace(message.Role), "assistant") && len(message.ToolCalls) > 0 {
			end := index + 1
			responded := make(map[string]struct{}, len(message.ToolCalls))
			for end < len(merged) && cursorAccountIsToolMessage(merged[end]) {
				if id := strings.TrimSpace(merged[end].ToolCallID); id != "" {
					responded[id] = struct{}{}
				}
				end++
			}
			keptCalls := make([]ToolCallDescriptor, 0, len(message.ToolCalls))
			allowedIDs := make(map[string]struct{}, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				id := strings.TrimSpace(call.ID)
				name := strings.TrimSpace(call.Function.Name)
				if id == "" || name == "" {
					continue
				}
				if _, ok := responded[id]; !ok {
					continue
				}
				keptCalls = append(keptCalls, call)
				allowedIDs[id] = struct{}{}
			}
			if len(keptCalls) > 0 {
				message.ToolCalls = keptCalls
				out = append(out, message)
				for toolIndex := index + 1; toolIndex < end; toolIndex++ {
					toolMsg := cloneProviderMessage(merged[toolIndex])
					if _, ok := allowedIDs[strings.TrimSpace(toolMsg.ToolCallID)]; ok {
						out = append(out, toolMsg)
					} else if note, ok := cursorAccountOrphanToolAsUserMessage(toolMsg); ok {
						out = append(out, note)
					}
				}
			} else {
				if strings.TrimSpace(cursorAccountMessagePlainText(message)) != "" {
					message.ToolCalls = nil
					out = append(out, message)
				}
				for toolIndex := index + 1; toolIndex < end; toolIndex++ {
					if note, ok := cursorAccountOrphanToolAsUserMessage(merged[toolIndex]); ok {
						out = append(out, note)
					}
				}
			}
			index = end - 1
			continue
		}
		if cursorAccountIsToolMessage(message) {
			if note, ok := cursorAccountOrphanToolAsUserMessage(message); ok {
				out = append(out, note)
			}
			continue
		}
		out = append(out, message)
	}
	return out
}

func cursorAccountRootBlobForMessage(message Message) (cursorAccountRootBlob, bool) {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	var payload map[string]any
	var blobRole string
	var isSummary bool
	switch {
	case role == "assistant":
		blobRole = "assistant"
		text := strings.TrimSpace(cursorAccountMessagePlainText(message))
		if len(message.ToolCalls) == 0 {
			if text == "" {
				return cursorAccountRootBlob{}, false
			}
			payload = map[string]any{
				"role":    "assistant",
				"content": text,
			}
		} else {
			parts := make([]map[string]any, 0, len(message.ToolCalls)+1)
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, call := range message.ToolCalls {
				callID := strings.TrimSpace(call.ID)
				toolName := strings.TrimSpace(call.Function.Name)
				if callID == "" || toolName == "" {
					continue
				}
				parts = append(parts, map[string]any{
					"type":       "tool-call",
					"toolCallId": callID,
					"toolName":   toolName,
					"args":       cursorAccountToolCallArgsObject(call.Function.Arguments),
				})
			}
			if len(parts) == 0 {
				return cursorAccountRootBlob{}, false
			}
			payload = map[string]any{
				"role":    "assistant",
				"content": parts,
			}
		}
	case cursorAccountIsToolMessage(message):
		callID := strings.TrimSpace(message.ToolCallID)
		if callID == "" {
			return cursorAccountRootBlob{}, false
		}
		blobRole = "tool"
		toolName := strings.TrimSpace(message.Name)
		if toolName == "" {
			toolName = "tool"
		}
		payload = map[string]any{
			"role": "tool",
			"content": []map[string]any{{
				"type":       "tool-result",
				"toolCallId": callID,
				"toolName":   toolName,
				"result":     cursorAccountToolText(message),
			}},
		}
	default:
		text := strings.TrimSpace(cursorAccountMessagePlainText(message))
		if text == "" {
			return cursorAccountRootBlob{}, false
		}
		blobRole = "user"
		isSummary = isCursorAccountSummaryText(text)
		payload = map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type": "text",
				"text": text,
			}},
		}
	}
	data, err := cursorAccountMarshalJSON(payload)
	if err != nil {
		return cursorAccountRootBlob{}, false
	}
	sum := sha256.Sum256(data)
	return cursorAccountRootBlob{
		role:      blobRole,
		isSummary: isSummary,
		id:        append([]byte(nil), sum[:]...),
		data:      data,
	}, true
}

// cursorAccountBoundRootBlobs keeps the newest root prompt blobs that fit in
// budget bytes while preserving the latest compaction summary if present and
// dropping leading detached tool results whose tool call was cut off. A budget
// of 0 keeps everything.
func cursorAccountBoundRootBlobs(blobs []cursorAccountRootBlob, budget int) []cursorAccountRootBlob {
	if budget <= 0 || len(blobs) == 0 {
		return blobs
	}
	total := 0
	for _, blob := range blobs {
		total += len(blob.data)
	}
	if total <= budget {
		return blobs
	}
	summaryIdx := -1
	for i := len(blobs) - 1; i >= 0; i-- {
		if blobs[i].isSummary {
			summaryIdx = i
			break
		}
	}
	if summaryIdx >= 0 && len(blobs[summaryIdx].data) <= budget {
		tail := blobs[summaryIdx+1:]
		remaining := budget - len(blobs[summaryIdx].data)
		start := len(tail)
		used := 0
		for start > 0 {
			size := len(tail[start-1].data)
			if used+size > remaining {
				break
			}
			used += size
			start--
		}
		for start < len(tail) && tail[start].role == "tool" {
			start++
		}
		kept := make([]cursorAccountRootBlob, 0, 1+len(tail)-start)
		kept = append(kept, blobs[summaryIdx])
		kept = append(kept, tail[start:]...)
		logger.Infof("cursor account synthesized history bounded to %d of %d messages with summary preserved (budget=%d bytes)", len(kept), len(blobs), budget)
		return kept
	}
	start := len(blobs)
	used := 0
	for start > 0 {
		size := len(blobs[start-1].data)
		if used+size > budget {
			break
		}
		used += size
		start--
	}
	for start < len(blobs) && blobs[start].role == "tool" {
		start++
	}
	if start > 0 {
		logger.Infof("cursor account synthesized history bounded to the newest %d of %d messages (budget=%d bytes)", len(blobs)-start, len(blobs), budget)
	}
	return blobs[start:]
}

func cursorAccountSynthesizeRootPromptMessages(messages []Message, budget int) ([][]byte, int, int) {
	sanitized := sanitizeCursorAccountHistoryMessages(messages)
	if len(sanitized) == 0 {
		return nil, 0, 0
	}
	blobs := make([]cursorAccountRootBlob, 0, len(sanitized))
	for _, message := range sanitized {
		if blob, ok := cursorAccountRootBlobForMessage(message); ok {
			blobs = append(blobs, blob)
		}
	}
	kept := cursorAccountBoundRootBlobs(blobs, budget)
	if len(kept) == 0 {
		return nil, 0, 0
	}
	placeholder := cursorAccountUserInfoPlaceholderBlob()
	cursorAccountRememberBlob(placeholder.id, placeholder.data)
	rootIDs := make([][]byte, 0, len(kept)+1)
	rootIDs = append(rootIDs, placeholder.id)
	keptBytes := 0
	for _, blob := range kept {
		cursorAccountRememberBlob(blob.id, blob.data)
		rootIDs = append(rootIDs, blob.id)
		keptBytes += len(blob.data)
	}
	return rootIDs, len(kept), keptBytes
}

// cursorAccountBoundHistory keeps the newest messages that fit in budget
// bytes, then drops tool results whose call was cut off. A budget of 0 keeps
// everything.
func cursorAccountBoundHistory(history []*agentv1.ConversationHistoryMessage, budget int) []*agentv1.ConversationHistoryMessage {
	if budget <= 0 {
		return history
	}
	start := len(history)
	used := 0
	for start > 0 {
		size := proto.Size(history[start-1])
		if used+size > budget {
			break
		}
		used += size
		start--
	}
	for start < len(history) && history[start].GetTool() != nil {
		start++
	}
	if start > 0 {
		logger.Infof("cursor account history bounded to the newest %d of %d messages (budget=%d bytes)", len(history)-start, len(history), budget)
	}
	return history[start:]
}

func cursorAccountUpstreamConversationID(req StreamRequest) string {
	if cursorAccountSideCall(req) {
		return uuid.NewString()
	}
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
		if cursorAccountScaffoldingMessage(message) {
			continue
		}
		if role == "user" || role == "system" {
			return false
		}
	}
	return false
}

// cursorAccountTurnMessages splits a provider transcript into prior history
// messages (replayed via ConversationStateStructure.root_prompt_messages_json
// when no saved checkpoint exists) and the newest user action text.
func cursorAccountTurnMessages(messages []Message) ([]Message, string) {
	return cursorAccountSplitTurnMessages(cursorAccountPromptMessages(messages))
}

// cursorAccountSideCallTurnMessages keeps a side call's own instructions and payload.
// Cursor's agent server writes the system prompt and refuses
// custom_system_prompt for these accounts, so the caller's system text rides
// in front of the user message. The scaffolding filter is skipped: a
// compaction transcript can contain those tags and still be the payload.
func cursorAccountSideCallTurnMessages(messages []Message) ([]Message, string) {
	var instructions []string
	rest := make([]Message, 0, len(messages))
	for _, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "system") {
			rest = append(rest, message)
			continue
		}
		if text := strings.TrimSpace(message.Content); text != "" {
			instructions = append(instructions, text)
		}
	}
	history, text := cursorAccountSplitTurnMessages(rest)
	if len(instructions) == 0 {
		return history, text
	}
	return history, "<instructions>\n" + strings.Join(instructions, "\n\n") + "\n</instructions>\n\n" + text
}

func isCursorAccountCommandMessage(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), "<cursor_commands>")
}

func cursorAccountSplitTurnMessages(messages []Message) ([]Message, string) {
	if len(messages) == 0 {
		return nil, ""
	}
	split := len(messages)
	last := messages[len(messages)-1]
	if !cursorAccountIsToolMessage(last) &&
		!strings.EqualFold(strings.TrimSpace(last.Role), "assistant") &&
		!isCursorAccountSummaryText(cursorAccountMessagePlainText(last)) {
		split = len(messages) - 1
	}
	turnEnd := len(messages)
	if split < len(messages) && split > 0 && isCursorAccountCommandMessage(cursorAccountMessagePlainText(messages[split])) {
		prev := messages[split-1]
		prevText := cursorAccountMessagePlainText(prev)
		if !cursorAccountIsToolMessage(prev) &&
			!strings.EqualFold(strings.TrimSpace(prev.Role), "assistant") &&
			!isCursorAccountSummaryText(prevText) &&
			!isCursorAccountBackgroundDelegationMessage(prevText) {
			split--
		}
	}
	historyEnd := split
	var backgroundDelegationNotices []string
	for historyEnd > 0 && isCursorAccountBackgroundDelegationMessage(messages[historyEnd-1].Content) {
		historyEnd--
	}
	for i := historyEnd; i < split; i++ {
		if notice := strings.TrimSpace(messages[i].Content); notice != "" {
			backgroundDelegationNotices = append(backgroundDelegationNotices, notice)
		}
	}
	history := append([]Message(nil), messages[:historyEnd]...)
	if split == len(messages) {
		return history, strings.Join(backgroundDelegationNotices, "\n\n")
	}
	turnParts := make([]string, 0, len(backgroundDelegationNotices)+(turnEnd-split))
	turnParts = append(turnParts, backgroundDelegationNotices...)
	for i := split; i < turnEnd; i++ {
		if part := strings.TrimSpace(cursorAccountMessageText(messages[i])); part != "" {
			turnParts = append(turnParts, part)
		}
	}
	return history, strings.Join(turnParts, "\n\n")
}

func cursorAccountTurn(messages []Message) ([]*agentv1.ConversationHistoryMessage, string) {
	return cursorAccountSplitTurn(cursorAccountPromptMessages(messages))
}

func cursorAccountSideCallTurn(messages []Message) ([]*agentv1.ConversationHistoryMessage, string) {
	historyMsgs, text := cursorAccountSideCallTurnMessages(messages)
	history := make([]*agentv1.ConversationHistoryMessage, 0, len(historyMsgs))
	for _, message := range historyMsgs {
		if item := cursorAccountHistoryMessage(message); item != nil {
			history = append(history, item)
		}
	}
	return history, text
}

func cursorAccountSplitTurn(messages []Message) ([]*agentv1.ConversationHistoryMessage, string) {
	historyMsgs, text := cursorAccountSplitTurnMessages(messages)
	history := make([]*agentv1.ConversationHistoryMessage, 0, len(historyMsgs))
	for _, message := range historyMsgs {
		if item := cursorAccountHistoryMessage(message); item != nil {
			history = append(history, item)
		}
	}
	return history, text
}

func isCursorAccountBackgroundDelegationMessage(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	return strings.Contains(trimmed, "此前转入后台继续运行的委派任务已结束") ||
		strings.Contains(trimmed, "subagent agent_id=")
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

func cursorAccountHasImageParts(message Message) bool {
	for _, part := range message.ContentParts {
		if strings.EqualFold(strings.TrimSpace(part.Type), "image") && part.Image != nil && len(part.Image.Data) > 0 {
			return true
		}
	}
	return false
}

func cursorAccountScaffoldingMessage(message Message) bool {
	if cursorAccountIsToolMessage(message) || cursorAccountHasImageParts(message) {
		return false
	}
	role := strings.ToLower(strings.TrimSpace(message.Role))
	if role == "assistant" || role == "tool" {
		return false
	}
	text := strings.TrimSpace(cursorAccountMessagePlainText(message))
	if text == "" {
		return false
	}
	if isCursorAccountBackgroundDelegationMessage(text) || isCursorAccountSummaryText(text) {
		return false
	}
	for _, tag := range []string{
		"<user_info>",
		"<rules>",
		"<agent_skills>",
		"<agent_transcripts>",
		"<mcp_file_system>",
		"<custom_subagents>",
		"<user_intent_summary>",
		"<hooks_additional_context>",
		"<current_file_contents>",
		"<commit_attribution_message>",
		"<pr_attribution_message>",
		"<current_plan>",
		"<todo_list>",
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
		return cursorAccountMessagePlainText(message)
	}
	return cursorAccountToolText(message)
}

func cursorAccountToolText(message Message) string {
	name := strings.TrimSpace(message.Name)
	// Shell listings are the one result Cursor splits (10_000 characters,
	// first and last 5_000). Read results, including a terminal log opened
	// as a file, are already in the form Cursor's exec host produced.
	// Cutting them drops the middle of a file the model is about to edit.
	if strings.EqualFold(name, "Shell") {
		return CursorAccountShellOutput(message.Content)
	}
	if strings.EqualFold(name, "Read") {
		return message.Content
	}
	return cursorAccountTruncateMiddle(message.Content, cursorAccountToolTextLimit)
}

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

// IsCursorInternalSpillWritePath reports whether path targets a Cursor
// agent-tools/*.txt output spill file (e.g. WebFetch/WebSearch/MCP/Shell spill
// written by Cursor's server/host). Matches Cursor 3.30.14's agent-tools rule.
func IsCursorInternalSpillWritePath(path string) bool {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || trimmed != path {
		return false
	}
	normalized := strings.ReplaceAll(trimmed, "\\", "/")
	parts := strings.Split(normalized, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return false
		}
		clean = append(clean, part)
	}
	if len(clean) < 2 {
		return false
	}
	parent := clean[len(clean)-2]
	base := clean[len(clean)-1]
	return strings.EqualFold(parent, "agent-tools") &&
		strings.HasSuffix(strings.ToLower(base), ".txt") &&
		len(base) > len(".txt")
}

func cursorAccountTerminalExecCloseID(message *agentv1.ExecClientMessage) (uint32, bool) {
	if message == nil || message.GetMessage() == nil {
		return 0, false
	}
	if message.GetShellStream() != nil {
		return cursorAccountShellCloseID(message)
	}
	if message.GetSubagentAwaitResult().GetStillRunning() != nil {
		return 0, false
	}
	// Cursor's cursor-agent-exec sends stream_close after every finished exec
	// (including id=0, because SimpleExecManager.nextId starts at 0). Only
	// non-terminal streaming frames stay open.
	return message.GetId(), true
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

func cursorAccountInteractionID(query *agentv1.InteractionQuery, explicit string) string {
	if id := strings.TrimSpace(explicit); id != "" {
		return id
	}
	if query == nil {
		return "interaction"
	}
	return fmt.Sprintf("interaction-%d", query.GetId())
}

func cursorAccountInteractionArgs(msg proto.Message) []byte {
	if msg == nil {
		return []byte("{}")
	}
	raw, err := protojson.Marshal(msg)
	if err != nil || len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

// cursorAccountInteractionTool maps a cloud InteractionQuery onto the local tool
// the IDE already renders. The original query is forwarded unchanged; its id is
// the id the IDE will answer with.
func cursorAccountInteractionTool(query *agentv1.InteractionQuery) (string, string, []byte, bool) {
	if query == nil {
		return "", "", nil, false
	}
	switch {
	case query.GetAskQuestionInteractionQuery() != nil:
		item := query.GetAskQuestionInteractionQuery()
		return "AskQuestion", cursorAccountInteractionID(query, item.GetToolCallId()), cursorAccountInteractionArgs(item.GetArgs()), true
	case query.GetCreatePlanRequestQuery() != nil:
		item := query.GetCreatePlanRequestQuery()
		return "CreatePlan", cursorAccountInteractionID(query, item.GetToolCallId()), cursorAccountInteractionArgs(item.GetArgs()), true
	case query.GetSwitchModeRequestQuery() != nil:
		item := query.GetSwitchModeRequestQuery()
		args := item.GetArgs()
		callID := ""
		if args != nil {
			callID = args.GetToolCallId()
		}
		callID = cursorAccountInteractionID(query, callID)
		if args != nil && strings.TrimSpace(args.GetToolCallId()) == "" {
			args.ToolCallId = callID
		}
		return "SwitchMode", callID, cursorAccountInteractionArgs(args), true
	case query.GetWebSearchRequestQuery() != nil:
		item := query.GetWebSearchRequestQuery()
		callID := ""
		if item.GetArgs() != nil {
			callID = item.GetArgs().GetToolCallId()
		}
		return "WebSearch", cursorAccountInteractionID(query, callID), cursorAccountInteractionArgs(item.GetArgs()), true
	case query.GetWebFetchRequestQuery() != nil:
		item := query.GetWebFetchRequestQuery()
		callID := ""
		if item.GetArgs() != nil {
			callID = item.GetArgs().GetToolCallId()
		}
		return "WebFetch", cursorAccountInteractionID(query, callID), cursorAccountInteractionArgs(item.GetArgs()), true
	case query.GetGenerateImageRequestQuery() != nil:
		return "GenerateImage", cursorAccountInteractionID(query, ""), cursorAccountInteractionArgs(query.GetGenerateImageRequestQuery().GetArgs()), true
	case query.GetMcpAuthRequestQuery() != nil:
		return "McpAuth", cursorAccountInteractionID(query, ""), cursorAccountInteractionArgs(query.GetMcpAuthRequestQuery().GetArgs()), true
	case query.GetConnectScmRequestQuery() != nil:
		return "ConnectScm", cursorAccountInteractionID(query, ""), cursorAccountInteractionArgs(query.GetConnectScmRequestQuery().GetArgs()), true
	case query.GetPrManagementRequestQuery() != nil:
		item := query.GetPrManagementRequestQuery()
		callID := ""
		if item.GetArgs() != nil {
			callID = item.GetArgs().GetToolCallId()
		}
		return "PrManagement", cursorAccountInteractionID(query, callID), cursorAccountInteractionArgs(item.GetArgs()), true
	default:
		return "", "", nil, false
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
	case exec.GetMcpAllowlistPrecheckArgs() != nil:
		client.Message = &agentv1.ExecClientMessage_McpAllowlistPrecheckResult{
			McpAllowlistPrecheckResult: &agentv1.McpAllowlistPrecheckResult{Allowlisted: true},
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

func cursorAccountExecWireCallID(exec *agentv1.ExecServerMessage) string {
	if exec == nil {
		return ""
	}
	if id := strings.TrimSpace(exec.GetExecId()); id != "" {
		return id
	}
	return fmt.Sprintf("exec-%d", exec.GetId())
}

// claimCursorCardDisplayID returns the id an upstream tool card (an edit or a
// wait) is shown under, claiming one the first time the card is seen. An entry
// that is present but empty names a known call that has no card yet.
func claimCursorCardDisplayID(displayByRaw map[string]string, dispatchIDs cursorDispatchIDs, primary string, aliases ...string) string {
	candidates := make([]string, 0, 1+len(aliases))
	if trimmed := strings.TrimSpace(primary); trimmed != "" {
		candidates = append(candidates, trimmed)
	}
	for _, alias := range aliases {
		if trimmed := strings.TrimSpace(alias); trimmed != "" {
			candidates = append(candidates, trimmed)
		}
	}
	displayID := ""
	for _, key := range candidates {
		if existing, ok := displayByRaw[key]; ok && existing != "" {
			displayID = existing
			break
		}
	}
	if displayID == "" {
		displayID = dispatchIDs.claim(strings.TrimSpace(primary))
	}
	for _, key := range candidates {
		displayByRaw[key] = displayID
	}
	return displayID
}

func cursorExecBelongsToUpstreamEdit(exec *agentv1.ExecServerMessage, cursorEditDisplayIDs map[string]string) bool {
	if exec == nil || len(cursorEditDisplayIDs) == 0 {
		return false
	}
	switch {
	case exec.GetReadArgs() != nil:
		_, ok := cursorEditDisplayIDs[strings.TrimSpace(exec.GetReadArgs().GetToolCallId())]
		return ok
	case exec.GetRedactedReadArgs() != nil:
		_, ok := cursorEditDisplayIDs[strings.TrimSpace(exec.GetRedactedReadArgs().GetToolCallId())]
		return ok
	case exec.GetWriteArgs() != nil:
		_, ok := cursorEditDisplayIDs[strings.TrimSpace(exec.GetWriteArgs().GetToolCallId())]
		return ok
	default:
		return false
	}
}

// noteCursorAwaitCall records an upstream AwaitShell call under every id it
// is announced with, so the file reads it polls with are recognized from the
// first one on.
func noteCursorAwaitCall(awaitCalls map[string]string, card *agentv1.ToolCall, callID string) {
	if card.GetAwaitToolCall() == nil {
		return
	}
	for _, id := range []string{card.GetToolCallId(), callID} {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if _, known := awaitCalls[id]; !known {
			awaitCalls[id] = ""
		}
	}
}

// cursorExecReadToolCallID returns the tool call a file read is made for, or
// "" when the exec is not a file read.
func cursorExecReadToolCallID(exec *agentv1.ExecServerMessage) string {
	switch {
	case exec.GetReadArgs() != nil:
		return strings.TrimSpace(exec.GetReadArgs().GetToolCallId())
	case exec.GetRedactedReadArgs() != nil:
		return strings.TrimSpace(exec.GetRedactedReadArgs().GetToolCallId())
	default:
		return ""
	}
}

// cursorExecIsInternalPoll reports whether a file read is one the upstream
// agent makes for itself rather than a Read the model called.
//
// Cursor's AwaitShell runs on Cursor's server. It watches a shell by reading
// the shell's terminal file through the client several times a second, every
// read under the AwaitShell call's own id, until the command ends, the pattern
// matches or block_until_ms runs out. The client answers those reads without
// showing them: the wait has its own card.
//
// A read is such a poll when its tool call was announced as an Await call. A
// tool call that reads a second time is polling too, whatever it was announced
// as; a Read the model called reads once.
func cursorExecIsInternalPoll(exec *agentv1.ExecServerMessage, awaitCalls map[string]string, polled map[string]bool) bool {
	toolCallID := cursorExecReadToolCallID(exec)
	if toolCallID == "" {
		return false
	}
	if _, waiting := awaitCalls[toolCallID]; waiting {
		return true
	}
	if polled[toolCallID] {
		return true
	}
	polled[toolCallID] = true
	return false
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
		// Internal spill writes to agent-tools/*.txt carry the parent WebFetch /
		// WebSearch / MCP toolCallId. Keep the exec's own wire ID so the spill
		// write never collides with the parent tool card or interaction state.
		if !IsCursorInternalSpillWritePath(exec.GetWriteArgs().GetPath()) {
			if id := strings.TrimSpace(exec.GetWriteArgs().GetToolCallId()); id != "" {
				callID = id
			}
		}
	case exec.GetDeleteArgs() != nil:
		name, msg = "Delete", exec.GetDeleteArgs()
		// The IDE runs a delete only once it finds the tool card named by
		// tool_call_id, and the card is created under this id.
		if id := strings.TrimSpace(exec.GetDeleteArgs().GetToolCallId()); id != "" {
			callID = id
		}
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
		if id := strings.TrimSpace(exec.GetFetchArgs().GetToolCallId()); id != "" {
			callID = id
		}
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

// cursorAccountAgentHost asks api2 for the agent host, falling back to the
// default. The gateway keeps the answer between passes (agentHosts).
func cursorAccountAgentHost(ctx context.Context, authorization string) string {
	host, ok := lookupCursorAccountAgentHost(ctx, authorization)
	if !ok {
		return cursorAccountAgentHostDefault
	}
	return host
}

func lookupCursorAccountAgentHost(ctx context.Context, authorization string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cursorAccountBackendRoot, "/")+cursorAccountServerConfigPath,
		bytes.NewReader(mustCursorProto(&aiserverv1.GetServerConfigRequest{})))
	if err != nil {
		return "", false
	}
	applyCursorAccountHeaders(req, authorization, "application/proto")
	resp, err := cursorAccountHTTPClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	cfg := &aiserverv1.GetServerConfigResponse{}
	if err := proto.Unmarshal(raw, cfg); err != nil {
		return "", false
	}
	host := strings.TrimSpace(cfg.GetAgentUrlConfig().GetAgentnUrl())
	if host == "" {
		host = strings.TrimSpace(cfg.GetAgentUrlConfig().GetAgentUrl())
	}
	if host == "" {
		return "", false
	}
	return strings.TrimRight(host, "/"), true
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
		return &cursorAccountAppendError{err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &cursorAccountAppendError{
			status: resp.StatusCode,
			err:    fmt.Errorf("cursor append returned HTTP %d: %s", resp.StatusCode, cursorAccountErrorText(raw)),
		}
	}
	return nil
}

func applyCursorAccountHeaders(req *http.Request, authorization, contentType string) {
	upstream.ApplyCursorClientHeaders(req, authorization, contentType)
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
	escaped := false
	for i := 0; i < len(rest); i++ {
		if escaped {
			escaped = false
			continue
		}
		if rest[i] == '\\' {
			escaped = true
			continue
		}
		if rest[i] == '"' {
			var decoded string
			if err := json.Unmarshal([]byte(`"`+rest[:i]+`"`), &decoded); err == nil {
				return decoded
			}
			return rest[:i]
		}
	}
	return ""
}

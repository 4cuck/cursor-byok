package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cursor/gen/aiserverv1"
	"cursor/internal/backend/server/upstream"
	"cursor/internal/cursoraccount"

	"google.golang.org/protobuf/proto"
)

// Account health states surfaced to the UI. They answer the question the account
// card previously could not: "is this account actually working right now?"
const (
	HealthStateOK           = "ok"
	HealthStateLocked       = "locked"
	HealthStateRejected     = "rejected"
	HealthStateExpired      = "expired"
	HealthStatePlanRequired = "plan_required"
	HealthStateError        = "error"
	HealthStateSignedOut    = "signed_out"
)

// CursorAccountHealth is the non-secret result of probing the active account.
type CursorAccountHealth struct {
	State string `json:"state"`
	// Message is one readable sentence for the card.
	Message string `json:"message"`
	Email   string `json:"email,omitempty"`
	// Model is the model that was probed, when a model call was attempted.
	Model string `json:"model,omitempty"`
	// LockedUntilUnixMS is set when Cursor refuses the account for device reasons.
	LockedUntilUnixMS int64 `json:"lockedUntilUnixMs,omitempty"`
	// CheckedAtUnixMS lets the UI show staleness.
	CheckedAtUnixMS int64 `json:"checkedAtUnixMs"`
	// ModelCallOK reports whether the model endpoint accepted the request.
	ModelCallOK bool `json:"modelCallOk"`
	// AccountID is the account that was probed. The card uses it so a check
	// cannot be painted onto a different row.
	AccountID string `json:"accountId,omitempty"`
	// Plan is the included-usage snapshot taken with the same credentials.
	Plan *CursorAccountPlan `json:"plan,omitempty"`
}

const (
	cursorHealthTimeout  = 30 * time.Second
	cursorHealthMaxBytes = 1 << 20
)

// CheckCursorAccountHealth probes one saved account end to end: it resolves
// authorization (which re-exchanges API keys), reads the plan and included
// usage, then issues a tiny model call. An empty accountID probes the current
// account. The model call is what distinguishes "signed in" from "actually usable".
func (s *ProxyService) CheckCursorAccountHealth(accountID string) (CursorAccountHealth, error) {
	accountID = strings.TrimSpace(accountID)
	if s == nil || s.cursorAccount == nil {
		return CursorAccountHealth{
			State:     HealthStateSignedOut,
			Message:   "Cursor account service is not initialized.",
			AccountID: accountID,
		}, nil
	}
	if accountID == "" && !s.cursorAccount.SignedIn() {
		return CursorAccountHealth{
			State:   HealthStateSignedOut,
			Message: "No Cursor account is signed in. Import a token or API key first.",
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), cursorHealthTimeout)
	defer cancel()

	health := CursorAccountHealth{CheckedAtUnixMS: time.Now().UnixMilli(), AccountID: accountID}

	authorization, err := s.cursorAccount.AuthorizationFor(ctx, accountID)
	if err != nil {
		health.State = HealthStateRejected
		health.Message = "Cursor rejected this account's credentials: " + err.Error()
		return health, nil
	}
	if health.AccountID == "" {
		health.AccountID = s.cursorAccount.CurrentAccountID()
	}
	if plan, planErr := fetchCursorAccountPlan(ctx, authorization); planErr == nil {
		health.Plan = &plan
	}

	if profile, err := fetchCursorProfile(ctx, authorization); err == nil {
		health.Email = strings.TrimSpace(profile.GetEmail())
	}

	model := "default"
	health.Model = model
	result, err := probeCursorModelCall(ctx, authorization, model)
	switch {
	case err != nil:
		health.State = HealthStateError
		health.Message = "Could not reach Cursor: " + err.Error()
	case result.DeviceLimit:
		health.State = HealthStateLocked
		health.LockedUntilUnixMS = time.Now().Add(cursoraccount.AccountDeviceCooldown).UnixMilli()
		health.Message = "Cursor locked this account: too many devices used it in the last 24 hours. " +
			"Requests will keep failing until the window rolls over."
	case result.OK:
		health.State = HealthStateOK
		health.ModelCallOK = true
		health.Message = "Account is working. Model calls are accepted."
	case result.Code == "ERROR_NOT_LOGGED_IN" || result.Code == "ERROR_USER_NOT_LOGGED_IN":
		health.State = HealthStateExpired
		health.Message = "The session is no longer valid. Re-import the account."
	case isClientVersionGate(result):
		health.State = HealthStateError
		health.Message = cursorVersionGateMessage(result)
	case isPlanRestriction(result):
		health.State = HealthStatePlanRequired
		health.Message = fmt.Sprintf(
			"Cursor requires a paid plan for this call (%s). The key and client are valid; "+
				"the account's plan does not cover model requests.",
			strings.TrimSpace(result.Code))
	default:
		health.State = HealthStateError
		health.Message = fmt.Sprintf("Cursor refused the model call (%s). %s",
			result.Code, strings.TrimSpace(firstNonEmpty(result.Detail, result.Title)))
	}

	// Persist the lock so the card keeps showing it after a restart.
	if health.State == HealthStateLocked && health.AccountID != "" {
		until := time.UnixMilli(health.LockedUntilUnixMS)
		_, _ = s.cursorAccount.MarkAccountLockedByID(health.AccountID, until)
	}
	// A successful probe clears any stale cooldown on the account that was probed.
	if health.State == HealthStateOK && health.AccountID != "" {
		if accounts, listErr := s.cursorAccount.ListAccounts(); listErr == nil {
			for _, account := range accounts {
				if account.ID == health.AccountID && account.LockedUntilUnixMS > 0 {
					_, _ = s.cursorAccount.ClearAccountLockByID(health.AccountID)
					break
				}
			}
		}
	}
	return health, nil
}

type cursorModelProbeResult struct {
	OK          bool
	DeviceLimit bool
	Code        string
	Title       string
	// Action is Cursor's machine-readable remedy hint: "payment" means the plan
	// does not cover this call, "login" means re-authentication is needed.
	Action string
	// Detail is the human-readable explanation when present.
	Detail string
}

// connectFrame wraps a JSON body in the Connect streaming envelope.
func connectFrame(payload []byte) []byte {
	header := make([]byte, 5)
	length := len(payload)
	header[1] = byte(length >> 24)
	header[2] = byte(length >> 16)
	header[3] = byte(length >> 8)
	header[4] = byte(length)
	return append(header, payload...)
}

// extractJSONField pulls a quoted value out of a raw frame. The payload is a
// sequence of length-prefixed JSON frames, so a full unmarshal is not possible.
func extractJSONField(text, marker string) string {
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

func fetchCursorProfile(ctx context.Context, authorization string) (*aiserverv1.GetMeResponse, error) {
	body, err := proto.Marshal(&aiserverv1.GetMeRequest{})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api2.cursor.sh/aiserver.v1.DashboardService/GetMe", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", authorization)
	req.Header.Set("x-cursor-checksum", upstream.BuildCursorChecksum(authorization))
	req.Header.Set("content-type", "application/proto")
	req.Header.Set("accept", "application/proto")
	req.Header.Set("connect-protocol-version", "1")

	resp, err := cursorHealthHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorHealthMaxBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("GetMe returned HTTP " + fmt.Sprint(resp.StatusCode))
	}
	profile := &aiserverv1.GetMeResponse{}
	if err := proto.Unmarshal(raw, profile); err != nil {
		return nil, err
	}
	return profile, nil
}

var cursorHealthHTTPClient = &http.Client{Timeout: cursorHealthTimeout}

// isPlanGateCode reports error codes the installed Cursor client maps to a plan
// upgrade or a payment prompt. ERROR_GPT_4_VISION_PREVIEW_RATE_LIMIT and
// ERROR_OUTDATED_CLIENT are deliberately absent: the retired ChatService
// endpoint returns the vision code, with actionRequired=payment, for every
// account, while the human-readable detail says the client version is
// unsupported. The installed client maps OUTDATED_CLIENT to a client-config
// action, not a plan action.
func isPlanGateCode(code string) bool {
	switch code {
	case "ERROR_PRO_REQUIRED",
		"ERROR_UPGRADE_REQUIRED",
		"ERROR_PRO_USER_ONLY",
		"ERROR_FREE_USER_USAGE_LIMIT",
		"ERROR_FREE_USER_RATE_LIMIT_EXCEEDED",
		"ERROR_PRO_USER_USAGE_LIMIT",
		"ERROR_PRO_USER_RATE_LIMIT_EXCEEDED",
		"ERROR_USAGE_PRICING_REQUIRED",
		"ERROR_USAGE_PRICING_REQUIRED_CHANGEABLE":
		return true
	default:
		return false
	}
}

func isClientVersionGate(result cursorModelProbeResult) bool {
	if result.Code == "ERROR_OUTDATED_CLIENT" || result.Code == "ERROR_DEPRECATED" {
		return true
	}
	text := strings.ToLower(result.Title + " " + result.Detail)
	return strings.Contains(text, "no longer supported") ||
		strings.Contains(text, "outdated version") ||
		strings.Contains(text, "update to the latest version")
}

func isPlanRestriction(result cursorModelProbeResult) bool {
	if isClientVersionGate(result) {
		return false
	}
	return result.Action == "payment" || result.Action == "upgrade" || isPlanGateCode(result.Code)
}

func cursorVersionGateMessage(result cursorModelProbeResult) string {
	detail := strings.TrimSpace(firstNonEmpty(result.Detail, result.Title))
	if detail == "" {
		detail = "Cursor rejected the client build."
	}
	code := strings.TrimSpace(result.Code)
	if code == "" {
		return detail
	}
	return code + ": " + detail
}

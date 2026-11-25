package modeladapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cursor/gen/aiserverv1"
	"cursor/internal/logger"

	"google.golang.org/protobuf/proto"
)

const (
	cursorAccountSetUserNoZdrConsentPath       = "/aiserver.v1.DashboardService/SetUserNoZdrModelConsent"
	cursorAccountSetTeamMemberNoZdrConsentPath = "/aiserver.v1.DashboardService/SetTeamMemberNoZdrModelConsent"
	cursorAccountGetTeamsPath                  = "/aiserver.v1.DashboardService/GetTeams"
	cursorAccountClientActionPath              = "/aiserver.v1.DashboardService/ClientAction"

	cursorFableConsentModelID = "claude-fable-5"
	cursorFableConsentVersion = "fable-data-retention-v1"

	cursorConsentRequestTimeout = 10 * time.Second
	cursorConsentMaxRespBytes   = 1 << 20
)

// cursorAccountRefusalError preserves the exact human-readable refusal text
// while carrying any safe server-driven DashboardAction CTAs decoded from the
// Connect ErrorDetails envelope.
type cursorAccountRefusalError struct {
	message          string
	dashboardActions []*aiserverv1.DashboardAction
}

func (e *cursorAccountRefusalError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func newCursorAccountRefusalError(prefix string, payload []byte) error {
	text := cursorAccountErrorText(payload)
	return &cursorAccountRefusalError{
		message:          prefix + text,
		dashboardActions: extractCursorDashboardActions(payload),
	}
}

func cursorAccountRefusalDashboardActions(err error) []*aiserverv1.DashboardAction {
	var refusal *cursorAccountRefusalError
	if errors.As(err, &refusal) && refusal != nil {
		return refusal.dashboardActions
	}
	return nil
}

// cursorAccountNeedsDataRetentionConsent reports whether Cursor refused the
// model call because the account has not yet acknowledged the model's non-ZDR
// data retention policy (e.g. Claude Fable 5 / 5.1 / 5.2 under Privacy Mode).
func cursorAccountNeedsDataRetentionConsent(err error) bool {
	if err == nil || cursorAccountAuthRejected(err) || cursorAccountDeviceLimited(err) {
		return false
	}
	return cursorAccountNeedsDataRetentionConsentText(err.Error())
}

func cursorAccountNeedsDataRetentionConsentText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || CursorAccountQuotaRefusalText(lower) {
		return false
	}
	hasRetention := strings.Contains(lower, "data retention") || strings.Contains(lower, "data-retention")
	if hasRetention {
		for _, marker := range []string{
			"acknowledge",
			"consent",
			"approve",
			"accept",
			"policy",
			"terms",
		} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return (strings.Contains(lower, "no-zdr") || strings.Contains(lower, "no_zdr")) &&
		strings.Contains(lower, "consent")
}

// cursorNoZdrConsentTarget maps a requested Cursor model slug (such as
// "claude-fable-5-2-low-fast" or "claude-fable-5-1") to the canonical
// model_id and consent_version expected by DashboardService's No-ZDR consent
// endpoints. Cursor stores a single "claude-fable-5" / "fable-data-retention-v1"
// record covering the entire Fable 5 family.
func cursorNoZdrConsentTarget(modelID, errText string) (consentModelID, consentVersion string) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if colon := strings.LastIndex(id, ":"); colon >= 0 {
		id = strings.TrimSpace(id[colon+1:])
	}
	if bracket := strings.Index(id, "["); bracket >= 0 {
		id = strings.TrimSpace(id[:bracket])
	}
	if base, _, _, ok := splitCursorAccountRunVariant(id); ok && base != "" {
		id = base
	}
	id = strings.TrimSuffix(id, "-thinking")
	if id == cursorFableConsentModelID || strings.HasPrefix(id, cursorFableConsentModelID+"-") {
		return cursorFableConsentModelID, cursorFableConsentVersion
	}
	if strings.Contains(strings.ToLower(errText), "fable 5") || strings.Contains(strings.ToLower(errText), "claude-fable-5") {
		return cursorFableConsentModelID, cursorFableConsentVersion
	}
	return id, ""
}

// retryAfterDataRetentionConsent automatically acknowledges a non-ZDR model's
// data retention policy on the current Cursor account and retries the turn
// once it succeeds. Individual user consent is attempted first, followed by
// per-member team consent and any safe server-provided DashboardAction button.
func (gateway *cursorAccountGateway) retryAfterDataRetentionConsent(
	ctx context.Context,
	req StreamRequest,
	accountID, authorization, modelID string,
	output *cursorAccountOutput,
	refusalErr error,
) (bool, error) {
	if output.forwarded || ctx.Err() != nil || !cursorAccountNeedsDataRetentionConsent(refusalErr) {
		return false, refusalErr
	}

	lastErr := refusalErr
	actions := cursorAccountRefusalDashboardActions(refusalErr)

	// Step 1: Individual user consent (works for OAuth session & API-key JWTs).
	if consented, err := acceptCursorUserDataRetentionConsent(ctx, authorization, modelID, lastErr.Error()); err != nil {
		logger.Warnf("cursor account auto data-retention user consent failed account=%s model=%s err=%v", accountID, modelID, err)
	} else if consented {
		logger.Infof("cursor account automatically acknowledged data retention policy account=%s model=%s scope=user", accountID, modelID)
		retryErr := gateway.streamAuthorizedForAccount(ctx, req, accountID, authorization, output)
		if retryErr == nil {
			return true, nil
		}
		lastErr = retryErr
		if output.forwarded || ctx.Err() != nil || !cursorAccountNeedsDataRetentionConsent(retryErr) {
			return false, lastErr
		}
		if nextActions := cursorAccountRefusalDashboardActions(retryErr); len(nextActions) > 0 {
			actions = nextActions
		}
	}

	// Step 2: Team-member consent when the account belongs to a team that allows
	// per-member opt-in rather than team-wide admin consent.
	if consented, err := acceptCursorTeamMemberDataRetentionConsent(ctx, authorization, modelID, lastErr.Error()); err != nil {
		logger.Warnf("cursor account auto data-retention team-member consent failed account=%s model=%s err=%v", accountID, modelID, err)
	} else if consented {
		logger.Infof("cursor account automatically acknowledged data retention policy account=%s model=%s scope=team_member", accountID, modelID)
		retryErr := gateway.streamAuthorizedForAccount(ctx, req, accountID, authorization, output)
		if retryErr == nil {
			return true, nil
		}
		lastErr = retryErr
		if output.forwarded || ctx.Err() != nil || !cursorAccountNeedsDataRetentionConsent(retryErr) {
			return false, lastErr
		}
		if nextActions := cursorAccountRefusalDashboardActions(retryErr); len(nextActions) > 0 {
			actions = nextActions
		}
	}

	// Step 3: Fallback to any safe consent DashboardAction CTA carried in the
	// refusal ErrorDetails (resilient if Cursor bumps consent_version).
	if len(actions) > 0 {
		if ok, err := executeCursorConsentDashboardActions(ctx, authorization, actions); err != nil {
			logger.Warnf("cursor account auto data-retention dashboard action failed account=%s model=%s err=%v", accountID, modelID, err)
		} else if ok {
			logger.Infof("cursor account automatically acknowledged data retention policy account=%s model=%s scope=client_action", accountID, modelID)
			retryErr := gateway.streamAuthorizedForAccount(ctx, req, accountID, authorization, output)
			if retryErr == nil {
				return true, nil
			}
			lastErr = retryErr
		}
	}

	return false, lastErr
}

func acceptCursorUserDataRetentionConsent(ctx context.Context, authorization, modelID, errText string) (bool, error) {
	consentModelID, consentVersion := cursorNoZdrConsentTarget(modelID, errText)
	if consentModelID == "" {
		return false, fmt.Errorf("missing consent model id")
	}
	resp := &aiserverv1.SetUserNoZdrModelConsentResponse{}
	err := postCursorAccountDashboardProto(ctx, authorization, cursorAccountSetUserNoZdrConsentPath, &aiserverv1.SetUserNoZdrModelConsentRequest{
		ModelId:        consentModelID,
		Enabled:        true,
		Acknowledged:   true,
		ConsentVersion: consentVersion,
	}, resp)
	if err != nil {
		return false, err
	}
	return resp.GetConsented(), nil
}

func acceptCursorTeamMemberDataRetentionConsent(ctx context.Context, authorization, modelID, errText string) (bool, error) {
	consentModelID, consentVersion := cursorNoZdrConsentTarget(modelID, errText)
	if consentModelID == "" {
		return false, nil
	}
	teamsResp := &aiserverv1.GetTeamsResponse{}
	if err := postCursorAccountDashboardProto(ctx, authorization, cursorAccountGetTeamsPath, &aiserverv1.GetTeamsRequest{}, teamsResp); err != nil {
		return false, err
	}
	teams := teamsResp.GetTeams()
	if len(teams) == 0 {
		return false, nil
	}
	var lastErr error
	for _, team := range teams {
		teamID := team.GetId()
		if teamID <= 0 {
			continue
		}
		resp := &aiserverv1.SetTeamMemberNoZdrModelConsentResponse{}
		err := postCursorAccountDashboardProto(ctx, authorization, cursorAccountSetTeamMemberNoZdrConsentPath, &aiserverv1.SetTeamMemberNoZdrModelConsentRequest{
			TeamId:         teamID,
			ModelId:        consentModelID,
			Enabled:        true,
			Acknowledged:   true,
			ConsentVersion: consentVersion,
		}, resp)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.GetConsented() {
			return true, nil
		}
	}
	return false, lastErr
}

func executeCursorConsentDashboardActions(ctx context.Context, authorization string, actions []*aiserverv1.DashboardAction) (bool, error) {
	var lastErr error
	for _, action := range actions {
		if !isSafeConsentDashboardAction(action) {
			continue
		}
		ok, err := runCursorConsentClientAction(ctx, authorization, action)
		if err != nil {
			lastErr = err
			continue
		}
		if ok {
			return true, nil
		}
	}
	return false, lastErr
}

// runCursorConsentClientAction mirrors Cursor IDE's kRt flow for DashboardAction
// error buttons: invoke DashboardService/ClientAction, and if the server replies
// with a second-step ClientActionConfirmation carrying a safe follow-up action,
// confirm once.
func runCursorConsentClientAction(ctx context.Context, authorization string, action *aiserverv1.DashboardAction) (bool, error) {
	current := action
	for step := 0; step < 2 && current != nil; step++ {
		if !isSafeConsentDashboardAction(current) {
			return false, fmt.Errorf("refused unsafe dashboard action %q", current.GetAction())
		}
		resp := &aiserverv1.ClientActionResponse{}
		err := postCursorAccountDashboardProto(ctx, authorization, cursorAccountClientActionPath, &aiserverv1.ClientActionRequest{
			Action: current.GetAction(),
			Args:   current.GetArgs(),
		}, resp)
		if err != nil {
			return false, err
		}
		if resp.GetSuccess() {
			return true, nil
		}
		next := resp.GetConfirmation().GetAction()
		if next == nil {
			if msg := strings.TrimSpace(resp.GetErrorMessage()); msg != "" {
				return false, errors.New(msg)
			}
			return false, nil
		}
		current = next
	}
	return false, nil
}

func postCursorAccountDashboardProto(ctx context.Context, authorization, path string, reqMsg, respMsg proto.Message) error {
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, cursorConsentRequestTimeout)
	defer cancel()

	body, err := proto.Marshal(reqMsg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, strings.TrimRight(cursorAccountBackendRoot, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	applyCursorAccountHeaders(req, authorization, "application/proto")
	req.Header.Set("accept", "application/proto")

	resp, err := cursorAccountHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorConsentMaxRespBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := cursorAccountErrorText(raw)
		if detail != "" {
			return fmt.Errorf("%s returned HTTP %d: %s", path, resp.StatusCode, detail)
		}
		return fmt.Errorf("%s returned HTTP %d", path, resp.StatusCode)
	}
	if respMsg == nil {
		return nil
	}
	return proto.Unmarshal(raw, respMsg)
}

type connectErrorDetailEnvelope struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	Debug struct {
		Details struct {
			Buttons []struct {
				DashboardAction *struct {
					Action         string            `json:"action"`
					Args           map[string]string `json:"args"`
					SuccessMessage string            `json:"successMessage"`
				} `json:"dashboardAction"`
			} `json:"buttons"`
		} `json:"details"`
	} `json:"debug"`
}

type connectErrorEnvelope struct {
	Details []connectErrorDetailEnvelope `json:"details"`
	Error   *struct {
		Details []connectErrorDetailEnvelope `json:"details"`
	} `json:"error"`
}

func extractCursorDashboardActions(payload []byte) []*aiserverv1.DashboardAction {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var env connectErrorEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil
	}
	details := env.Details
	if env.Error != nil && len(env.Error.Details) > 0 {
		details = append(details, env.Error.Details...)
	}
	if len(details) == 0 {
		return nil
	}
	var actions []*aiserverv1.DashboardAction
	for _, entry := range details {
		if entry.Type != "" && entry.Type != "aiserver.v1.ErrorDetails" {
			continue
		}
		extractedFromProto := false
		if raw, ok := decodeBase64Flexible(entry.Value); ok {
			errDetails := &aiserverv1.ErrorDetails{}
			if proto.Unmarshal(raw, errDetails) == nil {
				for _, button := range errDetails.GetDetails().GetButtons() {
					if act := button.GetDashboardAction(); act != nil && isSafeConsentDashboardAction(act) {
						actions = append(actions, act)
						extractedFromProto = true
					}
				}
			}
		}
		if extractedFromProto {
			continue
		}
		for _, button := range entry.Debug.Details.Buttons {
			if button.DashboardAction == nil {
				continue
			}
			act := &aiserverv1.DashboardAction{
				Action: strings.TrimSpace(button.DashboardAction.Action),
				Args:   button.DashboardAction.Args,
			}
			if isSafeConsentDashboardAction(act) {
				actions = append(actions, act)
			}
		}
	}
	return actions
}

func decodeBase64Flexible(raw string) ([]byte, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, false
	}
	for _, enc := range []*base64.Encoding{
		base64.RawStdEncoding,
		base64.StdEncoding,
		base64.RawURLEncoding,
		base64.URLEncoding,
	} {
		if decoded, err := enc.DecodeString(value); err == nil {
			return decoded, true
		}
	}
	return nil, false
}

// isSafeConsentDashboardAction ensures automatic ClientAction execution is
// strictly limited to data-retention/no-ZDR consent actions and can never
// trigger billing, upgrades, spend-limit, or privacy-mode mutations.
func isSafeConsentDashboardAction(action *aiserverv1.DashboardAction) bool {
	if action == nil {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(action.GetAction()))
	if name == "" {
		return false
	}
	for _, forbidden := range []string{
		"upgrade",
		"billing",
		"checkout",
		"payment",
		"subscribe",
		"credit",
		"spend",
		"limit",
		"invoice",
		"delete",
		"revoke",
		"logout",
		"privacy_mode",
		"privacymode",
		"disable_privacy",
	} {
		if strings.Contains(name, forbidden) {
			return false
		}
	}
	for _, allowed := range []string{
		"consent",
		"retention",
		"zdr",
		"policy",
		"acknowledge",
		"fable",
	} {
		if strings.Contains(name, allowed) {
			return true
		}
	}
	return false
}

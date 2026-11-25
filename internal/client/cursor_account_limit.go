package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"cursor/gen/aiserverv1"
	modeladapter "cursor/internal/backend/agent/model"
	"cursor/internal/backend/server/upstream"

	"google.golang.org/protobuf/proto"
)

const (
	// CursorUnlimitedHardLimitDollars is INT32_MAX, the sentinel Cursor's
	// DashboardService/GetHardLimit and SetHardLimit RPCs use to represent
	// "Unlimited" (no cap) on-demand spending.
	CursorUnlimitedHardLimitDollars int32 = math.MaxInt32

	CursorSpendLimitModeDisabled  = "disabled"
	CursorSpendLimitModeFixed     = "fixed"
	CursorSpendLimitModeUnlimited = "unlimited"

	cursorGetHardLimitPath = "/aiserver.v1.DashboardService/GetHardLimit"
	cursorSetHardLimitPath = "/aiserver.v1.DashboardService/SetHardLimit"
	cursorGetTeamsPath     = "/aiserver.v1.DashboardService/GetTeams"
)

// CursorAccountSpendLimit is the non-secret on-demand spending limit snapshot
// for one saved Cursor account.
type CursorAccountSpendLimit struct {
	AccountID             string `json:"accountId,omitempty"`
	Mode                  string `json:"mode"` // "disabled" | "fixed" | "unlimited"
	HardLimitDollars      int32  `json:"hardLimitDollars"`
	NoUsageBasedAllowed   bool   `json:"noUsageBasedAllowed"`
	DisabledByOrg         bool   `json:"disabledByOrg,omitempty"`
	Editable              bool   `json:"editable"`
	ReadOnlyReason        string `json:"readOnlyReason,omitempty"`
	TeamID                int32  `json:"teamId,omitempty"`
	PersonalScope         bool   `json:"personalScope,omitempty"`
	IsDynamicTeamLimit    bool   `json:"isDynamicTeamLimit,omitempty"`
	MembershipType        string `json:"membershipType,omitempty"`
	SubscriptionStatus    string `json:"subscriptionStatus,omitempty"`
	ClearedQuotaCooldowns int    `json:"clearedQuotaCooldowns,omitempty"`
}

// CursorAccountSpendLimitRequest updates the on-demand spending limit for one
// saved Cursor account (disable, raise/lower a fixed dollar limit, or set
// unlimited).
type CursorAccountSpendLimitRequest struct {
	AccountID        string `json:"accountId,omitempty"`
	Mode             string `json:"mode"` // "disabled" | "fixed" | "unlimited"
	HardLimitDollars int32  `json:"hardLimitDollars,omitempty"`
	TeamID           int32  `json:"teamId,omitempty"`
	PersonalScope    bool   `json:"personalScope,omitempty"`
}

type cursorDashboardRPCError struct {
	Path       string
	StatusCode int
	Code       string
	ErrorEnum  string
	Title      string
	Detail     string
	PaymentReq bool
	AuthReject bool
}

func (e *cursorDashboardRPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.PaymentReq {
		if e.Detail != "" {
			return "A saved payment method is required before enabling on-demand usage: " + e.Detail
		}
		return "A saved payment method is required in Cursor billing before enabling on-demand usage."
	}
	if msg := strings.TrimSpace(firstNonEmpty(e.Detail, e.Title)); msg != "" {
		if e.Code == "permission_denied" || e.StatusCode == http.StatusForbidden {
			return "Permission denied: " + msg
		}
		return msg
	}
	switch {
	case e.StatusCode == http.StatusUnauthorized || e.Code == "unauthenticated":
		return "Cursor rejected this account's credentials. Refresh or sign in again."
	case e.StatusCode == http.StatusForbidden || e.Code == "permission_denied":
		return "Permission denied: this account or API key is not allowed to change on-demand spend limits."
	case e.Code == "failed_precondition":
		return "On-demand spend limit cannot be changed for this account right now (check plan status or billing method)."
	case e.Code == "invalid_argument":
		return "Cursor rejected the requested spend limit amount."
	default:
		return fmt.Sprintf("Cursor dashboard returned HTTP %d", e.StatusCode)
	}
}

func (e *cursorDashboardRPCError) Unwrap() error {
	if e != nil && e.AuthReject {
		return errPlanAuthRejected
	}
	return nil
}

// GetCursorAccountSpendLimit fetches the live on-demand spend limit state for
// one saved Cursor account (or the current account when accountID is empty).
func (s *ProxyService) GetCursorAccountSpendLimit(accountID string) (CursorAccountSpendLimit, error) {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountSpendLimit{}, fmt.Errorf("Cursor account service is not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cursorHealthTimeout)
	defer cancel()
	return s.getCursorAccountSpendLimit(ctx, accountID)
}

// SetCursorAccountSpendLimit updates the on-demand spend limit for one saved
// Cursor account to disabled ($0), a fixed integer USD cap (higher or lower),
// or unlimited (INT32_MAX).
func (s *ProxyService) SetCursorAccountSpendLimit(req CursorAccountSpendLimitRequest) (CursorAccountSpendLimit, error) {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountSpendLimit{}, fmt.Errorf("Cursor account service is not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cursorHealthTimeout)
	defer cancel()
	return s.setCursorAccountSpendLimit(ctx, req)
}

func (s *ProxyService) resolveCursorAuthorization(ctx context.Context, accountID string) (resolvedID string, authorization string, err error) {
	accountID = strings.TrimSpace(accountID)
	resolvedID = accountID
	if resolvedID == "" {
		resolvedID = strings.TrimSpace(s.cursorAccount.CurrentAccountID())
	}
	authorization, err = s.cursorAccount.AuthorizationFor(ctx, accountID)
	if err != nil && resolvedID != "" {
		if renewed, refreshErr := s.cursorAccount.ForceRefresh(ctx, resolvedID); refreshErr == nil {
			authorization = renewed
			err = nil
		}
	}
	if err != nil {
		return resolvedID, "", fmt.Errorf("could not authorize Cursor account: %w", err)
	}
	return resolvedID, authorization, nil
}

func (s *ProxyService) getCursorAccountSpendLimit(ctx context.Context, accountID string) (CursorAccountSpendLimit, error) {
	resolvedID, authorization, err := s.resolveCursorAuthorization(ctx, accountID)
	if err != nil {
		return CursorAccountSpendLimit{}, err
	}
	limit, err := fetchCursorAccountSpendLimit(ctx, authorization, resolvedID)
	if err != nil && resolvedID != "" && (errors.Is(err, errPlanAuthRejected) || s.cursorAccount.UsesAPIKey(resolvedID)) {
		if renewed, refreshErr := s.cursorAccount.ForceRefresh(ctx, resolvedID); refreshErr == nil {
			limit, err = fetchCursorAccountSpendLimit(ctx, renewed, resolvedID)
		}
	}
	return limit, err
}

func (s *ProxyService) setCursorAccountSpendLimit(ctx context.Context, req CursorAccountSpendLimitRequest) (CursorAccountSpendLimit, error) {
	mode, err := normalizeCursorSpendLimitMode(req.Mode)
	if err != nil {
		return CursorAccountSpendLimit{}, err
	}
	req.Mode = mode
	if _, err := buildSetHardLimitProtoRequest(req, req.TeamID, req.PersonalScope); err != nil {
		return CursorAccountSpendLimit{}, err
	}

	resolvedID, authorization, err := s.resolveCursorAuthorization(ctx, req.AccountID)
	if err != nil {
		return CursorAccountSpendLimit{}, err
	}

	updated, err := applyCursorAccountSpendLimit(ctx, authorization, resolvedID, req)
	if err != nil && resolvedID != "" && (errors.Is(err, errPlanAuthRejected) || s.cursorAccount.UsesAPIKey(resolvedID)) {
		if renewed, refreshErr := s.cursorAccount.ForceRefresh(ctx, resolvedID); refreshErr == nil {
			if retryUpdated, retryErr := applyCursorAccountSpendLimit(ctx, renewed, resolvedID, req); retryErr == nil {
				updated = retryUpdated
				err = nil
			} else if !errors.Is(retryErr, errPlanAuthRejected) {
				err = retryErr
			}
		}
	}
	if err != nil {
		return CursorAccountSpendLimit{}, err
	}
	return updated, nil
}

type cursorSpendLimitScope struct {
	TeamID             int32
	PersonalScope      bool
	Editable           bool
	ReadOnlyReason     string
	MembershipType     string
	SubscriptionStatus string
}

func isUnifiedXSubscriptionMembership(membership string) bool {
	switch strings.ToLower(strings.TrimSpace(membership)) {
	case "premium", "plus", "super", "unified_ultra":
		return true
	default:
		return false
	}
}

func isTeamMembership(membership string) bool {
	switch strings.ToLower(strings.TrimSpace(membership)) {
	case "enterprise", "team", "business":
		return true
	default:
		return false
	}
}

func resolveCursorSpendLimitScope(stripe cursorStripeProfile, usage *aiserverv1.GetCurrentPeriodUsageResponse, teams []*aiserverv1.Team) cursorSpendLimitScope {
	membership := strings.ToLower(strings.TrimSpace(stripe.MembershipType))
	if membership == "" {
		membership = strings.ToLower(strings.TrimSpace(stripe.IndividualMembershipType))
	}
	subStatus := strings.ToLower(strings.TrimSpace(stripe.SubscriptionStatus))

	scope := cursorSpendLimitScope{
		Editable:           true,
		MembershipType:     membership,
		SubscriptionStatus: subStatus,
	}

	limitType := ""
	if usage != nil && usage.GetSpendLimitUsage() != nil {
		limitType = strings.ToLower(strings.TrimSpace(usage.GetSpendLimitUsage().GetLimitType()))
	}

	inTeam := isTeamMembership(membership) || limitType == "team"
	if inTeam {
		for _, team := range teams {
			if team == nil || team.GetId() <= 0 || team.GetDeletedAtMs() > 0 {
				continue
			}
			scope.TeamID = team.GetId()
			role := team.GetRole()
			isAdmin := role == aiserverv1.TeamRole_TEAM_ROLE_OWNER || role == aiserverv1.TeamRole_TEAM_ROLE_FREE_OWNER
			if team.GetAdminOnlyUsagePricing() && !isAdmin {
				scope.Editable = false
				scope.ReadOnlyReason = "Only team admins can change spend limits for this team."
			}
			break
		}
		return scope
	}

	switch membership {
	case "free", "hobby", "free_trial", "express":
		scope.Editable = false
		scope.ReadOnlyReason = "On-demand spending requires an active paid Cursor plan."
		return scope
	case "pro", "pro_plus":
		if subStatus == "trialing" {
			scope.Editable = false
			scope.ReadOnlyReason = "On-demand spending is unavailable while on a trial subscription."
			return scope
		}
	}

	if isUnifiedXSubscriptionMembership(membership) {
		scope.PersonalScope = true
	}

	return scope
}

func fetchCursorSpendLimitScope(ctx context.Context, authorization string, stripe cursorStripeProfile, usage *aiserverv1.GetCurrentPeriodUsageResponse) cursorSpendLimitScope {
	membership := strings.ToLower(strings.TrimSpace(firstNonEmpty(stripe.MembershipType, stripe.IndividualMembershipType)))
	limitType := ""
	if usage != nil && usage.GetSpendLimitUsage() != nil {
		limitType = strings.ToLower(strings.TrimSpace(usage.GetSpendLimitUsage().GetLimitType()))
	}
	var teams []*aiserverv1.Team
	if isTeamMembership(membership) || limitType == "team" {
		teamsResp := &aiserverv1.GetTeamsResponse{}
		if err := postCursorDashboardDetailed(ctx, authorization, cursorGetTeamsPath, &aiserverv1.GetTeamsRequest{}, teamsResp); err == nil {
			teams = teamsResp.GetTeams()
		}
	}
	return resolveCursorSpendLimitScope(stripe, usage, teams)
}

func buildGetHardLimitProtoRequest(teamID int32, personalScope bool) *aiserverv1.GetHardLimitRequest {
	req := &aiserverv1.GetHardLimitRequest{}
	if teamID > 0 {
		req.TeamId = proto.Int32(teamID)
	} else if personalScope {
		req.PersonalScope = proto.Bool(true)
	}
	return req
}

func normalizeCursorSpendLimitMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case CursorSpendLimitModeDisabled, "off", "no-usage-based":
		return CursorSpendLimitModeDisabled, nil
	case CursorSpendLimitModeFixed:
		return CursorSpendLimitModeFixed, nil
	case CursorSpendLimitModeUnlimited, "no_limit", "none":
		return CursorSpendLimitModeUnlimited, nil
	default:
		return "", fmt.Errorf("spend limit mode must be disabled, fixed, or unlimited")
	}
}

func buildSetHardLimitProtoRequest(req CursorAccountSpendLimitRequest, teamID int32, personalScope bool) (*aiserverv1.SetHardLimitRequest, error) {
	mode, err := normalizeCursorSpendLimitMode(req.Mode)
	if err != nil {
		return nil, err
	}
	protoReq := &aiserverv1.SetHardLimitRequest{
		PreserveHardLimitPerUser: true,
	}
	if teamID > 0 {
		protoReq.TeamId = proto.Int32(teamID)
	} else if personalScope {
		protoReq.PersonalScope = proto.Bool(true)
	}

	switch mode {
	case CursorSpendLimitModeDisabled:
		protoReq.HardLimit = 0
		protoReq.NoUsageBasedAllowed = true
	case CursorSpendLimitModeUnlimited:
		protoReq.HardLimit = CursorUnlimitedHardLimitDollars
		protoReq.NoUsageBasedAllowed = false
	case CursorSpendLimitModeFixed:
		if req.HardLimitDollars <= 0 {
			return nil, fmt.Errorf("fixed spend limit must be a positive whole dollar amount (use disabled for $0)")
		}
		if req.HardLimitDollars >= CursorUnlimitedHardLimitDollars {
			return nil, fmt.Errorf("fixed spend limit is too large (use unlimited instead)")
		}
		protoReq.HardLimit = req.HardLimitDollars
		protoReq.NoUsageBasedAllowed = false
	}
	return protoReq, nil
}

func decodeHardLimitResponse(accountID string, resp *aiserverv1.GetHardLimitResponse, scope cursorSpendLimitScope) CursorAccountSpendLimit {
	out := CursorAccountSpendLimit{
		AccountID:          strings.TrimSpace(accountID),
		Mode:               CursorSpendLimitModeDisabled,
		Editable:           scope.Editable,
		ReadOnlyReason:     scope.ReadOnlyReason,
		TeamID:             scope.TeamID,
		PersonalScope:      scope.PersonalScope,
		MembershipType:     scope.MembershipType,
		SubscriptionStatus: scope.SubscriptionStatus,
	}
	if resp == nil {
		return out
	}
	out.NoUsageBasedAllowed = resp.GetNoUsageBasedAllowed()
	out.DisabledByOrg = resp.GetOnDemandSpendDisabledByOrganization()
	out.IsDynamicTeamLimit = resp.GetIsDynamicTeamLimit()
	if out.DisabledByOrg {
		out.Editable = false
		if out.ReadOnlyReason == "" {
			out.ReadOnlyReason = "On-demand spending is disabled by your organization."
		}
	}

	raw := resp.GetHardLimit()
	switch {
	case resp.GetNoUsageBasedAllowed() || raw <= 0:
		out.Mode = CursorSpendLimitModeDisabled
		out.HardLimitDollars = 0
		out.NoUsageBasedAllowed = true
	case raw >= CursorUnlimitedHardLimitDollars:
		out.Mode = CursorSpendLimitModeUnlimited
		out.HardLimitDollars = CursorUnlimitedHardLimitDollars
		out.NoUsageBasedAllowed = false
	default:
		out.Mode = CursorSpendLimitModeFixed
		out.HardLimitDollars = raw
		out.NoUsageBasedAllowed = false
	}
	return out
}

func fetchCursorAccountSpendLimit(ctx context.Context, authorization, accountID string) (CursorAccountSpendLimit, error) {
	var stripe cursorStripeProfile
	_ = getCursorStripeProfile(ctx, authorization, &stripe)
	scope := fetchCursorSpendLimitScope(ctx, authorization, stripe, nil)

	resp := &aiserverv1.GetHardLimitResponse{}
	if err := postCursorDashboardDetailed(ctx, authorization, cursorGetHardLimitPath, buildGetHardLimitProtoRequest(scope.TeamID, scope.PersonalScope), resp); err != nil {
		return CursorAccountSpendLimit{}, err
	}
	return decodeHardLimitResponse(accountID, resp, scope), nil
}

func shouldClearCooldownAfterSpendLimitChange(prev *CursorAccountSpendLimit, nextMode string, nextDollars int32) bool {
	switch nextMode {
	case CursorSpendLimitModeUnlimited:
		return true
	case CursorSpendLimitModeFixed:
		if prev == nil {
			return nextDollars > 0
		}
		if prev.Mode == CursorSpendLimitModeDisabled {
			return nextDollars > 0
		}
		if prev.Mode == CursorSpendLimitModeFixed && nextDollars > prev.HardLimitDollars {
			return true
		}
		return false
	default:
		return false
	}
}

func applyCursorAccountSpendLimit(ctx context.Context, authorization, accountID string, req CursorAccountSpendLimitRequest) (CursorAccountSpendLimit, error) {
	var stripe cursorStripeProfile
	_ = getCursorStripeProfile(ctx, authorization, &stripe)
	scope := fetchCursorSpendLimitScope(ctx, authorization, stripe, nil)

	teamID := req.TeamID
	if teamID <= 0 {
		teamID = scope.TeamID
	} else {
		scope.TeamID = teamID
	}
	personalScope := req.PersonalScope || (teamID <= 0 && scope.PersonalScope)
	scope.PersonalScope = personalScope

	// Read previous limit best-effort so we know whether this change raised or
	// enabled on-demand spending (which should clear stale quota cooldowns).
	var previous *CursorAccountSpendLimit
	prevResp := &aiserverv1.GetHardLimitResponse{}
	if err := postCursorDashboardDetailed(ctx, authorization, cursorGetHardLimitPath, buildGetHardLimitProtoRequest(teamID, personalScope), prevResp); err == nil {
		decoded := decodeHardLimitResponse(accountID, prevResp, scope)
		previous = &decoded
	}

	protoReq, err := buildSetHardLimitProtoRequest(req, teamID, personalScope)
	if err != nil {
		return CursorAccountSpendLimit{}, err
	}
	if err := postCursorDashboardDetailed(ctx, authorization, cursorSetHardLimitPath, protoReq, &aiserverv1.SetHardLimitResponse{}); err != nil {
		return CursorAccountSpendLimit{}, err
	}

	// Read back the authoritative saved state from Cursor.
	afterResp := &aiserverv1.GetHardLimitResponse{}
	var result CursorAccountSpendLimit
	if err := postCursorDashboardDetailed(ctx, authorization, cursorGetHardLimitPath, buildGetHardLimitProtoRequest(teamID, personalScope), afterResp); err == nil {
		result = decodeHardLimitResponse(accountID, afterResp, scope)
	} else {
		result = decodeHardLimitResponse(accountID, &aiserverv1.GetHardLimitResponse{
			HardLimit:           protoReq.GetHardLimit(),
			NoUsageBasedAllowed: protoReq.GetNoUsageBasedAllowed(),
		}, scope)
	}

	if accountID != "" && shouldClearCooldownAfterSpendLimitChange(previous, result.Mode, result.HardLimitDollars) {
		result.ClearedQuotaCooldowns = modeladapter.ClearAllCursorAccountExhausted(accountID)
	}
	return result, nil
}

func postCursorDashboardDetailed(ctx context.Context, authorization, path string, request proto.Message, response proto.Message) error {
	body, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cursorBackendRoot, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	upstream.ApplyCursorClientHeaders(req, authorization, "application/proto")
	req.Header.Set("accept", "application/proto")
	resp, err := doCursorControlPlaneRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorHealthMaxBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseCursorDashboardError(path, resp.StatusCode, raw)
	}
	if response == nil {
		return nil
	}
	return proto.Unmarshal(raw, response)
}

func parseCursorDashboardError(path string, statusCode int, raw []byte) *cursorDashboardRPCError {
	rpcErr := &cursorDashboardRPCError{
		Path:       path,
		StatusCode: statusCode,
		AuthReject: statusCode == http.StatusUnauthorized,
	}

	type connectDetailEntry struct {
		Type  string `json:"type"`
		Value string `json:"value"`
		Debug struct {
			Error   string `json:"error"`
			Details struct {
				Title          string            `json:"title"`
				Detail         string            `json:"detail"`
				AdditionalInfo map[string]string `json:"additionalInfo"`
			} `json:"details"`
		} `json:"debug"`
	}
	type connectEnvelope struct {
		Code    string               `json:"code"`
		Message string               `json:"message"`
		Details []connectDetailEntry `json:"details"`
		Error   *struct {
			Code    string               `json:"code"`
			Message string               `json:"message"`
			Details []connectDetailEntry `json:"details"`
		} `json:"error"`
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var env connectEnvelope
		if json.Unmarshal(trimmed, &env) == nil {
			rpcErr.Code = strings.ToLower(strings.TrimSpace(env.Code))
			if rpcErr.Code == "" && env.Error != nil {
				rpcErr.Code = strings.ToLower(strings.TrimSpace(env.Error.Code))
			}
			details := env.Details
			if env.Error != nil && len(env.Error.Details) > 0 {
				details = append(details, env.Error.Details...)
			}
			for _, entry := range details {
				if entry.Debug.Error != "" && rpcErr.ErrorEnum == "" {
					rpcErr.ErrorEnum = strings.TrimSpace(entry.Debug.Error)
				}
				if entry.Debug.Details.Title != "" && rpcErr.Title == "" {
					rpcErr.Title = strings.TrimSpace(entry.Debug.Details.Title)
				}
				if entry.Debug.Details.Detail != "" && rpcErr.Detail == "" {
					rpcErr.Detail = strings.TrimSpace(entry.Debug.Details.Detail)
				}
				if entry.Debug.Details.AdditionalInfo["prepaidAutoTopUpPaymentMethodRequired"] == "true" {
					rpcErr.PaymentReq = true
				}
				if decoded, ok := decodeBase64Any(entry.Value); ok {
					errProto := &aiserverv1.ErrorDetails{}
					if proto.Unmarshal(decoded, errProto) == nil {
						if errProto.GetError() != aiserverv1.ErrorDetails_ERROR_UNSPECIFIED && rpcErr.ErrorEnum == "" {
							rpcErr.ErrorEnum = errProto.GetError().String()
						}
						if custom := errProto.GetDetails(); custom != nil {
							if custom.GetTitle() != "" && rpcErr.Title == "" {
								rpcErr.Title = strings.TrimSpace(custom.GetTitle())
							}
							if custom.GetDetail() != "" && rpcErr.Detail == "" {
								rpcErr.Detail = strings.TrimSpace(custom.GetDetail())
							}
							if custom.GetAdditionalInfo()["prepaidAutoTopUpPaymentMethodRequired"] == "true" {
								rpcErr.PaymentReq = true
							}
						}
					}
				}
			}
			if rpcErr.Detail == "" {
				msg := strings.TrimSpace(firstNonEmpty(env.Message, func() string {
					if env.Error != nil {
						return env.Error.Message
					}
					return ""
				}()))
				if msg != "" && !strings.EqualFold(msg, "Error") {
					rpcErr.Detail = msg
				}
			}
		}
	}

	if rpcErr.Code == "unauthenticated" ||
		rpcErr.ErrorEnum == "ERROR_NOT_LOGGED_IN" ||
		rpcErr.ErrorEnum == "ERROR_USER_NOT_LOGGED_IN" {
		rpcErr.AuthReject = true
	}
	if rpcErr.Code == "permission_denied" || rpcErr.ErrorEnum == "ERROR_UNAUTHORIZED" {
		rpcErr.AuthReject = false
	}
	lowerDetail := strings.ToLower(rpcErr.Title + " " + rpcErr.Detail)
	if strings.Contains(lowerDetail, "payment method") || strings.Contains(lowerDetail, "add a card") {
		rpcErr.PaymentReq = true
	}
	return rpcErr
}

func decodeBase64Any(raw string) ([]byte, bool) {
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

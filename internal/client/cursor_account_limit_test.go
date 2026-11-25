package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cursor/gen/aiserverv1"
	modeladapter "cursor/internal/backend/agent/model"

	"google.golang.org/protobuf/proto"
)

func TestBuildSetHardLimitProtoRequestModesAndValidation(t *testing.T) {
	// Disabled ($0)
	req, err := buildSetHardLimitProtoRequest(CursorAccountSpendLimitRequest{
		Mode: CursorSpendLimitModeDisabled,
	}, 0, false)
	if err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if req.GetHardLimit() != 0 || !req.GetNoUsageBasedAllowed() || !req.GetPreserveHardLimitPerUser() {
		t.Fatalf("disabled proto = %+v", req)
	}
	if req.TeamId != nil || req.PersonalScope != nil {
		t.Fatalf("unexpected scope fields on legacy personal request: %+v", req)
	}

	// Fixed dollar amount
	req, err = buildSetHardLimitProtoRequest(CursorAccountSpendLimitRequest{
		Mode:             CursorSpendLimitModeFixed,
		HardLimitDollars: 150,
	}, 0, true)
	if err != nil {
		t.Fatalf("fixed: %v", err)
	}
	if req.GetHardLimit() != 150 || req.GetNoUsageBasedAllowed() || !req.GetPreserveHardLimitPerUser() || !req.GetPersonalScope() {
		t.Fatalf("fixed proto = %+v", req)
	}

	// Unlimited (INT32_MAX) on team scope
	req, err = buildSetHardLimitProtoRequest(CursorAccountSpendLimitRequest{
		Mode: CursorSpendLimitModeUnlimited,
	}, 42, true)
	if err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	if req.GetHardLimit() != CursorUnlimitedHardLimitDollars || req.GetNoUsageBasedAllowed() || !req.GetPreserveHardLimitPerUser() {
		t.Fatalf("unlimited proto = %+v", req)
	}
	if req.GetTeamId() != 42 || req.PersonalScope != nil {
		t.Fatalf("team scope must override personal_scope: %+v", req)
	}

	// Invalid fixed amounts
	for _, invalid := range []CursorAccountSpendLimitRequest{
		{Mode: CursorSpendLimitModeFixed, HardLimitDollars: 0},
		{Mode: CursorSpendLimitModeFixed, HardLimitDollars: -10},
		{Mode: CursorSpendLimitModeFixed, HardLimitDollars: math.MaxInt32},
		{Mode: "bogus"},
	} {
		if _, err := buildSetHardLimitProtoRequest(invalid, 0, false); err == nil {
			t.Fatalf("expected error for invalid request %+v", invalid)
		}
	}
}

func TestDecodeHardLimitResponseModesAndOrgBlock(t *testing.T) {
	scope := cursorSpendLimitScope{Editable: true, MembershipType: "pro", SubscriptionStatus: "active"}

	disabled := decodeHardLimitResponse("acc-1", &aiserverv1.GetHardLimitResponse{
		HardLimit:           50,
		NoUsageBasedAllowed: true,
	}, scope)
	if disabled.Mode != CursorSpendLimitModeDisabled || disabled.HardLimitDollars != 0 || !disabled.NoUsageBasedAllowed {
		t.Fatalf("disabled = %+v", disabled)
	}

	fixed := decodeHardLimitResponse("acc-1", &aiserverv1.GetHardLimitResponse{
		HardLimit:           75,
		NoUsageBasedAllowed: false,
	}, scope)
	if fixed.Mode != CursorSpendLimitModeFixed || fixed.HardLimitDollars != 75 || fixed.NoUsageBasedAllowed || !fixed.Editable {
		t.Fatalf("fixed = %+v", fixed)
	}

	unlimited := decodeHardLimitResponse("acc-1", &aiserverv1.GetHardLimitResponse{
		HardLimit:           CursorUnlimitedHardLimitDollars,
		NoUsageBasedAllowed: false,
	}, scope)
	if unlimited.Mode != CursorSpendLimitModeUnlimited || unlimited.HardLimitDollars != CursorUnlimitedHardLimitDollars || unlimited.NoUsageBasedAllowed {
		t.Fatalf("unlimited = %+v", unlimited)
	}

	orgBlocked := decodeHardLimitResponse("acc-1", &aiserverv1.GetHardLimitResponse{
		HardLimit:                           0,
		NoUsageBasedAllowed:                 true,
		OnDemandSpendDisabledByOrganization: true,
	}, scope)
	if orgBlocked.Editable || !orgBlocked.DisabledByOrg || orgBlocked.ReadOnlyReason == "" {
		t.Fatalf("orgBlocked = %+v", orgBlocked)
	}
}

func TestResolveCursorSpendLimitScopePlansAndTeams(t *testing.T) {
	// Free plan cannot enable on-demand
	freeScope := resolveCursorSpendLimitScope(cursorStripeProfile{MembershipType: "free"}, nil, nil)
	if freeScope.Editable || freeScope.ReadOnlyReason == "" {
		t.Fatalf("free scope = %+v", freeScope)
	}

	// Pro trialing cannot enable on-demand
	trialScope := resolveCursorSpendLimitScope(cursorStripeProfile{MembershipType: "pro", SubscriptionStatus: "trialing"}, nil, nil)
	if trialScope.Editable {
		t.Fatalf("trial scope should not be editable: %+v", trialScope)
	}

	// Unified X Subscription uses personal_scope=true
	xScope := resolveCursorSpendLimitScope(cursorStripeProfile{MembershipType: "super", SubscriptionStatus: "active"}, nil, nil)
	if !xScope.Editable || !xScope.PersonalScope || xScope.TeamID != 0 {
		t.Fatalf("unified X scope = %+v", xScope)
	}

	// Enterprise team member with admin_only_usage_pricing=true is read-only
	memberScope := resolveCursorSpendLimitScope(
		cursorStripeProfile{MembershipType: "enterprise", SubscriptionStatus: "active"},
		nil,
		[]*aiserverv1.Team{{
			Id:                    77,
			Role:                  aiserverv1.TeamRole_TEAM_ROLE_MEMBER,
			AdminOnlyUsagePricing: true,
		}},
	)
	if memberScope.TeamID != 77 || memberScope.Editable || memberScope.ReadOnlyReason == "" {
		t.Fatalf("restricted team member scope = %+v", memberScope)
	}

	// Enterprise team owner can edit
	ownerScope := resolveCursorSpendLimitScope(
		cursorStripeProfile{MembershipType: "enterprise", SubscriptionStatus: "active"},
		nil,
		[]*aiserverv1.Team{{
			Id:                    77,
			Role:                  aiserverv1.TeamRole_TEAM_ROLE_OWNER,
			AdminOnlyUsagePricing: true,
		}},
	)
	if ownerScope.TeamID != 77 || !ownerScope.Editable {
		t.Fatalf("team owner scope = %+v", ownerScope)
	}
}

func TestApplyCursorAccountSpendLimitRaiseLowerUnlimitedAndDisable(t *testing.T) {
	var (
		currentHardLimit int32 = 50
		currentDisabled        = false
		lastSetReq       *aiserverv1.SetHardLimitRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/full_stripe_profile":
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"membershipType":"pro","subscriptionStatus":"active"}`))
		case cursorGetHardLimitPath:
			raw, _ := proto.Marshal(&aiserverv1.GetHardLimitResponse{
				HardLimit:           currentHardLimit,
				NoUsageBasedAllowed: currentDisabled,
			})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		case cursorSetHardLimitPath:
			body, _ := io.ReadAll(r.Body)
			req := &aiserverv1.SetHardLimitRequest{}
			if err := proto.Unmarshal(body, req); err != nil {
				t.Errorf("unmarshal SetHardLimitRequest: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			lastSetReq = req
			currentHardLimit = req.GetHardLimit()
			currentDisabled = req.GetNoUsageBasedAllowed()
			raw, _ := proto.Marshal(&aiserverv1.SetHardLimitResponse{})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	previousRoot := cursorBackendRoot
	cursorBackendRoot = srv.URL
	t.Cleanup(func() { cursorBackendRoot = previousRoot })

	accountID := "test-spend-limit-acct"
	t.Cleanup(func() { modeladapter.ClearAllCursorAccountExhausted(accountID) })

	// 1. Raise $50 -> $200 clears active quota cooldowns
	modeladapter.MarkCursorAccountModelExhausted(accountID, "claude-4.5-sonnet")
	raised, err := applyCursorAccountSpendLimit(context.Background(), "Bearer tok", accountID, CursorAccountSpendLimitRequest{
		Mode:             CursorSpendLimitModeFixed,
		HardLimitDollars: 200,
	})
	if err != nil {
		t.Fatalf("raise failed: %v", err)
	}
	if raised.Mode != CursorSpendLimitModeFixed || raised.HardLimitDollars != 200 || raised.ClearedQuotaCooldowns != 1 {
		t.Fatalf("raised = %+v", raised)
	}
	if lastSetReq.GetHardLimit() != 200 || lastSetReq.GetNoUsageBasedAllowed() || !lastSetReq.GetPreserveHardLimitPerUser() {
		t.Fatalf("wire raise req = %+v", lastSetReq)
	}

	// 2. Lower $200 -> $25 does NOT clear cooldowns
	modeladapter.MarkCursorAccountModelExhausted(accountID, "claude-4.5-sonnet")
	lowered, err := applyCursorAccountSpendLimit(context.Background(), "Bearer tok", accountID, CursorAccountSpendLimitRequest{
		Mode:             CursorSpendLimitModeFixed,
		HardLimitDollars: 25,
	})
	if err != nil {
		t.Fatalf("lower failed: %v", err)
	}
	if lowered.Mode != CursorSpendLimitModeFixed || lowered.HardLimitDollars != 25 || lowered.ClearedQuotaCooldowns != 0 {
		t.Fatalf("lowered = %+v", lowered)
	}

	// 3. Switch to Unlimited (INT32_MAX) clears cooldowns
	unlimited, err := applyCursorAccountSpendLimit(context.Background(), "Bearer tok", accountID, CursorAccountSpendLimitRequest{
		Mode: CursorSpendLimitModeUnlimited,
	})
	if err != nil {
		t.Fatalf("unlimited failed: %v", err)
	}
	if unlimited.Mode != CursorSpendLimitModeUnlimited || unlimited.HardLimitDollars != CursorUnlimitedHardLimitDollars || unlimited.ClearedQuotaCooldowns != 1 {
		t.Fatalf("unlimited = %+v", unlimited)
	}

	// 4. Disable ($0) sets no_usage_based_allowed=true
	disabled, err := applyCursorAccountSpendLimit(context.Background(), "Bearer tok", accountID, CursorAccountSpendLimitRequest{
		Mode: CursorSpendLimitModeDisabled,
	})
	if err != nil {
		t.Fatalf("disable failed: %v", err)
	}
	if disabled.Mode != CursorSpendLimitModeDisabled || disabled.HardLimitDollars != 0 || !disabled.NoUsageBasedAllowed {
		t.Fatalf("disabled = %+v", disabled)
	}
}

func TestFetchCursorAccountPlanPopulatesHardLimitAndUnlimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/full_stripe_profile":
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"membershipType":"ultra","subscriptionStatus":"active"}`))
		case "/aiserver.v1.DashboardService/GetPlanInfo":
			price := "$200/mo"
			raw, _ := proto.Marshal(&aiserverv1.GetPlanInfoResponse{
				PlanInfo: &aiserverv1.GetPlanInfoResponse_PlanInfo{
					PlanName:            "Ultra",
					IncludedAmountCents: 40000,
					Price:               &price,
				},
			})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
			totalPct := 100.0
			raw, _ := proto.Marshal(&aiserverv1.GetCurrentPeriodUsageResponse{
				PlanUsage: &aiserverv1.GetCurrentPeriodUsageResponse_PlanUsage{
					Limit:            40000,
					Remaining:        0,
					TotalPercentUsed: &totalPct,
				},
				SpendLimitUsage: &aiserverv1.GetCurrentPeriodUsageResponse_SpendLimitUsage{
					IndividualUsed: 1234,
				},
			})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		case cursorGetHardLimitPath:
			raw, _ := proto.Marshal(&aiserverv1.GetHardLimitResponse{
				HardLimit:           CursorUnlimitedHardLimitDollars,
				NoUsageBasedAllowed: false,
			})
			w.Header().Set("content-type", "application/proto")
			_, _ = w.Write(raw)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	previousRoot := cursorBackendRoot
	cursorBackendRoot = srv.URL
	t.Cleanup(func() { cursorBackendRoot = previousRoot })

	plan, err := fetchCursorAccountPlan(context.Background(), "Bearer tok")
	if err != nil {
		t.Fatalf("fetchCursorAccountPlan: %v", err)
	}
	if !plan.OnDemandLimitKnown || plan.OnDemandMode != CursorSpendLimitModeUnlimited {
		t.Fatalf("expected unlimited on-demand mode, got %+v", plan)
	}
	if plan.OnDemandUsedCents != 1234 || plan.OnDemandLimitCents != 0 {
		t.Fatalf("expected used=1234 and limitCents=0 for unlimited, got %+v", plan)
	}
	if !plan.OnDemandEditable {
		t.Fatalf("expected active ultra plan to be editable: %+v", plan)
	}
}

func TestParseCursorDashboardErrorMapsPaymentPermissionAndAuth(t *testing.T) {
	// Proto-encoded ErrorDetails with payment method requirement
	errProto, _ := proto.Marshal(&aiserverv1.ErrorDetails{
		Error: aiserverv1.ErrorDetails_ERROR_CUSTOM_MESSAGE,
		Details: &aiserverv1.CustomErrorDetails{
			Title:  "Payment method required",
			Detail: "Add a payment method in billing settings.",
			AdditionalInfo: map[string]string{
				"prepaidAutoTopUpPaymentMethodRequired": "true",
			},
		},
	})
	payload, _ := json.Marshal(map[string]any{
		"code":    "failed_precondition",
		"message": "Error",
		"details": []map[string]any{{
			"type":  "aiserver.v1.ErrorDetails",
			"value": base64.RawStdEncoding.EncodeToString(errProto),
		}},
	})
	payErr := parseCursorDashboardError(cursorSetHardLimitPath, http.StatusPreconditionFailed, payload)
	if !payErr.PaymentReq || !strings.Contains(strings.ToLower(payErr.Error()), "payment method") {
		t.Fatalf("payment error = %+v (%s)", payErr, payErr.Error())
	}

	// Permission denied must NOT unwrap as auth rejection
	permErr := parseCursorDashboardError(cursorSetHardLimitPath, http.StatusForbidden, []byte(`{"code":"permission_denied","message":"Only admins can edit usage settings"}`))
	if errors.Is(permErr, errPlanAuthRejected) {
		t.Fatalf("permission_denied must not be classified as auth rejected")
	}
	if !strings.Contains(permErr.Error(), "Only admins can edit usage settings") {
		t.Fatalf("permErr = %q", permErr.Error())
	}

	// 401 unauthenticated unwraps as errPlanAuthRejected
	authErr := parseCursorDashboardError(cursorSetHardLimitPath, http.StatusUnauthorized, []byte(`{"code":"unauthenticated","message":"Not logged in"}`))
	if !errors.Is(authErr, errPlanAuthRejected) {
		t.Fatalf("401 must unwrap as errPlanAuthRejected")
	}
}

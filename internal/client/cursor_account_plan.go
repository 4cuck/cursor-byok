package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cursor/gen/aiserverv1"
	modeladapter "cursor/internal/backend/agent/model"
	"cursor/internal/backend/server/upstream"

	"google.golang.org/protobuf/proto"
)

var errPlanAuthRejected = errors.New("cursor plan lookup rejected credentials")

// CursorAccountPlan is the non-secret plan and included-usage snapshot for one
// Cursor account. Amounts are cents, matching Cursor's dashboard fields, plus
// Cursor 3.30's Named Models (apiPercentUsed) vs Auto/Grok/Composer
// (autoPercentUsed) and TotalIncluded (totalPercentUsed) percentages.
type CursorAccountPlan struct {
	PlanName            string   `json:"planName,omitempty"`
	MembershipType      string   `json:"membershipType,omitempty"`
	SubscriptionStatus  string   `json:"subscriptionStatus,omitempty"`
	Price               string   `json:"price,omitempty"`
	IncludedCents       int32    `json:"includedCents,omitempty"`
	TotalSpendCents     int32    `json:"totalSpendCents,omitempty"`
	BonusCents          int32    `json:"bonusCents,omitempty"`
	BonusTooltip        string   `json:"bonusTooltip,omitempty"`
	RemainingCents      int32    `json:"remainingCents,omitempty"`
	LimitCents          int32    `json:"limitCents,omitempty"`
	UsedCents           int32    `json:"usedCents,omitempty"`
	PercentUsed         float64  `json:"percentUsed"`
	TotalPercentKnown   bool     `json:"totalPercentKnown,omitempty"`
	AutoBucketKnown     bool     `json:"autoBucketKnown,omitempty"`
	AutoUsedCents       int32    `json:"autoUsedCents,omitempty"`
	AutoLimitCents      int32    `json:"autoLimitCents,omitempty"`
	AutoPercentUsed     float64  `json:"autoPercentUsed"`
	NamedBucketKnown    bool     `json:"namedBucketKnown,omitempty"`
	NamedUsedCents      int32    `json:"namedUsedCents,omitempty"`
	NamedLimitCents     int32    `json:"namedLimitCents,omitempty"`
	NamedPercentUsed    float64  `json:"namedPercentUsed"`
	HasBucketBreakdown  bool     `json:"hasBucketBreakdown,omitempty"`
	OnDemandUsedCents   int32    `json:"onDemandUsedCents,omitempty"`
	OnDemandLimitCents  int32    `json:"onDemandLimitCents,omitempty"`
	BillingCycleEnd     int64    `json:"billingCycleEnd,omitempty"`
	DisplayMessage      string   `json:"displayMessage,omitempty"`
	NamedDisplayMessage string   `json:"namedDisplayMessage,omitempty"`
	AutoDisplayMessage  string   `json:"autoDisplayMessage,omitempty"`
	ExhaustedModels     []string `json:"exhaustedModels,omitempty"`
	UsageKnown          bool     `json:"usageKnown"`
	Message             string   `json:"message,omitempty"`
}

type cursorStripeProfile struct {
	MembershipType           string `json:"membershipType"`
	IndividualMembershipType string `json:"individualMembershipType"`
	SubscriptionStatus       string `json:"subscriptionStatus"`
}

// GetCursorAccountPlan reads the plan name and included usage left for one
// saved account. An empty id uses the current account. Failures stay on the
// result so the card can show them without a thrown dialog.
func (s *ProxyService) GetCursorAccountPlan(accountID string) (CursorAccountPlan, error) {
	if s == nil || s.cursorAccount == nil {
		return CursorAccountPlan{Message: "Cursor account service is not initialized."}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cursorHealthTimeout)
	defer cancel()
	return s.loadCursorAccountPlan(ctx, accountID)
}

// ClearCursorAccountQuotaCooldown clears active per-(account, model) quota
// cooldowns for an account (or all accounts when accountID is empty).
func (s *ProxyService) ClearCursorAccountQuotaCooldown(accountID string) (int, error) {
	return modeladapter.ClearAllCursorAccountExhausted(accountID), nil
}

func (s *ProxyService) loadCursorAccountPlan(ctx context.Context, accountID string) (CursorAccountPlan, error) {
	accountID = strings.TrimSpace(accountID)
	resolvedID := accountID
	if resolvedID == "" {
		resolvedID = strings.TrimSpace(s.cursorAccount.CurrentAccountID())
	}
	authorization, err := s.cursorAccount.AuthorizationFor(ctx, accountID)
	if err != nil && resolvedID != "" {
		if renewed, refreshErr := s.cursorAccount.ForceRefresh(ctx, resolvedID); refreshErr == nil {
			authorization = renewed
			err = nil
		}
	}
	if err != nil {
		return CursorAccountPlan{Message: "Could not authorize this account."}, nil
	}
	plan, err := fetchCursorAccountPlan(ctx, authorization)
	if err != nil && resolvedID != "" && (errors.Is(err, errPlanAuthRejected) || s.cursorAccount.UsesAPIKey(resolvedID)) {
		if renewed, refreshErr := s.cursorAccount.ForceRefresh(ctx, resolvedID); refreshErr == nil {
			plan, err = fetchCursorAccountPlan(ctx, renewed)
		}
	}
	if err != nil {
		return CursorAccountPlan{Message: "Cursor did not return plan or usage for this key."}, nil
	}
	if resolvedID != "" {
		exhausted := modeladapter.ListCursorAccountExhaustedModels(resolvedID)
		if len(exhausted) > 0 {
			names := make([]string, 0, len(exhausted))
			for _, item := range exhausted {
				names = append(names, item.ModelID)
			}
			plan.ExhaustedModels = names
		}
	}
	return plan, nil
}

func fetchCursorAccountPlan(ctx context.Context, authorization string) (CursorAccountPlan, error) {
	var planInfo *aiserverv1.GetPlanInfoResponse
	var usage *aiserverv1.GetCurrentPeriodUsageResponse
	var stripe cursorStripeProfile
	var failures int
	var authRejected bool

	info := &aiserverv1.GetPlanInfoResponse{}
	if err := postCursorDashboard(ctx, authorization, "/aiserver.v1.DashboardService/GetPlanInfo", &aiserverv1.GetPlanInfoRequest{}, info); err != nil {
		failures++
		if errors.Is(err, errPlanAuthRejected) {
			authRejected = true
		}
	} else {
		planInfo = info
	}

	period := &aiserverv1.GetCurrentPeriodUsageResponse{}
	if err := postCursorDashboard(ctx, authorization, "/aiserver.v1.DashboardService/GetCurrentPeriodUsage", &aiserverv1.GetCurrentPeriodUsageRequest{}, period); err != nil {
		failures++
		if errors.Is(err, errPlanAuthRejected) {
			authRejected = true
		}
	} else {
		usage = period
	}

	if err := getCursorStripeProfile(ctx, authorization, &stripe); err != nil {
		failures++
		if errors.Is(err, errPlanAuthRejected) {
			authRejected = true
		}
	}

	if failures == 3 {
		if authRejected {
			return CursorAccountPlan{}, fmt.Errorf("cursor plan lookup failed: %w", errPlanAuthRejected)
		}
		return CursorAccountPlan{}, fmt.Errorf("cursor plan lookup failed")
	}
	return buildCursorAccountPlan(planInfo, usage, stripe), nil
}

func doCursorControlPlaneRequest(req *http.Request) (*http.Response, error) {
	resp, err := cursorHealthHTTPClient.Do(req)
	if err == nil {
		return resp, nil
	}
	clone := req.Clone(req.Context())
	if req.GetBody != nil {
		if body, bodyErr := req.GetBody(); bodyErr == nil {
			clone.Body = body
		}
	}
	return cursorModelsDirectHTTPClient.Do(clone)
}

func postCursorDashboard(ctx context.Context, authorization, path string, request proto.Message, response proto.Message) error {
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
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("cursor dashboard returned HTTP %d: %w", resp.StatusCode, errPlanAuthRejected)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cursor dashboard returned HTTP %d", resp.StatusCode)
	}
	return proto.Unmarshal(raw, response)
}

func getCursorStripeProfile(ctx context.Context, authorization string, profile *cursorStripeProfile) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cursorBackendRoot, "/")+"/auth/full_stripe_profile", nil)
	if err != nil {
		return err
	}
	upstream.ApplyCursorClientHeaders(req, authorization, "")
	req.Header.Set("accept", "application/json")
	resp, err := doCursorControlPlaneRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorHealthMaxBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("stripe profile returned HTTP %d: %w", resp.StatusCode, errPlanAuthRejected)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("stripe profile returned HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, profile)
}

func buildCursorAccountPlan(info *aiserverv1.GetPlanInfoResponse, usage *aiserverv1.GetCurrentPeriodUsageResponse, stripe cursorStripeProfile) CursorAccountPlan {
	plan := CursorAccountPlan{}
	membership := strings.TrimSpace(stripe.MembershipType)
	if membership == "" {
		membership = strings.TrimSpace(stripe.IndividualMembershipType)
	}
	plan.MembershipType = membership
	plan.SubscriptionStatus = strings.TrimSpace(stripe.SubscriptionStatus)

	if info != nil && info.GetPlanInfo() != nil {
		details := info.GetPlanInfo()
		plan.PlanName = strings.TrimSpace(details.GetPlanName())
		plan.Price = strings.TrimSpace(details.GetPrice())
		plan.IncludedCents = details.GetIncludedAmountCents()
		if end := details.GetBillingCycleEnd(); end > 0 {
			plan.BillingCycleEnd = end
		}
	}
	if plan.PlanName == "" {
		plan.PlanName = displayPlanName(membership)
	}

	if usage != nil {
		if usage.GetBillingCycleEnd() > 0 {
			plan.BillingCycleEnd = usage.GetBillingCycleEnd()
		}
		plan.DisplayMessage = strings.TrimSpace(usage.GetDisplayMessage())
		plan.NamedDisplayMessage = strings.TrimSpace(usage.GetNamedModelSelectedDisplayMessage())
		plan.AutoDisplayMessage = strings.TrimSpace(usage.GetAutoModelSelectedDisplayMessage())

		if spend := usage.GetSpendLimitUsage(); spend != nil {
			if spend.GetIndividualLimit() > 0 {
				plan.OnDemandLimitCents = spend.GetIndividualLimit()
				used := spend.GetIndividualLimit() - spend.GetIndividualRemaining()
				if spend.GetIndividualUsed() > 0 {
					used = spend.GetIndividualUsed()
				}
				if used < 0 {
					used = 0
				}
				plan.OnDemandUsedCents = used
			} else if spend.GetOverallLimit() > 0 {
				plan.OnDemandLimitCents = spend.GetOverallLimit()
				used := spend.GetOverallLimit() - spend.GetOverallRemaining()
				if used < 0 {
					used = 0
				}
				plan.OnDemandUsedCents = used
			}
		}

		if pu := usage.GetPlanUsage(); pu != nil {
			plan.TotalSpendCents = pu.GetTotalSpend()
			plan.BonusCents = pu.GetBonusSpend()
			plan.BonusTooltip = strings.TrimSpace(pu.GetBonusTooltip())

			if pu.AutoSpend != nil || pu.AutoLimit != nil || pu.AutoPercentUsed != nil {
				plan.AutoBucketKnown = true
				plan.AutoUsedCents = pu.GetAutoSpend()
				plan.AutoLimitCents = pu.GetAutoLimit()
				plan.AutoPercentUsed = clampPercent(pu.GetAutoPercentUsed())
				if pu.AutoPercentUsed == nil && plan.AutoLimitCents > 0 && plan.AutoUsedCents > 0 {
					plan.AutoPercentUsed = clampPercent(float64(plan.AutoUsedCents) / float64(plan.AutoLimitCents) * 100)
				}
			}
			if pu.ApiSpend != nil || pu.ApiLimit != nil || pu.ApiPercentUsed != nil {
				plan.NamedBucketKnown = true
				plan.NamedUsedCents = pu.GetApiSpend()
				plan.NamedLimitCents = pu.GetApiLimit()
				plan.NamedPercentUsed = clampPercent(pu.GetApiPercentUsed())
				if pu.ApiPercentUsed == nil && plan.NamedLimitCents > 0 && plan.NamedUsedCents > 0 {
					plan.NamedPercentUsed = clampPercent(float64(plan.NamedUsedCents) / float64(plan.NamedLimitCents) * 100)
				}
			}
			if plan.NamedBucketKnown || plan.AutoBucketKnown {
				plan.HasBucketBreakdown = true
			}
			if pu.TotalPercentUsed != nil {
				plan.TotalPercentKnown = true
				plan.UsageKnown = true
				plan.PercentUsed = clampPercent(pu.GetTotalPercentUsed())
			}
		}

		remaining, limit, known := includedUsage(usage, plan.TotalPercentKnown)
		if known {
			plan.UsageKnown = true
			plan.RemainingCents = remaining
			plan.LimitCents = limit
			if plan.IncludedCents == 0 {
				plan.IncludedCents = limit
			}
			used := limit - remaining
			if used < 0 {
				used = 0
			}
			plan.UsedCents = used
			if !plan.TotalPercentKnown && limit > 0 {
				plan.PercentUsed = float64(used) / float64(limit) * 100
				if plan.PercentUsed < 0 {
					plan.PercentUsed = 0
				}
				if plan.PercentUsed > 100 && remaining >= 0 {
					plan.PercentUsed = 100
				}
			}
		}
	}
	if plan.PlanName == "" && !plan.UsageKnown && plan.SubscriptionStatus == "" {
		plan.Message = "Cursor did not return plan or usage for this key."
	}
	return plan
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// includedUsage resolves cents remaining/limit. When Cursor 3.30 already
// reported totalPercentUsed on planUsage (pooled quota without a cents limit),
// spendLimitUsage is an on-demand spend cap rather than included plan balance.
func includedUsage(usage *aiserverv1.GetCurrentPeriodUsageResponse, hasPooledPercent bool) (remaining int32, limit int32, known bool) {
	if usage == nil {
		return 0, 0, false
	}
	if plan := usage.GetPlanUsage(); plan != nil && plan.GetLimit() > 0 {
		return plan.GetRemaining(), plan.GetLimit(), true
	}
	if hasPooledPercent {
		return 0, 0, false
	}
	if spend := usage.GetSpendLimitUsage(); spend != nil {
		if spend.GetIndividualLimit() > 0 {
			return spend.GetIndividualRemaining(), spend.GetIndividualLimit(), true
		}
		if spend.GetOverallLimit() > 0 {
			return spend.GetOverallRemaining(), spend.GetOverallLimit(), true
		}
	}
	return 0, 0, false
}

func displayPlanName(membership string) string {
	switch strings.ToLower(strings.TrimSpace(membership)) {
	case "":
		return ""
	case "free", "hobby":
		return "Free"
	case "pro":
		return "Pro"
	case "pro_plus", "pro+":
		return "Pro+"
	case "ultra":
		return "Ultra"
	case "premium":
		return "Premium"
	case "business", "team":
		return "Business"
	case "enterprise":
		return "Enterprise"
	default:
		return membership
	}
}

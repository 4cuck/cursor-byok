package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cursor/gen/aiserverv1"

	"google.golang.org/protobuf/proto"
)

// CursorAccountPlan is the non-secret plan and included-usage snapshot for one
// Cursor account. Amounts are cents, matching Cursor's dashboard fields.
type CursorAccountPlan struct {
	PlanName           string  `json:"planName,omitempty"`
	MembershipType     string  `json:"membershipType,omitempty"`
	SubscriptionStatus string  `json:"subscriptionStatus,omitempty"`
	Price              string  `json:"price,omitempty"`
	IncludedCents      int32   `json:"includedCents,omitempty"`
	RemainingCents     int32   `json:"remainingCents,omitempty"`
	LimitCents         int32   `json:"limitCents,omitempty"`
	UsedCents          int32   `json:"usedCents,omitempty"`
	PercentUsed        float64 `json:"percentUsed,omitempty"`
	BillingCycleEnd    int64   `json:"billingCycleEnd,omitempty"`
	DisplayMessage     string  `json:"displayMessage,omitempty"`
	UsageKnown         bool    `json:"usageKnown"`
	Message            string  `json:"message,omitempty"`
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

func (s *ProxyService) loadCursorAccountPlan(ctx context.Context, accountID string) (CursorAccountPlan, error) {
	authorization, err := s.cursorAccount.AuthorizationFor(ctx, strings.TrimSpace(accountID))
	if err != nil {
		return CursorAccountPlan{Message: "Could not authorize this account."}, nil
	}
	plan, err := fetchCursorAccountPlan(ctx, authorization)
	if err != nil {
		return CursorAccountPlan{Message: "Cursor did not return plan or usage for this key."}, nil
	}
	return plan, nil
}

func fetchCursorAccountPlan(ctx context.Context, authorization string) (CursorAccountPlan, error) {
	var planInfo *aiserverv1.GetPlanInfoResponse
	var usage *aiserverv1.GetCurrentPeriodUsageResponse
	var stripe cursorStripeProfile
	var failures int

	info := &aiserverv1.GetPlanInfoResponse{}
	if err := postCursorDashboard(ctx, authorization, "/aiserver.v1.DashboardService/GetPlanInfo", &aiserverv1.GetPlanInfoRequest{}, info); err != nil {
		failures++
	} else {
		planInfo = info
	}

	period := &aiserverv1.GetCurrentPeriodUsageResponse{}
	if err := postCursorDashboard(ctx, authorization, "/aiserver.v1.DashboardService/GetCurrentPeriodUsage", &aiserverv1.GetCurrentPeriodUsageRequest{}, period); err != nil {
		failures++
	} else {
		usage = period
	}

	if err := getCursorStripeProfile(ctx, authorization, &stripe); err != nil {
		failures++
	}

	if failures == 3 {
		return CursorAccountPlan{}, fmt.Errorf("cursor plan lookup failed")
	}
	return buildCursorAccountPlan(planInfo, usage, stripe), nil
}

func postCursorDashboard(ctx context.Context, authorization, path string, request proto.Message, response proto.Message) error {
	body, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", authorization)
	req.Header.Set("content-type", "application/proto")
	req.Header.Set("accept", "application/proto")
	req.Header.Set("connect-protocol-version", "1")
	resp, err := cursorHealthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorHealthMaxBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cursor dashboard returned HTTP %d", resp.StatusCode)
	}
	return proto.Unmarshal(raw, response)
}

func getCursorStripeProfile(ctx context.Context, authorization string, profile *cursorStripeProfile) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api2.cursor.sh/auth/full_stripe_profile", nil)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", authorization)
	req.Header.Set("accept", "application/json")
	resp, err := cursorHealthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorHealthMaxBytes))
	if err != nil {
		return err
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
		remaining, limit, known := includedUsage(usage)
		if known {
			plan.UsageKnown = true
			plan.RemainingCents = remaining
			plan.LimitCents = limit
			if plan.IncludedCents == 0 {
				plan.IncludedCents = limit
			}
			used := limit - remaining
			if remaining < 0 {
				used = limit - remaining
			}
			if used < 0 {
				used = 0
			}
			plan.UsedCents = used
			if limit > 0 {
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

func includedUsage(usage *aiserverv1.GetCurrentPeriodUsageResponse) (remaining int32, limit int32, known bool) {
	if usage == nil {
		return 0, 0, false
	}
	if plan := usage.GetPlanUsage(); plan != nil && plan.GetLimit() > 0 {
		return plan.GetRemaining(), plan.GetLimit(), true
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

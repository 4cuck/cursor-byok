package client

import (
	"testing"

	"cursor/gen/aiserverv1"
)

func TestBuildCursorAccountPlanUsesIncludedRemaining(t *testing.T) {
	price := "$20/mo"
	plan := buildCursorAccountPlan(
		&aiserverv1.GetPlanInfoResponse{
			PlanInfo: &aiserverv1.GetPlanInfoResponse_PlanInfo{
				PlanName:            "Pro",
				IncludedAmountCents: 2000,
				Price:               &price,
			},
		},
		&aiserverv1.GetCurrentPeriodUsageResponse{
			BillingCycleEnd: 1_800_000_000_000,
			PlanUsage: &aiserverv1.GetCurrentPeriodUsageResponse_PlanUsage{
				Remaining: 1500,
				Limit:     2000,
			},
		},
		cursorStripeProfile{MembershipType: "pro", SubscriptionStatus: "active"},
	)
	if plan.PlanName != "Pro" || plan.Price != "$20/mo" || plan.SubscriptionStatus != "active" {
		t.Fatalf("plan identity = %+v", plan)
	}
	if !plan.UsageKnown || plan.RemainingCents != 1500 || plan.LimitCents != 2000 || plan.UsedCents != 500 {
		t.Fatalf("usage = %+v", plan)
	}
	if plan.PercentUsed != 25 {
		t.Fatalf("percent used = %v", plan.PercentUsed)
	}
	if plan.BillingCycleEnd != 1_800_000_000_000 {
		t.Fatalf("cycle end = %d", plan.BillingCycleEnd)
	}
}

func TestBuildCursorAccountPlanFallsBackToMembership(t *testing.T) {
	plan := buildCursorAccountPlan(nil, nil, cursorStripeProfile{
		IndividualMembershipType: "premium",
		SubscriptionStatus:       "past_due",
	})
	if plan.PlanName != "Premium" || plan.MembershipType != "premium" || plan.SubscriptionStatus != "past_due" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.UsageKnown {
		t.Fatal("missing usage must not look like a zero balance")
	}
}

func TestBuildCursorAccountPlanUsesSpendLimitWhenIncludedIsMissing(t *testing.T) {
	limit := int32(5000)
	plan := buildCursorAccountPlan(nil, &aiserverv1.GetCurrentPeriodUsageResponse{
		SpendLimitUsage: &aiserverv1.GetCurrentPeriodUsageResponse_SpendLimitUsage{
			IndividualLimit:     &limit,
			IndividualRemaining: 1250,
		},
	}, cursorStripeProfile{})
	if !plan.UsageKnown || plan.RemainingCents != 1250 || plan.LimitCents != 5000 {
		t.Fatalf("spend limit usage = %+v", plan)
	}
}

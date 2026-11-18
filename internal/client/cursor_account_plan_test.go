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

func TestBuildCursorAccountPlanExtractsNamedAndAutoBuckets(t *testing.T) {
	autoSpend := int32(400)
	autoLimit := int32(2000)
	autoPct := 20.0
	apiSpend := int32(2000)
	apiLimit := int32(2000)
	apiPct := 100.0
	namedMsg := "Other Models limit reached; switched to Auto"
	bonusTip := "Promo bonus"

	plan := buildCursorAccountPlan(
		nil,
		&aiserverv1.GetCurrentPeriodUsageResponse{
			NamedModelSelectedDisplayMessage: &namedMsg,
			PlanUsage: &aiserverv1.GetCurrentPeriodUsageResponse_PlanUsage{
				Remaining:       1600,
				Limit:           4000,
				BonusSpend:      500,
				BonusTooltip:    &bonusTip,
				AutoSpend:       &autoSpend,
				AutoLimit:       &autoLimit,
				AutoPercentUsed: &autoPct,
				ApiSpend:        &apiSpend,
				ApiLimit:        &apiLimit,
				ApiPercentUsed:  &apiPct,
			},
		},
		cursorStripeProfile{MembershipType: "pro", SubscriptionStatus: "active"},
	)
	if !plan.HasBucketBreakdown || !plan.NamedBucketKnown || !plan.AutoBucketKnown {
		t.Fatalf("expected HasBucketBreakdown + known flags = true, got %+v", plan)
	}
	if plan.NamedUsedCents != 2000 || plan.NamedLimitCents != 2000 || plan.NamedPercentUsed != 100 {
		t.Fatalf("named bucket = %+v", plan)
	}
	if plan.AutoUsedCents != 400 || plan.AutoLimitCents != 2000 || plan.AutoPercentUsed != 20 {
		t.Fatalf("auto bucket = %+v", plan)
	}
	if plan.BonusCents != 500 || plan.BonusTooltip != "Promo bonus" || plan.NamedDisplayMessage != namedMsg {
		t.Fatalf("bonus/messages = %+v", plan)
	}
}

func TestBuildCursorAccountPlanKeepsPooledPercentWithoutMistakingOnDemandLimitForIncluded(t *testing.T) {
	autoPct := 100.0
	totalPct := 100.0
	onDemandLimit := int32(200000)
	plan := buildCursorAccountPlan(
		nil,
		&aiserverv1.GetCurrentPeriodUsageResponse{
			PlanUsage: &aiserverv1.GetCurrentPeriodUsageResponse_PlanUsage{
				AutoPercentUsed:  &autoPct,
				TotalPercentUsed: &totalPct,
			},
			SpendLimitUsage: &aiserverv1.GetCurrentPeriodUsageResponse_SpendLimitUsage{
				IndividualLimit:     &onDemandLimit,
				IndividualRemaining: 200000,
			},
		},
		cursorStripeProfile{MembershipType: "pro", SubscriptionStatus: "active"},
	)
	if !plan.UsageKnown || !plan.TotalPercentKnown || plan.PercentUsed != 100 {
		t.Fatalf("pooled percent plan = %+v, want 100%% used", plan)
	}
	if plan.LimitCents != 0 {
		t.Fatalf("on-demand spend cap leaked into included LimitCents: %+v", plan)
	}
	if plan.OnDemandLimitCents != 200000 || plan.OnDemandUsedCents != 0 {
		t.Fatalf("on-demand spend limit = %+v", plan)
	}
}

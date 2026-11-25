import assert from "node:assert/strict";
import test from "node:test";
import {
  autoBucketCaption,
  autoBucketUsedPercent,
  bonusUsageCaption,
  CURSOR_UNLIMITED_HARD_LIMIT_DOLLARS,
  credentialTypeLabel,
  formatPlanCents,
  formatSpendLimitSummary,
  namedBucketCaption,
  namedBucketUsedPercent,
  onDemandLimitMode,
  onDemandUsageCaption,
  remainingUsagePercent,
  renewalLabel,
  subscriptionStatusLabel,
  usageRemainingCaption,
  usedUsagePercent,
  validateSpendLimitInput,
} from "./cursorAccountPlan.js";

test("formats included cents as dollars", () => {
  assert.equal(formatPlanCents(2000), "$20.00");
  assert.equal(formatPlanCents(1540), "$15.40");
});

test("remaining and used percent reflect included usage and pooled percentages", () => {
  assert.equal(remainingUsagePercent({ usageKnown: true, remainingCents: 1500, limitCents: 2000 }), 75);
  assert.equal(usedUsagePercent({ usageKnown: true, remainingCents: 1500, limitCents: 2000 }), 25);
  assert.equal(remainingUsagePercent({ usageKnown: true, remainingCents: -100, limitCents: 2000 }), 0);
  assert.equal(remainingUsagePercent({ usageKnown: false, remainingCents: 0, limitCents: 0 }), 0);

  // Cursor 3.30 pooled usage with bonus spend keeps totalPercentUsed instead of showing 0% left.
  const pooled = {
    usageKnown: true,
    totalPercentKnown: true,
    percentUsed: 7.72,
    totalSpendCents: 3562,
    limitCents: 2000,
    bonusCents: 1562,
    remainingCents: 0,
  };
  assert.equal(usedUsagePercent(pooled), 8);
  assert.equal(remainingUsagePercent(pooled), 92);
  assert.equal(usageRemainingCaption(pooled), "$35.62 used ($20.00 plan + $15.62 bonus)");
});

test("caption distinguishes leftover usage from an overage", () => {
  assert.equal(
    usageRemainingCaption({ usageKnown: true, remainingCents: 1500, limitCents: 2000 }),
    "$15.00 left of $20.00",
  );
  assert.equal(
    usageRemainingCaption({ usageKnown: true, remainingCents: -250, limitCents: 2000 }),
    "$2.50 over the included amount",
  );
  assert.equal(usageRemainingCaption({ usageKnown: false }), "");
});

test("formats Named models and Auto / Grok % used breakdown matching Cursor 3.30", () => {
  const plan = {
    hasBucketBreakdown: true,
    namedBucketKnown: true,
    namedPercentUsed: 100,
    autoBucketKnown: true,
    autoPercentUsed: 5.27,
    bonusCents: 1562,
    onDemandLimitCents: 200000,
    onDemandUsedCents: 0,
  };
  assert.equal(namedBucketUsedPercent(plan), 100);
  assert.equal(autoBucketUsedPercent(plan), 5);
  assert.equal(namedBucketCaption(plan), "Named models: 100% used");
  assert.equal(autoBucketCaption(plan), "Auto / Grok: 5% used");
  assert.equal(bonusUsageCaption(plan), "+$15.62 free bonus");
  assert.equal(onDemandUsageCaption(plan), "On-demand limit: $0.00 / $2,000.00");
  assert.equal(credentialTypeLabel("api_key"), "API Key");
  assert.equal(credentialTypeLabel("oauth"), "OAuth");

  // 0% used bucket is still shown when known rather than hidden.
  const zeroNamed = {
    hasBucketBreakdown: true,
    namedBucketKnown: true,
    namedPercentUsed: 0,
    autoBucketKnown: true,
    autoPercentUsed: 0.44,
  };
  assert.equal(namedBucketUsedPercent(zeroNamed), 0);
  assert.equal(namedBucketCaption(zeroNamed), "Named models: 0% used");
  assert.equal(autoBucketUsedPercent(zeroNamed), 1);
  assert.equal(autoBucketCaption(zeroNamed), "Auto / Grok: 1% used");
});

test("subscription status stays readable", () => {
  assert.equal(subscriptionStatusLabel("past_due"), "Past due");
  assert.equal(subscriptionStatusLabel("active"), "Active");
  assert.equal(subscriptionStatusLabel(""), "");
});

test("renewal label uses the billing cycle end", () => {
  const end = Date.UTC(2026, 4, 12);
  assert.match(renewalLabel({ billingCycleEnd: end }, Date.UTC(2026, 4, 1)), /^Resets /);
  assert.equal(renewalLabel({ billingCycleEnd: 0 }), "");
});

test("formats on-demand disabled, fixed, and unlimited modes", () => {
  assert.equal(onDemandLimitMode({ onDemandMode: "disabled" }), "disabled");
  assert.equal(onDemandLimitMode({ onDemandMode: "unlimited" }), "unlimited");
  assert.equal(onDemandLimitMode({ onDemandHardLimitDollars: CURSOR_UNLIMITED_HARD_LIMIT_DOLLARS }), "unlimited");
  assert.equal(onDemandLimitMode({ onDemandLimitCents: 5000 }), "fixed");

  assert.equal(onDemandUsageCaption({ onDemandMode: "disabled", onDemandUsedCents: 0 }), "On-demand: Disabled");
  assert.equal(onDemandUsageCaption({ onDemandMode: "disabled", onDemandUsedCents: 350 }), "On-demand: Disabled ($3.50 used)");
  assert.equal(onDemandUsageCaption({ onDemandMode: "unlimited", onDemandUsedCents: 0 }), "On-demand: Unlimited");
  assert.equal(onDemandUsageCaption({ onDemandMode: "unlimited", onDemandUsedCents: 1234 }), "On-demand: $12.34 (Unlimited)");
  assert.equal(onDemandUsageCaption({ onDemandMode: "fixed", onDemandHardLimitDollars: 100, onDemandUsedCents: 250 }), "On-demand limit: $2.50 / $100.00");

  assert.equal(formatSpendLimitSummary("disabled", 0), "Disabled ($0)");
  assert.equal(formatSpendLimitSummary("fixed", 250), "$250");
  assert.equal(formatSpendLimitSummary("unlimited", CURSOR_UNLIMITED_HARD_LIMIT_DOLLARS), "Unlimited");
});

test("validates spend limit modal input for disabled, fixed, and unlimited", () => {
  assert.deepEqual(validateSpendLimitInput("disabled", ""), { ok: true, mode: "disabled", hardLimitDollars: 0 });
  assert.deepEqual(validateSpendLimitInput("unlimited", ""), {
    ok: true,
    mode: "unlimited",
    hardLimitDollars: CURSOR_UNLIMITED_HARD_LIMIT_DOLLARS,
  });
  assert.deepEqual(validateSpendLimitInput("fixed", "$150"), { ok: true, mode: "fixed", hardLimitDollars: 150 });
  assert.deepEqual(validateSpendLimitInput("fixed", "1,200"), { ok: true, mode: "fixed", hardLimitDollars: 1200 });
  assert.equal(validateSpendLimitInput("fixed", "0").ok, false);
  assert.equal(validateSpendLimitInput("fixed", "-5").ok, false);
  assert.equal(validateSpendLimitInput("fixed", "12.50").ok, false);
  assert.equal(validateSpendLimitInput("fixed", String(CURSOR_UNLIMITED_HARD_LIMIT_DOLLARS)).ok, false);
});

import assert from "node:assert/strict";
import test from "node:test";
import {
  autoBucketCaption,
  autoBucketRemainingPercent,
  bonusUsageCaption,
  credentialTypeLabel,
  formatPlanCents,
  namedBucketCaption,
  namedBucketRemainingPercent,
  remainingUsagePercent,
  renewalLabel,
  subscriptionStatusLabel,
  usageRemainingCaption,
} from "./cursorAccountPlan.js";

test("formats included cents as dollars", () => {
  assert.equal(formatPlanCents(2000), "$20.00");
  assert.equal(formatPlanCents(1540), "$15.40");
});

test("remaining percent is how much of the included amount is left", () => {
  assert.equal(remainingUsagePercent({ usageKnown: true, remainingCents: 1500, limitCents: 2000 }), 75);
  assert.equal(remainingUsagePercent({ usageKnown: true, remainingCents: -100, limitCents: 2000 }), 0);
  assert.equal(remainingUsagePercent({ usageKnown: false, remainingCents: 0, limitCents: 0 }), 0);
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

test("formats Named and Auto bucket breakdown and credential types", () => {
  const plan = {
    hasBucketBreakdown: true,
    namedUsedCents: 1500,
    namedLimitCents: 2000,
    autoUsedCents: 400,
    autoLimitCents: 2000,
    bonusCents: 500,
  };
  assert.equal(namedBucketRemainingPercent(plan), 25);
  assert.equal(autoBucketRemainingPercent(plan), 80);
  assert.equal(namedBucketCaption(plan), "Named models: $5.00 / $20.00 left (25%)");
  assert.equal(autoBucketCaption(plan), "Auto: $16.00 / $20.00 left (80%)");
  assert.equal(bonusUsageCaption(plan), "Bonus used: $5.00");
  assert.equal(credentialTypeLabel("api_key"), "API Key");
  assert.equal(credentialTypeLabel("oauth"), "OAuth");
});

test("subscription status stays readable", () => {
  assert.equal(subscriptionStatusLabel("past_due"), "Past due");
  assert.equal(subscriptionStatusLabel("active"), "Active");
  assert.equal(subscriptionStatusLabel(""), "");
});

test("renewal label uses the billing cycle end", () => {
  const end = Date.UTC(2026, 4, 12);
  assert.match(renewalLabel({ billingCycleEnd: end }, Date.UTC(2026, 4, 1)), /^Renews /);
  assert.equal(renewalLabel({ billingCycleEnd: 0 }), "");
});

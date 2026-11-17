import assert from "node:assert/strict";
import test from "node:test";
import {
  formatPlanCents,
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

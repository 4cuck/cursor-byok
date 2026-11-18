/** Display helpers for a Cursor account plan snapshot. Amounts are cents. */

export function formatPlanCents(cents) {
  const value = Number(cents);
  const amount = Number.isFinite(value) ? value / 100 : 0;
  return amount.toLocaleString("en-US", { style: "currency", currency: "USD" });
}

// roundCursorUsedPercent mirrors Cursor IDE 3.30's BQm() helper: any non-zero
// usage under 1% rounds up to 1% so active accounts never show "0% used".
export function roundCursorUsedPercent(rawPercent) {
  const pct = Number(rawPercent);
  if (!Number.isFinite(pct) || pct <= 0) return 0;
  if (pct > 0 && pct < 1) return 1;
  return Math.round(Math.min(Math.max(pct, 0), 100));
}

export function usedUsagePercent(plan) {
  if (!plan?.usageKnown) return 0;
  if (plan.totalPercentKnown || Number.isFinite(Number(plan.percentUsed))) {
    const pct = Number(plan.percentUsed);
    if (Number.isFinite(pct) && (plan.totalPercentKnown || pct > 0 || !plan.limitCents)) {
      return roundCursorUsedPercent(pct);
    }
  }
  const limit = Number(plan.limitCents) || 0;
  if (limit > 0) {
    const remaining = Number(plan.remainingCents) || 0;
    const used = Math.max(0, limit - remaining);
    return roundCursorUsedPercent((used / limit) * 100);
  }
  return 0;
}

export function remainingUsagePercent(plan) {
  if (!plan?.usageKnown) return 0;
  if (plan.totalPercentKnown && !plan.limitCents) {
    return Math.max(0, Math.min(100, 100 - roundCursorUsedPercent(plan.percentUsed)));
  }
  const limit = Number(plan.limitCents);
  if (limit > 0) {
    if (plan.totalPercentKnown && Number(plan.bonusCents) > 0 && Number(plan.remainingCents) === 0) {
      return Math.max(0, Math.min(100, 100 - roundCursorUsedPercent(plan.percentUsed)));
    }
    const remaining = Math.max(0, Number(plan.remainingCents) || 0);
    return Math.max(0, Math.min(100, Math.round((remaining / limit) * 100)));
  }
  return 0;
}

export function usageRemainingCaption(plan) {
  if (!plan?.usageKnown) return "";
  if (plan.totalPercentKnown && Number(plan.bonusCents) > 0 && Number(plan.totalSpendCents) > 0) {
    return `${formatPlanCents(plan.totalSpendCents)} used (${formatPlanCents(plan.limitCents || plan.includedCents)} plan + ${formatPlanCents(plan.bonusCents)} bonus)`;
  }
  if (plan.totalPercentKnown && !plan.limitCents) {
    return "Included total usage";
  }
  if ((Number(plan.remainingCents) || 0) < 0) {
    return `${formatPlanCents(Math.abs(plan.remainingCents))} over the included amount`;
  }
  return `${formatPlanCents(plan.remainingCents)} left of ${formatPlanCents(plan.limitCents)}`;
}

export function subscriptionStatusLabel(status) {
  switch (String(status || "").trim().toLowerCase()) {
    case "active":
      return "Active";
    case "trialing":
      return "Trial";
    case "past_due":
      return "Past due";
    case "canceled":
    case "cancelled":
      return "Canceled";
    case "unpaid":
      return "Unpaid";
    case "incomplete":
      return "Incomplete";
    case "":
      return "";
    default:
      return status;
  }
}

export function credentialTypeLabel(credentialType) {
  switch (String(credentialType || "").trim().toLowerCase()) {
    case "api_key":
      return "API Key";
    case "oauth":
      return "OAuth";
    default:
      return "";
  }
}

export function usedBarClass(usedPct) {
  const used = Number(usedPct) || 0;
  if (used >= 95) return "bg-[#e06a6a]";
  if (used >= 75) return "bg-[#e2b15a]";
  return "bg-[#3dbe7a]";
}

export function bucketBarClass(remainingPct) {
  const remaining = Number(remainingPct) || 0;
  if (remaining >= 50) return "bg-[#3dbe7a]";
  if (remaining >= 20) return "bg-[#e2b15a]";
  return "bg-[#e06a6a]";
}

export function usageBarClass(plan) {
  return usedBarClass(usedUsagePercent(plan));
}

export function namedBucketUsedPercent(plan) {
  if (!plan?.hasBucketBreakdown) return null;
  const known = Boolean(plan.namedBucketKnown)
    || (Number(plan.namedLimitCents) || 0) > 0
    || (Number.isFinite(Number(plan.namedPercentUsed)) && Number(plan.namedPercentUsed) > 0);
  if (!known) return null;
  if (Number.isFinite(Number(plan.namedPercentUsed)) && (Boolean(plan.namedBucketKnown) || Number(plan.namedPercentUsed) > 0)) {
    return roundCursorUsedPercent(plan.namedPercentUsed);
  }
  const limit = Number(plan.namedLimitCents) || 0;
  if (limit > 0) {
    const used = Math.max(0, Number(plan.namedUsedCents) || 0);
    return roundCursorUsedPercent((used / limit) * 100);
  }
  return 0;
}

export function autoBucketUsedPercent(plan) {
  if (!plan?.hasBucketBreakdown) return null;
  const known = Boolean(plan.autoBucketKnown)
    || (Number(plan.autoLimitCents) || 0) > 0
    || (Number.isFinite(Number(plan.autoPercentUsed)) && Number(plan.autoPercentUsed) > 0);
  if (!known) return null;
  if (Number.isFinite(Number(plan.autoPercentUsed)) && (Boolean(plan.autoBucketKnown) || Number(plan.autoPercentUsed) > 0)) {
    return roundCursorUsedPercent(plan.autoPercentUsed);
  }
  const limit = Number(plan.autoLimitCents) || 0;
  if (limit > 0) {
    const used = Math.max(0, Number(plan.autoUsedCents) || 0);
    return roundCursorUsedPercent((used / limit) * 100);
  }
  return 0;
}

export function namedBucketRemainingPercent(plan) {
  const used = namedBucketUsedPercent(plan);
  return used == null ? null : Math.max(0, 100 - used);
}

export function autoBucketRemainingPercent(plan) {
  const used = autoBucketUsedPercent(plan);
  return used == null ? null : Math.max(0, 100 - used);
}

export function namedBucketCaption(plan) {
  const usedPct = namedBucketUsedPercent(plan);
  if (usedPct == null) return "";
  const limit = Number(plan?.namedLimitCents) || 0;
  if (limit > 0) {
    const used = Math.max(0, Number(plan?.namedUsedCents) || 0);
    return `Named models: ${formatPlanCents(used)} / ${formatPlanCents(limit)} (${usedPct}% used)`;
  }
  return `Named models: ${usedPct}% used`;
}

export function autoBucketCaption(plan) {
  const usedPct = autoBucketUsedPercent(plan);
  if (usedPct == null) return "";
  const limit = Number(plan?.autoLimitCents) || 0;
  if (limit > 0) {
    const used = Math.max(0, Number(plan?.autoUsedCents) || 0);
    return `Auto / Grok: ${formatPlanCents(used)} / ${formatPlanCents(limit)} (${usedPct}% used)`;
  }
  return `Auto / Grok: ${usedPct}% used`;
}

export function bonusUsageCaption(plan) {
  const bonus = Number(plan?.bonusCents) || 0;
  if (bonus <= 0) return "";
  return `+${formatPlanCents(bonus)} free bonus`;
}

export function onDemandUsageCaption(plan) {
  const limit = Number(plan?.onDemandLimitCents) || 0;
  if (limit <= 0) return "";
  const used = Math.max(0, Number(plan?.onDemandUsedCents) || 0);
  return `On-demand limit: ${formatPlanCents(used)} / ${formatPlanCents(limit)}`;
}

export function renewalLabel(plan, now = Date.now()) {
  const end = Number(plan?.billingCycleEnd);
  if (!end) return "";
  const date = new Date(end);
  if (Number.isNaN(date.getTime()) || date.getTime() < now - 24 * 60 * 60 * 1000) return "";
  return `Resets ${date.toLocaleDateString(undefined, { month: "short", day: "numeric" })}`;
}

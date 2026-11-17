/** Display helpers for a Cursor account plan snapshot. Amounts are cents. */

export function formatPlanCents(cents) {
  const value = Number(cents);
  const amount = Number.isFinite(value) ? value / 100 : 0;
  return amount.toLocaleString("en-US", { style: "currency", currency: "USD" });
}

export function remainingUsagePercent(plan) {
  if (!plan?.usageKnown || !plan.limitCents) return 0;
  const limit = Number(plan.limitCents);
  const remaining = Math.max(0, Number(plan.remainingCents) || 0);
  if (limit <= 0) return 0;
  return Math.max(0, Math.min(100, Math.round((remaining / limit) * 100)));
}

export function usageRemainingCaption(plan) {
  if (!plan?.usageKnown) return "";
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

export function usageBarClass(plan) {
  const remaining = remainingUsagePercent(plan);
  if (remaining >= 50) return "bg-[#3dbe7a]";
  if (remaining >= 20) return "bg-[#e2b15a]";
  return "bg-[#e06a6a]";
}

export function renewalLabel(plan, now = Date.now()) {
  const end = Number(plan?.billingCycleEnd);
  if (!end) return "";
  const date = new Date(end);
  if (Number.isNaN(date.getTime()) || date.getTime() < now - 24 * 60 * 60 * 1000) return "";
  return `Renews ${date.toLocaleDateString(undefined, { month: "short", day: "numeric" })}`;
}

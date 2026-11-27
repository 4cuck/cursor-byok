// Display helpers for the Control Center's Claude subscription section.
//
// The texts below are Chinese source literals, which the build replaces with
// localized text objects. Such an object is not a primitive string: do not
// compare one with === or test it with typeof. Turning it into text (in a
// template, with String(), or by joining) gives the current language.

const MINUTE_MS = 60_000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

// A token that expires within this time is flagged as expiring soon.
const EXPIRY_SOON_MS = 3 * DAY_MS;

// Usage older than this is read again when the section opens.
export const CLAUDE_USAGE_STALE_MS = 5 * MINUTE_MS;

// The largest credential the backend accepts, in characters.
export const CLAUDE_CREDENTIALS_MAX_LENGTH = 64 * 1024;

const PLAN_LABELS = { pro: "Pro", max: "Max", team: "Team", enterprise: "Enterprise" };

// The plan's product name, or "" when the plan is not known.
export function claudePlanLabel(plan) {
  return PLAN_LABELS[String(plan || "").trim().toLowerCase()] || "";
}

// What an account is called in lists and messages: its e-mail address, or the
// name on it when Anthropic gave no address. "" when neither is known.
export function claudeAccountName(account) {
  return String(account?.email || account?.displayName || "").trim();
}

export function claudeAccountInitial(account) {
  const first = Array.from(claudeAccountName(account))[0];
  return first ? first.toUpperCase() : "C";
}

function clampedPercent(value) {
  if (value === null || value === undefined || value === "") return null;
  const number = Number(value);
  if (!Number.isFinite(number)) return null;
  return Math.min(100, Math.max(0, number));
}

// "exhausted" at the limit, "warning" from 80% of it, otherwise "ok".
export function claudeUsageLevel(usedPercent) {
  if (usedPercent >= 100) return "exhausted";
  if (usedPercent >= 80) return "warning";
  return "ok";
}

// The windows are named as the Claude CLI names them on its usage screen.
export function claudeUsageWindowLabel(window) {
  const id = String(window?.id || "").trim();
  switch (id) {
    case "5h":
      return "当前会话（5 小时）";
    case "7d":
      return "本周（全部模型）";
    case "7d-opus":
      return "本周（仅 Opus）";
    case "7d-sonnet":
      return "本周（仅 Sonnet）";
    default: {
      // A weekly limit for one model family, which Anthropic names itself.
      const family = String(window?.label || "").trim();
      return family ? `本周（${family}）` : id;
    }
  }
}

// The account's usage windows, ready to draw: [{ id, label, usedPercent,
// level, resetsAt }]. Windows without an id or a percentage are left out.
export function claudeUsageWindows(account) {
  const windows = Array.isArray(account?.usageWindows) ? account.usageWindows : [];
  const result = [];
  for (const window of windows) {
    const id = String(window?.id || "").trim();
    const usedPercent = clampedPercent(window?.usedPercent);
    if (!id || usedPercent === null) continue;
    result.push({
      id,
      label: claudeUsageWindowLabel(window),
      usedPercent,
      level: claudeUsageLevel(usedPercent),
      resetsAt: String(window.resetsAt || "").trim(),
    });
  }
  return result;
}

// The first window Anthropic reports as used to 100%, or null. That figure
// alone does not tell whether requests are refused: requests have been seen
// to go through while a window stood at 100%.
export function claudeExhaustedWindow(account) {
  return claudeUsageWindows(account).find((window) => window.level === "exhausted") || null;
}

// What is known about when a token expires: { known, expired, soon,
// remainingMs }. A token without an expiry time is not known to expire.
export function claudeTokenExpiry(expiresAtUnixMs, now = Date.now()) {
  const expiresAt = Number(expiresAtUnixMs || 0);
  if (!Number.isFinite(expiresAt) || expiresAt <= 0) {
    return { known: false, expired: false, soon: false, remainingMs: 0 };
  }
  const remainingMs = expiresAt - now;
  return {
    known: true,
    expired: remainingMs <= 0,
    soon: remainingMs > 0 && remainingMs < EXPIRY_SOON_MS,
    remainingMs: Math.max(0, remainingMs),
  };
}

// "剩余 N 天", in the largest unit that fits, or "" when no time is left.
export function claudeRemainingLabel(remainingMs) {
  const remaining = Number(remainingMs);
  if (!Number.isFinite(remaining) || remaining <= 0) return "";
  if (remaining >= DAY_MS) return `剩余 ${Math.floor(remaining / DAY_MS)} 天`;
  if (remaining >= HOUR_MS) return `剩余 ${Math.floor(remaining / HOUR_MS)} 小时`;
  return `剩余 ${Math.max(1, Math.floor(remaining / MINUTE_MS))} 分钟`;
}

function dayKey(date, timeZone) {
  return new Intl.DateTimeFormat("en-CA", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    ...(timeZone ? { timeZone } : {}),
  }).format(date);
}

// A time of day when value is today, and a date with the time otherwise.
// value is anything Date accepts; "" when it is not a time.
export function formatClaudeDateTime(value, locale, now = Date.now(), timeZone) {
  if (value === null || value === undefined || value === "" || value === 0) return "";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "";
  const today = dayKey(date, timeZone) === dayKey(new Date(now), timeZone);
  return new Intl.DateTimeFormat(locale || undefined, {
    ...(today ? {} : { month: "2-digit", day: "2-digit" }),
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    ...(timeZone ? { timeZone } : {}),
  }).format(date);
}

// A date with its year; "" when value is not a time.
export function formatClaudeDate(value, locale, timeZone) {
  if (value === null || value === undefined || value === "" || value === 0) return "";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "";
  return new Intl.DateTimeFormat(locale || undefined, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    ...(timeZone ? { timeZone } : {}),
  }).format(date);
}

// What removing an account does to the model rows: { kept, removed }. Rows
// other accounts serve too are kept; rows only this account serves go with it.
export function claudeRemovalImpact(models, accountId) {
  let kept = 0;
  let removed = 0;
  for (const model of Array.isArray(models) ? models : []) {
    const accountIds = Array.isArray(model?.accountIds) ? model.accountIds : [];
    if (!accountIds.includes(accountId)) continue;
    if (accountIds.length > 1) kept += 1;
    else removed += 1;
  }
  return { kept, removed };
}

// Whether the account's usage was read too long ago to show without asking
// Anthropic again.
export function claudeUsageIsStale(account, now = Date.now(), maxAgeMs = CLAUDE_USAGE_STALE_MS) {
  const readAt = Number(account?.usageAtUnixMs || 0);
  return !(readAt > 0) || now - readAt > maxAgeMs;
}

// Whether Fast mode cannot work with the saved accounts. Anthropic serves Fast
// mode only to accounts that pay for use beyond the plan ("usage credits"), so
// this is true when every account with a working token is known to have that
// turned off. It is false while that is unknown for any of them.
export function claudeFastModeUnavailable(accounts) {
  const usable = (Array.isArray(accounts) ? accounts : []).filter((account) => account?.status === "ok");
  if (usable.length === 0) return false;
  return usable.every((account) => account.usageCredits && account.usageCredits.enabled === false);
}

import test from "node:test";
import assert from "node:assert/strict";

import {
  claudeAccountInitial,
  claudeAccountName,
  claudeExhaustedWindow,
  claudeFastModeUnavailable,
  claudePlanLabel,
  claudeRemainingLabel,
  claudeRemovalImpact,
  claudeTokenExpiry,
  claudeUsageIsStale,
  claudeUsageLevel,
  claudeUsageWindowLabel,
  claudeUsageWindows,
  formatClaudeDate,
  formatClaudeDateTime,
} from "./claudeAccounts.js";

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const NOW = Date.UTC(2026, 10, 27, 15, 0, 0);

test("names the plans it knows and no other", () => {
  assert.equal(claudePlanLabel("pro"), "Pro");
  assert.equal(claudePlanLabel(" MAX "), "Max");
  assert.equal(claudePlanLabel("team"), "Team");
  assert.equal(claudePlanLabel("enterprise"), "Enterprise");
  assert.equal(claudePlanLabel("free"), "");
  assert.equal(claudePlanLabel(undefined), "");
});

test("calls an account by its e-mail address, then by its name", () => {
  assert.equal(claudeAccountName({ email: " alice@example.test ", displayName: "Alice" }), "alice@example.test");
  assert.equal(claudeAccountName({ displayName: "Alice" }), "Alice");
  assert.equal(claudeAccountName({}), "");
  assert.equal(claudeAccountName(null), "");

  assert.equal(claudeAccountInitial({ email: "alice@example.test" }), "A");
  assert.equal(claudeAccountInitial({ displayName: "张三" }), "张");
  assert.equal(claudeAccountInitial({}), "C");
});

test("grades usage by how close it is to the limit", () => {
  assert.equal(claudeUsageLevel(0), "ok");
  assert.equal(claudeUsageLevel(79.9), "ok");
  assert.equal(claudeUsageLevel(80), "warning");
  assert.equal(claudeUsageLevel(99.9), "warning");
  assert.equal(claudeUsageLevel(100), "exhausted");
});

test("names the usage windows as the Claude CLI does", () => {
  assert.equal(claudeUsageWindowLabel({ id: "5h" }), "当前会话（5 小时）");
  assert.equal(claudeUsageWindowLabel({ id: "7d" }), "本周（全部模型）");
  assert.equal(claudeUsageWindowLabel({ id: "7d-opus" }), "本周（仅 Opus）");
  assert.equal(claudeUsageWindowLabel({ id: "7d-sonnet" }), "本周（仅 Sonnet）");
  assert.equal(claudeUsageWindowLabel({ id: "7d:fable", label: "Fable" }), "本周（Fable）");
  assert.equal(claudeUsageWindowLabel({ id: "something-new" }), "something-new");
});

test("prepares usage windows for drawing and drops the ones it cannot draw", () => {
  const windows = claudeUsageWindows({
    usageWindows: [
      { id: "5h", usedPercent: 100, resetsAt: "2026-11-27T17:50:00+00:00" },
      { id: "7d", usedPercent: 74.4 },
      { id: "7d:fable", label: "Fable", usedPercent: 140 },
      { id: "7d-sonnet", usedPercent: -3 },
      { id: "", usedPercent: 10 },
      { id: "7d-opus", usedPercent: null },
      { id: "broken", usedPercent: "many" },
      null,
    ],
  });
  assert.deepEqual(windows, [
    { id: "5h", label: "当前会话（5 小时）", usedPercent: 100, level: "exhausted", resetsAt: "2026-11-27T17:50:00+00:00" },
    { id: "7d", label: "本周（全部模型）", usedPercent: 74.4, level: "ok", resetsAt: "" },
    { id: "7d:fable", label: "本周（Fable）", usedPercent: 100, level: "exhausted", resetsAt: "" },
    { id: "7d-sonnet", label: "本周（仅 Sonnet）", usedPercent: 0, level: "ok", resetsAt: "" },
  ]);
  assert.deepEqual(claudeUsageWindows({}), []);
  assert.deepEqual(claudeUsageWindows(null), []);
});

test("finds the window that is at its limit", () => {
  assert.equal(claudeExhaustedWindow({ usageWindows: [{ id: "5h", usedPercent: 99 }, { id: "7d", usedPercent: 100 }] }).id, "7d");
  assert.equal(claudeExhaustedWindow({ usageWindows: [{ id: "5h", usedPercent: 99 }] }), null);
  assert.equal(claudeExhaustedWindow({}), null);
});

test("tells an expired token from one that expires soon and one that does not", () => {
  assert.deepEqual(claudeTokenExpiry(0, NOW), { known: false, expired: false, soon: false, remainingMs: 0 });
  assert.deepEqual(claudeTokenExpiry(undefined, NOW), { known: false, expired: false, soon: false, remainingMs: 0 });
  assert.deepEqual(claudeTokenExpiry(NOW - 1, NOW), { known: true, expired: true, soon: false, remainingMs: 0 });
  assert.deepEqual(claudeTokenExpiry(NOW, NOW), { known: true, expired: true, soon: false, remainingMs: 0 });
  assert.deepEqual(claudeTokenExpiry(NOW + 5 * HOUR, NOW), { known: true, expired: false, soon: true, remainingMs: 5 * HOUR });
  assert.deepEqual(claudeTokenExpiry(NOW + 3 * DAY, NOW), { known: true, expired: false, soon: false, remainingMs: 3 * DAY });
  assert.deepEqual(claudeTokenExpiry(NOW + 357 * DAY, NOW), { known: true, expired: false, soon: false, remainingMs: 357 * DAY });
});

test("states the time left in the largest unit that fits", () => {
  assert.equal(claudeRemainingLabel(357 * DAY + 5 * HOUR), "剩余 357 天");
  assert.equal(claudeRemainingLabel(DAY), "剩余 1 天");
  assert.equal(claudeRemainingLabel(DAY - 1), "剩余 23 小时");
  assert.equal(claudeRemainingLabel(HOUR), "剩余 1 小时");
  assert.equal(claudeRemainingLabel(59 * MINUTE + 59_000), "剩余 59 分钟");
  assert.equal(claudeRemainingLabel(20_000), "剩余 1 分钟");
  assert.equal(claudeRemainingLabel(0), "");
  assert.equal(claudeRemainingLabel(-5), "");
  assert.equal(claudeRemainingLabel(undefined), "");
});

test("writes a time of day for today and a date with the time otherwise", () => {
  // NOW is 2026-11-27 15:00 UTC, which is 23:00 on the 27th in Shanghai.
  const zone = "Asia/Shanghai";
  assert.equal(formatClaudeDateTime("2026-11-27T15:50:00+00:00", "en-US", NOW, zone), "23:50");
  // 17:50 UTC is 01:50 on the 28th there, which is no longer today.
  assert.equal(formatClaudeDateTime("2026-11-27T17:50:00+00:00", "en-US", NOW, zone), "11/28, 01:50");
  assert.equal(formatClaudeDateTime("2026-11-27T17:50:00+00:00", "en-US", NOW, "UTC"), "17:50");
  assert.equal(formatClaudeDateTime(NOW - 2 * DAY, "en-US", NOW, "UTC"), "11/25, 15:00");
  assert.equal(formatClaudeDateTime("not a time", "en-US", NOW, "UTC"), "");
  assert.equal(formatClaudeDateTime("", "en-US", NOW, "UTC"), "");
  assert.equal(formatClaudeDateTime(0, "en-US", NOW, "UTC"), "");

  assert.equal(formatClaudeDate(Date.UTC(2027, 10, 19, 12), "en-US", "UTC"), "11/19/2027");
  assert.equal(formatClaudeDate(0, "en-US", "UTC"), "");
  assert.equal(formatClaudeDate("not a time", "en-US", "UTC"), "");
});

test("counts what removing an account does to the model rows", () => {
  const models = [
    { id: "row-1", accountIds: ["alice", "bob"] },
    { id: "row-2", accountIds: ["alice"] },
    { id: "row-3", accountIds: ["bob"] },
    { id: "row-4", accountIds: ["bob", "alice"] },
    { id: "row-5" },
  ];
  assert.deepEqual(claudeRemovalImpact(models, "alice"), { kept: 2, removed: 1 });
  assert.deepEqual(claudeRemovalImpact(models, "bob"), { kept: 2, removed: 1 });
  assert.deepEqual(claudeRemovalImpact(models, "carol"), { kept: 0, removed: 0 });
  assert.deepEqual(claudeRemovalImpact(undefined, "alice"), { kept: 0, removed: 0 });
});

test("treats usage that was never read, or read long ago, as stale", () => {
  assert.equal(claudeUsageIsStale({ usageAtUnixMs: NOW - MINUTE }, NOW), false);
  assert.equal(claudeUsageIsStale({ usageAtUnixMs: NOW - 5 * MINUTE }, NOW), false);
  assert.equal(claudeUsageIsStale({ usageAtUnixMs: NOW - 5 * MINUTE - 1 }, NOW), true);
  assert.equal(claudeUsageIsStale({ usageAtUnixMs: 0 }, NOW), true);
  assert.equal(claudeUsageIsStale({}, NOW), true);
});

test("reports Fast mode as unavailable only when every usable account is known to lack usage credits", () => {
  const off = { status: "ok", usageCredits: { enabled: false } };
  const on = { status: "ok", usageCredits: { enabled: true } };
  const unknown = { status: "ok" };
  const expiredOn = { status: "expired", usageCredits: { enabled: true } };

  assert.equal(claudeFastModeUnavailable([off]), true);
  assert.equal(claudeFastModeUnavailable([off, off]), true);
  assert.equal(claudeFastModeUnavailable([off, on]), false);
  assert.equal(claudeFastModeUnavailable([off, unknown]), false);
  // An account whose token does not work serves no request, so its credits do not count.
  assert.equal(claudeFastModeUnavailable([off, expiredOn]), true);
  assert.equal(claudeFastModeUnavailable([expiredOn]), false);
  assert.equal(claudeFastModeUnavailable([]), false);
  assert.equal(claudeFastModeUnavailable(undefined), false);
});

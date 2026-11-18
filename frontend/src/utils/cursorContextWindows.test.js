import assert from "node:assert/strict";
import test from "node:test";

import {
  cursorContextWindows,
  formatContextWindowTokens,
  resolveCursorContextWindowTokens,
} from "./cursorContextWindows.js";
import { mergeDuplicateModelAdapter, normalizeModelAdapter } from "./modelAdapter.js";

const syncedFableSpec = JSON.stringify({
  contextTokenLimit: 300_000,
  contextTokenLimitForMaxMode: 1_000_000,
  contextWindows: [
    { tokens: 1_000_000, value: "1m", maxMode: true },
    { tokens: 300_000, value: "300k", default: true },
  ],
});
const olderFableSpec = JSON.stringify({ contextTokenLimit: 300_000, contextTokenLimitForMaxMode: 1_000_000 });

function cursorAdapter(spec, contextWindowTokens) {
  return normalizeModelAdapter({
    id: "cursor_account1:claude-fable-5-2",
    source: "cursor_account",
    displayName: "Claude Fable 5.2",
    groupName: "cursor_account1",
    supplierID: "cursor_account1",
    modelID: "claude-fable-5-2",
    cursorModelSpec: spec,
    contextWindowTokens,
  });
}

test("lists Cursor windows smallest first with Max and default flags", () => {
  assert.deepEqual(cursorContextWindows(syncedFableSpec), [
    { tokens: 300_000, value: "300k", maxMode: false, isDefault: true },
    { tokens: 1_000_000, value: "1m", maxMode: true, isDefault: false },
  ]);
  assert.deepEqual(cursorContextWindows(olderFableSpec), []);
  assert.deepEqual(cursorContextWindows("{"), []);
  assert.deepEqual(cursorContextWindows(""), []);
});

test("keeps an offered window and falls back to Cursor's default", () => {
  assert.equal(resolveCursorContextWindowTokens(syncedFableSpec, 1_000_000), 1_000_000);
  assert.equal(resolveCursorContextWindowTokens(syncedFableSpec, 128_000), 300_000);
  assert.equal(resolveCursorContextWindowTokens(syncedFableSpec, 0), 300_000);
  assert.equal(resolveCursorContextWindowTokens(olderFableSpec, 1_000_000), 0);
});

test("formats window sizes like Cursor's picker", () => {
  assert.equal(formatContextWindowTokens(272_000), "272K");
  assert.equal(formatContextWindowTokens(262_000), "262K");
  assert.equal(formatContextWindowTokens(1_000_000), "1M");
  assert.equal(formatContextWindowTokens(1_048_576), "1.05M");
  assert.equal(formatContextWindowTokens(0), "");
});

test("normalizes a stored Cursor window against the synced list", () => {
  assert.equal(cursorAdapter(syncedFableSpec, 1_000_000).contextWindowTokens, 1_000_000);
  assert.equal(cursorAdapter(syncedFableSpec, 128_000).contextWindowTokens, 300_000);
});

test("re-sync keeps a chosen window and replaces an older guess", () => {
  const incoming = cursorAdapter(syncedFableSpec, 300_000);
  assert.equal(mergeDuplicateModelAdapter(cursorAdapter(syncedFableSpec, 1_000_000), incoming).contextWindowTokens, 1_000_000);
  assert.equal(mergeDuplicateModelAdapter(cursorAdapter(olderFableSpec, 1_000_000), incoming).contextWindowTokens, 300_000);
});

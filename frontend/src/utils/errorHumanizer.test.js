import test from "node:test";
import assert from "node:assert/strict";

import { toUserError, toUserErrorWithCause } from "./errorHumanizer.js";

test("toUserErrorWithCause appends sanitized technical cause", () => {
  const text = toUserErrorWithCause(new Error(
    "all model catalog candidates failed: model catalog network request failed: dial tcp 127.0.0.1:7890: connect: connection refused",
  ));

  assert.match(text, /Cannot reach the service right now/);
  assert.match(text, /dial tcp 127\.0\.0\.1:7890: connect: connection refused/);
});

test("toUserErrorWithCause keeps credentials out of the banner", () => {
  const text = toUserErrorWithCause(new Error(
    "all model catalog candidates failed: model catalog network request failed: Authorization: Bearer sk-secret",
  ));

  assert.doesNotMatch(text, /sk-secret/);
});

test("toUserErrorWithCause does not duplicate identical text", () => {
  const text = toUserErrorWithCause(new Error("操作已取消"));

  // The user-facing sentence is English; the raw cause is appended because it
  // differs, which keeps the original wording available for diagnostics.
  assert.match(text, /^The operation was cancelled\./);
});

test("toUserError stays generic for unclassified errors", () => {
  const text = toUserError(new Error("Authorization: Bearer sk-secret"));

  assert.equal(text, "The service encountered an error. Retry or export diagnostic information.");
});

test("toUserError maps account switch codes to readable English", () => {
  const text = toUserError(new Error("cursor_state_db_missing: cursor state db is missing"));

  assert.equal(text, "Cursor's state database was not found. Launch Cursor once first.");
});

test("toUserError maps rejected API keys to readable English", () => {
  const text = toUserError(new Error("account_import_api_key_rejected: cursor api key was rejected"));

  assert.equal(text, "This API key is invalid or has been revoked.");
});

test("toUserError maps invalid Cursor API key and xAI Console key errors", () => {
  assert.equal(
    toUserError(new Error("account_import_invalid_api_key: cursor api key must start with crsr_ or grok_")),
    "API key format is invalid. It must start with crsr_ or grok_.",
  );
  assert.match(
    toUserError(new Error("account_import_xai_api_key: xai- keys are xAI Console API keys, not Cursor account keys")),
    /xAI Console API key.*Third-party suppliers.*crsr_ or grok_/,
  );
});
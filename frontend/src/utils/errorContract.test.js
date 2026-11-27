import test from "node:test";
import assert from "node:assert/strict";

import {
  normalizeClientError,
  safeErrorLogAttributes,
  summarizePayload,
} from "./errorContract.js";

test("classifies timeout as retryable", () => {
  const error = normalizeClientError(new Error("request timeout"), { operation: "LoadState" });

  assert.equal(error.code, "timeout");
  assert.equal(error.kind, "timeout");
  assert.equal(error.disposition, "retryable");
  assert.equal(error.operation, "LoadState");
});

test("classifies authentication failure as blocked", () => {
  const error = normalizeClientError({ statusCode: 401, message: "invalid api key" });

  assert.equal(error.code, "authentication_required");
  assert.equal(error.disposition, "blocked");
  assert.equal(error.statusCode, 401);
});

test("recognizes workspace MCP trust as a stable user action requirement", () => {
  const error = normalizeClientError(new Error(
    "mcp_workspace_trust_required: workspace MCP server requires explicit trust",
  ));

  assert.equal(error.code, "mcp_workspace_trust_required");
  assert.equal(error.kind, "authorization");
  assert.equal(error.disposition, "user_action_required");
});

test("preserves structured backend fields", () => {
  const error = normalizeClientError({
    code: "provider_rate_limited",
    kind: "rate_limit",
    disposition: "retryable",
    userMessage: "稍后重试",
    technicalMessage: "status=429",
    traceId: "req-123",
    retryAfterMs: 1200,
  });

  assert.equal(error.traceId, "req-123");
  assert.equal(error.retryAfterMs, 1200);
  assert.equal(error.technicalMessage, "status=429");
});

test("summarizes payload without leaking secrets or full prompts", () => {
  const summary = summarizePayload({
    apiKey: "sk-secret",
    nested: { authorization: "Bearer secret", model: "gpt-test" },
    prompt: "private prompt",
    items: [1, 2, 3, 4, 5, 6],
  });

  assert.equal(summary.apiKey, "[redacted]");
  assert.equal(summary.nested.authorization, "[redacted]");
  assert.equal(summary.nested.model, "gpt-test");
  assert.equal(summary.prompt, "[redacted]");
  assert.deepEqual(summary.items, { type: "array", length: 6 });
});

test("does not expose sensitive technical message to the user", () => {
  const error = normalizeClientError(new Error("Authorization: Bearer sk-secret"));

  assert.equal(error.userMessage, "The service encountered an error. Retry or export diagnostic information.");
  assert.doesNotMatch(error.technicalMessage, /sk-secret/);
});

test("redacts secret values from URL query parameters", () => {
  const error = normalizeClientError(new Error("request failed https://api.example.test/v1?token=secret-token&model=test"));

  assert.doesNotMatch(error.technicalMessage, /secret-token/);
  assert.match(error.technicalMessage, /token=\[redacted\]/);
  assert.match(error.technicalMessage, /model=test/);
});

test("safe log attributes exclude raw messages and causes", () => {
  const attributes = safeErrorLogAttributes(new Error("prompt=private apiKey=sk-secret"), {
    operation: "ui.render",
    traceId: "trace-safe",
  });

  assert.deepEqual(attributes, {
    operation: "ui.render",
    code: "internal_error",
    kind: "internal",
    disposition: "fatal",
    statusCode: 0,
    traceId: "trace-safe",
  });
  assert.doesNotMatch(JSON.stringify(attributes), /private|sk-secret/);
});

test("maps backend model catalog network failure to retryable network error", () => {
  const error = normalizeClientError(new Error(
    "所有模型目录候选地址均失败：模型目录网络请求失败: dial tcp 127.0.0.1:7890: connect: connection refused",
  ));

  assert.equal(error.code, "network_error");
  assert.equal(error.kind, "network");
  assert.equal(error.disposition, "retryable");
});

test("maps backend network wording in Chinese to network error", () => {
  const error = normalizeClientError(new Error("模型目录网络请求失败"));

  assert.equal(error.code, "network_error");
  assert.equal(error.disposition, "retryable");
});

test("turns Claude subscription codes into their own sentences", () => {
  const cases = [
    ["claude_subscription_import_invalid: the pasted text is neither a credentials file nor an access token", "This is neither a Claude credentials file nor an access token."],
    ["claude_subscription_not_oauth_token: this is not a Claude subscription token (sk-ant-oat...)", "This is not a Claude subscription token. Subscription tokens start with sk-ant-oat; an API key goes into Model Settings instead."],
    ["claude_subscription_token_rejected: Anthropic refused this token", "Anthropic refused this token. Sign in again with the Claude CLI and submit the new credentials."],
    ["claude_subscription_not_found: this Claude account is no longer saved", "This Claude account is no longer saved. Refresh the list."],
    ["claude_subscription_model_not_found: no saved Claude account serves this model row", "This model is no longer in the list. Refresh and try again."],
    ["claude_subscription_no_usable_account: no saved Claude account has a usable token", "No saved Claude account has a working token. Add an account, or replace the credentials of a saved one."],
    ["claude_subscription_details_unavailable: this token may not read the account's profile", "This token works, but it is not allowed to read the account's profile or usage."],
  ];
  for (const [text, message] of cases) {
    const error = normalizeClientError(new Error(text), { operation: "ImportClaudeAccount" });
    assert.equal(error.code, text.slice(0, text.indexOf(":")));
    assert.equal(error.kind, "account");
    assert.equal(error.disposition, "blocked");
    assert.equal(error.userMessage, message);
  }
});

test("a Claude subscription code wins over an HTTP status in its detail, and the detail is kept", () => {
  const unreachable = normalizeClientError(new Error(
    "claude_subscription_unreachable: the token could not be checked with Anthropic: HTTP 503: Service unavailable",
  ));
  assert.equal(unreachable.code, "claude_subscription_unreachable");
  assert.equal(
    unreachable.userMessage,
    "Could not reach Anthropic. Check your network or proxy and try again. (the token could not be checked with Anthropic: HTTP 503: Service unavailable)",
  );

  const expired = normalizeClientError("claude_subscription_token_expired: this token expired on 2026-11-19");
  assert.equal(
    expired.userMessage,
    "This token has expired. Sign in again with the Claude CLI and submit the new credentials. (this token expired on 2026-11-19)",
  );

  // A failed save while a Claude account changes model rows keeps its own code.
  const save = normalizeClientError(new Error("config_save_failed: disk full"));
  assert.equal(save.code, "config_save_failed");
});

test("redacts Anthropic keys and Claude subscription tokens from technical text", () => {
  const error = normalizeClientError(new Error(
    "claude_subscription_unreachable: request with sk-ant-oat01-abcDEF_123-xyz and sk-ant-api03-QRS_456 failed",
  ));

  assert.doesNotMatch(error.technicalMessage, /abcDEF|QRS_456/);
  assert.doesNotMatch(error.userMessage, /abcDEF|QRS_456/);
  assert.match(error.technicalMessage, /sk-ant-\[redacted\]/);
});

test("maps backend model catalog auth failure via HTTP status in text", () => {
  const error = normalizeClientError(new Error(
    "模型列表鉴权失败（HTTP 401），请检查访问密钥是否有效、是否有模型列表权限",
  ));

  assert.equal(error.code, "authentication_required");
  assert.equal(error.kind, "authentication");
  assert.equal(error.disposition, "blocked");
  assert.equal(error.statusCode, 401);
});

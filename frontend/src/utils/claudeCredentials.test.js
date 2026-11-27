import assert from "node:assert/strict";
import test from "node:test";

import { parseClaudeCredentials } from "./claudeCredentials.js";

const NOW = Date.UTC(2026, 10, 27);
const LATER = NOW + 86_400_000;
const TOKEN = "sk-ant-oat01-AbC_dEf-123";

function credentials(overrides = {}) {
  return JSON.stringify({
    claudeAiOauth: {
      accessToken: TOKEN,
      refreshToken: null,
      expiresAt: LATER,
      scopes: ["user:inference"],
      subscriptionType: "pro",
      ...overrides,
    },
  });
}

test("reads the access token from the Claude CLI credentials file", () => {
  assert.deepEqual(parseClaudeCredentials(credentials(), NOW), {
    ok: true,
    accessToken: TOKEN,
    expiresAt: LATER,
    fromFile: true,
    hasRefreshToken: false,
  });
});

test("reports a refresh token when the file has one", () => {
  const result = parseClaudeCredentials(credentials({ refreshToken: "sk-ant-ort01-xyz" }), NOW);
  assert.equal(result.ok, true);
  assert.equal(result.hasRefreshToken, true);
});

test("accepts the claudeAiOauth object on its own", () => {
  const result = parseClaudeCredentials(JSON.stringify({ accessToken: ` ${TOKEN} `, expiresAt: LATER }), NOW);
  assert.equal(result.ok, true);
  assert.equal(result.accessToken, TOKEN);
});

test("accepts the token alone, with nothing known about its expiry", () => {
  assert.deepEqual(parseClaudeCredentials(`  ${TOKEN}\n`, NOW), {
    ok: true,
    accessToken: TOKEN,
    expiresAt: 0,
    fromFile: false,
    hasRefreshToken: false,
  });
});

test("treats a small expiresAt as seconds", () => {
  const result = parseClaudeCredentials(credentials({ expiresAt: LATER / 1000 }), NOW);
  assert.equal(result.ok, true);
  assert.equal(result.expiresAt, LATER);
});

test("keeps a file without expiresAt", () => {
  const result = parseClaudeCredentials(credentials({ expiresAt: undefined }), NOW);
  assert.equal(result.ok, true);
  assert.equal(result.expiresAt, 0);
});

test("refuses a token that has expired", () => {
  const expiredAt = NOW - 1000;
  assert.deepEqual(parseClaudeCredentials(credentials({ expiresAt: expiredAt }), NOW), {
    ok: false,
    reason: "expired",
    expiresAt: expiredAt,
  });
});

test("names what is wrong with other input", () => {
  assert.equal(parseClaudeCredentials("   ", NOW).reason, "empty");
  assert.equal(parseClaudeCredentials(undefined, NOW).reason, "empty");
  assert.equal(parseClaudeCredentials("{not json", NOW).reason, "invalid_json");
  assert.equal(parseClaudeCredentials("sk-ant-api03-key", NOW).reason, "invalid_json");
  assert.equal(parseClaudeCredentials("{}", NOW).reason, "missing_token");
  assert.equal(parseClaudeCredentials("null", NOW).reason, "missing_token");
  assert.equal(parseClaudeCredentials('"text"', NOW).reason, "missing_token");
  assert.equal(parseClaudeCredentials(JSON.stringify({ claudeAiOauth: { accessToken: 42 } }), NOW).reason, "missing_token");
});

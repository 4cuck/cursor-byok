import assert from "node:assert/strict";
import test from "node:test";
import { normalizeClientError } from "./errorContract.js";
import { toUserErrorWithCause } from "./errorHumanizer.js";

test("normalizeClientError walks cause chain and redacts credentials", () => {
  const grokHexKey = `grok_${"a".repeat(32)}_${"b".repeat(64)}`;
  const xaiKey = `xai-${"A".repeat(80)}`;
  const rawCause = new Error(
    `Invalid runtime call: Unable to parse request body as JSON: crsr_secret_token_12345 ${grokHexKey} ${xaiKey} hint.grok_multi_agent_no_client_tools xai-compaction`,
  );
  const firstPass = normalizeClientError(rawCause, { operation: "SaveUserConfig" });
  const wrapped = new Error(firstPass.userMessage, { cause: rawCause });
  Object.assign(wrapped, firstPass);

  const presented = toUserErrorWithCause(wrapped);
  assert.match(presented, /Unable to parse request body as JSON/);
  assert.doesNotMatch(presented, /crsr_secret_token_12345/);
  assert.doesNotMatch(presented, new RegExp(grokHexKey));
  assert.doesNotMatch(presented, new RegExp(xaiKey));
  assert.match(presented, /crsr_\[redacted\]/);
  assert.match(presented, /grok_\[redacted\]/);
  assert.match(presented, /xai-\[redacted\]/);
  assert.match(presented, /hint\.grok_multi_agent_no_client_tools/);
  assert.match(presented, /xai-compaction/);
});

test("normalizeClientError surfaces backend detail for cursor_model_catalog_failed and config_save_failed", () => {
  const catalogErr = new Error(
    "cursor_model_catalog_failed: Cursor refused the model catalog request (HTTP 403): API key revoked",
  );
  const catalogMsg = toUserErrorWithCause(catalogErr);
  assert.match(catalogMsg, /Cursor rejected the model catalog request/);
  assert.match(catalogMsg, /API key revoked/);

  const saveErr = new Error(
    "config_save_failed: Model adapter id cannot be reused by a different model configuration",
  );
  const saveMsg = toUserErrorWithCause(saveErr);
  assert.match(saveMsg, /Saving the model list failed/);
  assert.match(saveMsg, /Model adapter id cannot be reused/);
});

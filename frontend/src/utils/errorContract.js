// Error presentation for the whole UI. Messages are English by design: the
// backend returns stable codes, and this module turns them into one readable
// sentence. Keep user-visible copy here so localization stays in one place.

const STATUS_CODES = new Map([
  [400, { code: "configuration_invalid", kind: "configuration", disposition: "blocked", message: "Request parameters or configuration are invalid. Check your configuration." }],
  [401, { code: "authentication_required", kind: "authentication", disposition: "blocked", message: "Invalid key or no available quota. Check the supplier configuration." }],
  [402, { code: "quota_exceeded", kind: "quota", disposition: "blocked", message: "This supplier has insufficient quota. Check the account or switch suppliers." }],
  [403, { code: "permission_denied", kind: "permission", disposition: "blocked", message: "This key has no access permission. Check the supplier configuration." }],
  [404, { code: "resource_not_found", kind: "configuration", disposition: "blocked", message: "The model or endpoint does not exist. Check your configuration." }],
  [408, { code: "timeout", kind: "timeout", disposition: "retryable", message: "Request timed out. Preparing recovery." }],
  [429, { code: "rate_limited", kind: "rate_limit", disposition: "retryable", message: "Too many requests. It will retry automatically shortly." }],
  [500, { code: "provider_unavailable", kind: "provider", disposition: "retryable", message: "Supplier is temporarily unavailable. Preparing recovery." }],
  [502, { code: "provider_unavailable", kind: "provider", disposition: "retryable", message: "Supplier gateway error. Preparing recovery." }],
  [503, { code: "provider_unavailable", kind: "provider", disposition: "retryable", message: "Supplier is temporarily unavailable. Preparing recovery." }],
  [504, { code: "provider_unavailable", kind: "provider", disposition: "retryable", message: "Supplier gateway timed out. Preparing recovery." }],
]);

export const MCP_WORKSPACE_TRUST_REQUIRED_CODE = "mcp_workspace_trust_required";

// Account switch / import codes from internal/cursoraccount. These cross the
// Wails boundary as text only, so they are matched by name. Without this table
// every account failure degraded into a generic "service error" message.
const ACCOUNT_ERROR_MESSAGES = new Map([
  ["account_not_found", "Account not found. Refresh the account list."],
  ["account_unusable", "This account is missing required credentials. Import it again."],
  ["account_switch_busy", "A switch is already in progress. Try again in a moment."],
  ["confirmation_expired", "The confirmation expired. Start the switch again."],
  ["confirmation_already_used", "This confirmation was already used. Start the switch again."],
  ["cursor_process_probe_failed", "Could not detect the Cursor process. Verify Cursor is installed."],
  ["cursor_state_db_missing", "Cursor's state database was not found. Launch Cursor once first."],
  ["cursor_process_close_failed", "Could not close Cursor. Quit it manually and retry."],
  ["account_switch_backup_failed", "Backing up Cursor state failed. Nothing was modified."],
  ["account_switch_write_failed", "Writing the Cursor login state failed."],
  ["account_switch_verify_failed", "Verifying the written Cursor login state failed."],
  ["cursor_restart_failed", "Restarting Cursor failed. Rolled back to the previous state."],
  ["account_switch_rollback_failed", "The switch failed and the rollback also failed. Check Cursor manually."],
  ["account_import_api_key_rejected", "This API key is invalid or has been revoked."],
  ["account_import_api_key_unreachable", "Could not reach Cursor. Check your network or proxy."],
  ["account_import_api_key_invalid_response", "Cursor returned an unexpected response for this API key."],
  ["account_import_identity_unavailable", "Could not read this account's identity. Verify the credential is valid."],
  ["account_import_invalid_api_key", "API key format is invalid. It must start with crsr_ or grok_."],
  ["account_import_xai_api_key", "This is an xAI Console API key (xai-...). Add it under Third-party suppliers → xAI (Grok) instead; Cursor accounts use a crsr_ or grok_ dashboard key or OAuth sign-in."],
  ["account_import_empty", "No credential was provided."],
  ["account_import_too_large", "The credential is too large."],
  ["account_import_invalid_schema", "The import file does not match the expected format."],
  ["account_import_local_state_missing", "No local Cursor login state was found to import."],
  ["account_export_empty", "No accounts were selected to export."],
  ["cursor_account_service_unavailable", "The Cursor account service is not available. Restart the app and retry."],
  ["cursor_account_sign_in_required", "No usable Cursor session. Import the account or set another account as current."],
  ["cursor_model_catalog_failed", "Cursor rejected the model catalog request. Check the account's sign-in and network, then retry."],
  ["config_save_failed", "Saving the model list failed. The configuration was rejected."],
  ["cursor_restore_failed", "The configuration saved, but restoring Cursor's official login failed. Restart Cursor and retry."],
  ["cursor_account_reinject_failed", "The configuration saved, but installing the selected Cursor account failed. Restart Cursor and retry."],
]);

const DIAGNOSTIC_CODES = new Map([
  [MCP_WORKSPACE_TRUST_REQUIRED_CODE, {
    code: MCP_WORKSPACE_TRUST_REQUIRED_CODE,
    kind: "authorization",
    disposition: "user_action_required",
    message: "The workspace MCP configuration must be trusted before connecting.",
  }],
]);

const SAFE_TRACE_ID = /^[A-Za-z0-9._:-]{1,128}$/;
const SENSITIVE_KEYS = /key|token|secret|password|authorization|cookie|credential|prompt|message|body|content|request|response|error|stack|cause/i;

function asText(value) {
  if (typeof value === "string") return value.trim();
  if (value instanceof Error) return value.message.trim();
  if (value && typeof value === "object") {
    return String(value.message || value.error || value.rawResponse || "").trim();
  }
  return value == null ? "" : String(value).trim();
}

function deepestCauseText(error) {
  let node = error && typeof error === "object" ? error.cause : null;
  for (let depth = 0; node && depth < 6; depth += 1) {
    const candidate = typeof node === "string" ? node.trim() : asText(node);
    if (candidate && candidate !== "unknown error") {
      return candidate;
    }
    node = typeof node === "object" ? node.cause : null;
  }
  return "";
}

function redactTechnicalText(raw) {
  const text = String(raw || "").trim();
  if (!text) return "unknown error";
  return text
    .replace(/(authorization\s*[=:]\s*)(?:bearer\s+)?[^\s,;&?#]+/gi, "$1[redacted]")
    .replace(/\bcrsr_[A-Za-z0-9_-]+/g, "crsr_[redacted]")
    .replace(/\bgrok_(?:[0-9a-fA-F]{16,}(?:_[0-9a-fA-F]{16,})+|(?=[A-Za-z0-9_-]*\d)[A-Za-z0-9_-]{12,})/g, "grok_[redacted]")
    .replace(/\bxai-[A-Za-z0-9]{20,}\b/g, "xai-[redacted]")
    .replace(/([?&](?:api[_-]?key|token|secret|password)=)[^&\s]*/gi, "$1[redacted]")
    .replace(/((?:api[_-]?key|token|secret|password)\s*[=:]\s*)[^\s,;&?#]+/gi, "$1[redacted]")
    .slice(0, 1000);
}

function makeTraceId() {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `ui-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

export function extractStatusCode(error) {
  const explicit = Number(error?.statusCode || error?.status || error?.code);
  if (Number.isInteger(explicit) && explicit >= 100 && explicit < 600) return explicit;
  const match = asText(error).match(/(?:status|HTTP)[=:\s]+(\d{3})/i) || asText(error).match(/\b(\d{3})\b/);
  return match ? Number(match[1]) : 0;
}

function technicalMessage(error) {
  if (typeof error?.technicalMessage === "string" && error.technicalMessage.trim() && error.technicalMessage.trim() !== "unknown error") {
    return redactTechnicalText(error.technicalMessage);
  }
  const causeText = deepestCauseText(error);
  const directText = asText(error);
  if (causeText && causeText !== directText) {
    return redactTechnicalText(causeText);
  }
  return redactTechnicalText(directText);
}

function safeTraceId(value) {
  const text = String(value || "").trim();
  return SAFE_TRACE_ID.test(text) ? text : makeTraceId();
}

export function normalizeClientError(error, { operation = "ui.operation", traceId = "" } = {}) {
  const directText = asText(error);
  const causeText = deepestCauseText(error);
  const text = (causeText && (!directText || directText === error?.userMessage)) ? causeText : directText;
  const lower = text.toLowerCase();
  const status = extractStatusCode(error) || (causeText ? extractStatusCode(causeText) : 0);
  const structuredCode = typeof error?.code === "string" ? error.code.trim() : "";
  const messageCode = lower.match(/\bmcp_workspace_trust_required\b/)?.[0] || "";
  const diagnosticCode = DIAGNOSTIC_CODES.has(structuredCode) ? structuredCode : messageCode;
  const mapped = DIAGNOSTIC_CODES.get(diagnosticCode) || STATUS_CODES.get(status);

  // Account codes take precedence over HTTP status mapping: the backend reports
  // these by stable name, and a bare number inside the message would otherwise
  // replace the specific cause with a generic status sentence.
  let accountHit = null;
  for (const [code, message] of ACCOUNT_ERROR_MESSAGES) {
    if (structuredCode === code || lower.includes(code)) {
      accountHit = { code, kind: "account", disposition: "blocked", message };
      break;
    }
  }
  let result = accountHit || mapped;

  // Keep the backend's own explanation when an account/config failure includes detail.
  if (accountHit && (result.code === "cursor_model_catalog_failed" || result.code === "config_save_failed")) {
    const idx = lower.indexOf(result.code);
    if (idx >= 0) {
      const detail = redactTechnicalText(text.slice(idx + result.code.length).replace(/^[\s:]+/, "").trim());
      if (detail && detail !== "unknown error") {
        result = { ...result, message: `${result.message} (${detail})` };
      }
    }
  }

  // Anything else the Cursor account layer reports is still far more useful than
  // the generic fatal sentence. Cursor account failures carry no HTTP status, so
  // without this fallback every expired session, revoked key, or unresolved
  // account id looked identical to the user.
  if (!result && /cursor account|cursor_account|sign in to a cursor/i.test(text)) {
    result = {
      code: "cursor_account_error",
      kind: "account",
      disposition: "blocked",
      message: `Cursor account error: ${text}`,
    };
  }

  if (!result && (lower.includes("cancel") || lower.includes("aborted") || lower.includes("已取消"))) {
    result = { code: "canceled", kind: "canceled", disposition: "canceled", message: "The operation was cancelled." };
  } else if (!result && (lower.includes("timeout") || lower.includes("deadline") || lower.includes("超时"))) {
    result = { code: "timeout", kind: "timeout", disposition: "retryable", message: "Request timed out. Preparing recovery." };
  } else if (!result && (
    // Backend error text crosses the Wails boundary verbatim, so cover both
    // English and Chinese network wording; otherwise "proxy not running"
    // degrades into a generic service error.
    lower.includes("network")
    || lower.includes("connection")
    || lower.includes("proxyconnect")
    || lower.includes("proxy")
    || lower.includes("tls")
    || lower.includes("x509")
    || lower.includes("no such host")
    || lower.includes("failed to fetch")
    || lower.includes("网络")
    || lower.includes("代理")
    || /\beof\b/.test(lower)
  )) {
    result = { code: "network_error", kind: "network", disposition: "retryable", message: "Cannot reach the service right now. Preparing recovery." };
  }

  result ||= { code: "internal_error", kind: "internal", disposition: "fatal", message: "The service encountered an error. Retry or export diagnostic information." };
  const structuredKind = typeof error?.kind === "string" ? error.kind.trim() : "";
  const structuredDisposition = typeof error?.disposition === "string" ? error.disposition.trim() : "";
  const structuredUserMessage = typeof error?.userMessage === "string" ? error.userMessage.trim() : "";
  const structuredTechnicalMessage = typeof error?.technicalMessage === "string"
    ? error.technicalMessage
    : technicalMessage(error);
  const retryAfter = Number(error?.retryAfterMs || error?.retryAfter || 0);
  return {
    operation,
    code: diagnosticCode || structuredCode || result.code,
    kind: structuredKind || result.kind,
    disposition: structuredDisposition || result.disposition,
    retryAfterMs: Number.isFinite(retryAfter) && retryAfter > 0 ? retryAfter : 0,
    userMessage: structuredUserMessage || result.message,
    technicalMessage: technicalMessage(structuredTechnicalMessage),
    traceId: safeTraceId(traceId || error?.traceId),
    statusCode: status,
    cause: error,
  };
}

export function safeErrorLogAttributes(error, options = {}) {
  const normalized = normalizeClientError(error, options);
  return {
    operation: normalized.operation,
    code: normalized.code,
    kind: normalized.kind,
    disposition: normalized.disposition,
    statusCode: normalized.statusCode,
    traceId: normalized.traceId,
  };
}

export function isRetryableError(error) {
  return normalizeClientError(error).disposition === "retryable";
}

export function isBlockedError(error) {
  return normalizeClientError(error).disposition === "blocked";
}

export function toUserError(error) {
  return normalizeClientError(error).userMessage;
}

export function summarizePayload(payload, depth = 0) {
  if (payload == null) return payload;
  if (Array.isArray(payload)) return { type: "array", length: payload.length };
  if (typeof payload !== "object") {
    return typeof payload === "string" ? `${payload.slice(0, 80)}${payload.length > 80 ? "…" : ""}` : payload;
  }
  if (depth >= 3) return { type: "object", keys: Object.keys(payload).slice(0, 20) };
  return Object.fromEntries(Object.entries(payload).map(([key, value]) => [
    key,
    SENSITIVE_KEYS.test(key) ? "[redacted]" : summarizePayloadValue(value, depth + 1),
  ]));
}

function summarizePayloadValue(value, depth) {
  if (Array.isArray(value)) return { type: "array", length: value.length };
  if (value && typeof value === "object") return summarizePayload(value, depth);
  if (typeof value === "string") return value.length > 160 ? `${value.slice(0, 160)}…` : value;
  return value;
}
import { extractStatusCode as extractStructuredStatusCode, normalizeClientError } from "./errorContract.js";

// Turns raw provider/adapter errors into one readable English sentence.
//
// Adapter errors look like `anthropic adapter status=404 body={...}`, which is
// useless in a dialog. This module groups them by HTTP status and keywords,
// while the raw text stays available for diagnostics.

const STATUS_MESSAGES = {
  400: "Request rejected (this model may not support the given parameters)",
  401: "Invalid key or no permission for this model",
  403: "Invalid key or no permission for this model",
  404: "This model does not exist or is not enabled for this supplier",
  408: "Request timed out; the supplier did not respond",
  429: "Rate limited. Try again shortly.",
  500: "Supplier service error. Try again shortly.",
  502: "Supplier gateway error. Try again shortly.",
  503: "Supplier temporarily unavailable. Try again shortly.",
  504: "Supplier gateway timed out. Try again shortly.",
};

function rawText(error) {
  if (error == null) return "";
  if (typeof error === "string") return error.trim();
  if (typeof error === "object") {
    return String(error.message || error.error || error.rawResponse || "").trim();
  }
  return String(error).trim();
}

/** Extracts the upstream HTTP status code from a raw error, or 0. */
export function extractStatusCode(error) {
  return extractStructuredStatusCode(error);
}

/**
 * Translates a raw error into readable English, falling back to the raw text.
 * @param {unknown} error raw error (string / Error / {message,error})
 * @returns {string}
 */
export function humanizeProviderError(error) {
  const text = rawText(error);
  if (!text) return "Request failed";

  const lower = text.toLowerCase();
  if (lower.includes("timeout") || lower.includes("deadline") || lower.includes("超时")) {
    return "Request timed out; the supplier did not respond";
  }
  if (lower.includes("context canceled") || lower.includes("已取消")) {
    return "Request cancelled";
  }
  if (lower.includes("api key") || lower.includes("apikey") || lower.includes("unauthorized")) {
    return "Invalid key or no permission for this model";
  }
  if (lower.includes("no such host") || lower.includes("connection refused") || lower.includes("dial tcp")) {
    return "Cannot connect to the endpoint. Check the network or the URL.";
  }

  const status = extractStatusCode(text);
  if (status && STATUS_MESSAGES[status]) {
    return STATUS_MESSAGES[status];
  }
  if (status >= 500) return "Supplier service error. Try again shortly.";
  if (status >= 400) return "Request rejected. Check the model and parameter configuration.";

  // Unclassifiable: keep the raw text so the cause is not lost (truncated).
  return text.length > 160 ? `${text.slice(0, 160)}…` : text;
}

// toUserError delegates to the structured presenter; string parsing stays in
// the errorContract compatibility entry point.
export function toUserError(error) {
  return normalizeClientError(error).userMessage;
}

/**
 * toUserErrorWithCause appends a redacted technical reason (truncated to 160
 * chars). Generic copy such as "cannot reach the service" is not enough to
 * locate a fault; the underlying cause (for example
 * `dial tcp 127.0.0.1:7890: connection refused`) tells the user immediately
 * that the local proxy is unreachable.
 * @param {unknown} error raw error
 * @returns {string}
 */
export function toUserErrorWithCause(error) {
  const normalized = normalizeClientError(error);
  const user = normalized.userMessage || "";
  const technical = String(normalized.technicalMessage || "").trim();
  if (!technical || technical === user) return user;
  const detail = technical.length > 160 ? `${technical.slice(0, 160)}…` : technical;
  return `${user} (${detail})`;
}
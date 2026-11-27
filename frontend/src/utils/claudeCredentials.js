// Reads a Claude subscription access token out of what the user pasted: the
// Claude CLI's credentials.json (~/.claude/credentials.json), the claudeAiOauth
// object inside it, or the token alone.

const ACCESS_TOKEN_PATTERN = /^sk-ant-oat\d*-[A-Za-z0-9_-]+$/;

// The Claude CLI writes expiresAt in milliseconds. A value this small can only
// be seconds.
const SECONDS_CEILING = 1e11;

function expiryMilliseconds(value) {
  const expiresAt = Number(value);
  if (!Number.isFinite(expiresAt) || expiresAt <= 0) return 0;
  return expiresAt < SECONDS_CEILING ? expiresAt * 1000 : expiresAt;
}

/**
 * @param {unknown} input The pasted text.
 * @param {number} [now] The current time in milliseconds.
 * @returns {{ ok: true, accessToken: string, expiresAt: number, fromFile: boolean, hasRefreshToken: boolean }
 *   | { ok: false, reason: "empty" | "invalid_json" | "missing_token" | "expired", expiresAt: number }}
 *   expiresAt is in milliseconds and 0 when it is not known.
 */
export function parseClaudeCredentials(input, now = Date.now()) {
  const raw = String(input ?? "").trim();
  if (!raw) return { ok: false, reason: "empty", expiresAt: 0 };
  if (ACCESS_TOKEN_PATTERN.test(raw)) {
    return { ok: true, accessToken: raw, expiresAt: 0, fromFile: false, hasRefreshToken: false };
  }

  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { ok: false, reason: "invalid_json", expiresAt: 0 };
  }
  const oauth = parsed && typeof parsed === "object" ? parsed.claudeAiOauth ?? parsed : null;
  const accessToken = typeof oauth?.accessToken === "string" ? oauth.accessToken.trim() : "";
  if (!accessToken) return { ok: false, reason: "missing_token", expiresAt: 0 };

  const expiresAt = expiryMilliseconds(oauth.expiresAt);
  if (expiresAt > 0 && expiresAt <= now) return { ok: false, reason: "expired", expiresAt };
  return {
    ok: true,
    accessToken,
    expiresAt,
    fromFile: true,
    hasRefreshToken: typeof oauth.refreshToken === "string" && oauth.refreshToken.trim() !== "",
  };
}

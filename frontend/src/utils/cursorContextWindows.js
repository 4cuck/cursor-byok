// Context windows Cursor offers for a cursor_account model. "Sync models" stores
// them in the adapter's cursorModelSpec as `contextWindows`; the backend derives
// them from Cursor's AvailableModels catalog (context parameter and max-mode variants).

/**
 * @typedef {object} CursorContextWindow
 * @property {number} tokens
 * @property {string} value    Cursor's context parameter value, such as "1m"; empty without a context parameter.
 * @property {boolean} maxMode Cursor serves this window only in Max mode.
 * @property {boolean} isDefault Cursor's default window for the model.
 */

/** @returns {CursorContextWindow[]} smallest first; empty when the spec was synced before windows were listed. */
export function cursorContextWindows(specJSON) {
  const raw = String(specJSON || "").trim();
  if (!raw) return [];
  let spec;
  try {
    spec = JSON.parse(raw);
  } catch {
    return [];
  }
  const items = Array.isArray(spec?.contextWindows) ? spec.contextWindows : [];
  const windows = [];
  const seen = new Set();
  for (const item of items) {
    const tokens = Number(item?.tokens);
    if (!Number.isInteger(tokens) || tokens <= 0 || seen.has(tokens)) continue;
    seen.add(tokens);
    windows.push({
      tokens,
      value: String(item?.value || ""),
      maxMode: item?.maxMode === true,
      isDefault: item?.default === true,
    });
  }
  return windows.sort((left, right) => left.tokens - right.tokens);
}

export function defaultCursorContextWindow(windows) {
  return windows.find((option) => option.isDefault) || windows[0] || null;
}

/** Selected window when Cursor offers it, else Cursor's default; 0 when the spec lists no windows. */
export function resolveCursorContextWindowTokens(specJSON, selectedTokens) {
  const windows = cursorContextWindows(specJSON);
  if (!windows.length) return 0;
  const selected = Number(selectedTokens);
  const match = windows.find((option) => option.tokens === selected);
  return (match || defaultCursorContextWindow(windows)).tokens;
}

/** 272000 → "272K", 1000000 → "1M", 1048576 → "1.05M". */
export function formatContextWindowTokens(tokens) {
  const value = Number(tokens);
  if (!Number.isFinite(value) || value <= 0) return "";
  if (value >= 1_000_000) {
    return `${trimFraction((value / 1_000_000).toFixed(2))}M`;
  }
  if (value >= 1_000) {
    return `${trimFraction((value / 1_000).toFixed(1))}K`;
  }
  return String(Math.round(value));
}

function trimFraction(text) {
  return text.includes(".") ? text.replace(/\.?0+$/, "") : text;
}

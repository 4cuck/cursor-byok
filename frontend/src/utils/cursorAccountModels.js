// Turns the models exposed by the signed-in Cursor account into
// `cursor_account` model adapters.
//
// Why this exists: importing a `crsr_` API key gives cursor-byok a usable
// upstream identity, but the model list is empty, so nothing can be routed to
// it. Once the account's catalog is known, adapters can be materialized and the
// user can pick between the official upstream and third-party suppliers in the
// same model list.
import {
  CREDENTIAL_SCOPE_CURSOR_ACCOUNT,
  MODEL_SOURCE_CURSOR_ACCOUNT,
  createEmptyModelAdapter,
  normalizeModelAdapter,
} from "./modelAdapter.js";

const CURSOR_ACCOUNT_SUPPLIER_ID = "cursor_account";

export function cursorAccountSlotLabel(value) {
  const label = String(value || "").trim();
  return /^cursor_account\d+$/.test(label) ? label : "";
}

/** Every Cursor account shares one model list. Set as current chooses which account serves it. */
export function cursorAccountOwnerLabel() {
  return CURSOR_ACCOUNT_SUPPLIER_ID;
}

export function isSharedCursorAccountAdapter(adapter) {
  if (String(adapter?.source || "").trim().toLowerCase() !== MODEL_SOURCE_CURSOR_ACCOUNT) return false;
  if (String(adapter?.cursorAccountID || "").trim()) return false;
  if (cursorAccountSlotLabel(adapter?.supplierID) || cursorAccountSlotLabel(adapter?.groupName)) return false;
  const supplier = String(adapter?.supplierID || "").trim();
  const group = String(adapter?.groupName || "").trim();
  const id = String(adapter?.id || "");
  return supplier === CURSOR_ACCOUNT_SUPPLIER_ID
    || group === "Cursor account"
    || group === CURSOR_ACCOUNT_SUPPLIER_ID
    || id.startsWith(`${CURSOR_ACCOUNT_SUPPLIER_ID}:`);
}

/** Builds one cursor_account adapter for a model name. */
export function buildCursorAccountAdapter(model, owner = {}) {
  const name = String(model?.name || "").trim();
  if (!name) return null;
  const displayName = String(model?.displayName || name).trim() || name;
  const ownerLabel = owner.label === CURSOR_ACCOUNT_SUPPLIER_ID || cursorAccountSlotLabel(owner.label)
    ? String(owner.label).trim()
    : CURSOR_ACCOUNT_SUPPLIER_ID;
  const email = String(owner.email || "").trim();
  const note = email ? `${email} · ${ownerLabel}` : ownerLabel;
  return normalizeModelAdapter({
    ...createEmptyModelAdapter(),
    // Stable id per account: re-importing the same model updates it, and a
    // second Cursor account does not overwrite the first account's copy.
    id: `${ownerLabel}:${name}`,
    source: MODEL_SOURCE_CURSOR_ACCOUNT,
    credentialScope: CREDENTIAL_SCOPE_CURSOR_ACCOUNT,
    displayName,
    groupName: ownerLabel,
    type: MODEL_SOURCE_CURSOR_ACCOUNT,
    supplierID: ownerLabel,
    cursorAccountID: String(owner.accountID || "").trim(),
    modelID: name,
    cursorModelSpec: String(model?.pickerSpec || "").trim(),
    contextWindowTokens: Number(model?.contextWindowTokens) || 0,
    tooltipData: model?.defaultOn ? `${note} (default)` : note,
    customHeadersEnabled: false,
    customHeadersJSON: "",
    balanceProfile: "none",
  });
}

/**
 * Converts an account model catalog into adapters.
 * @param {{models?: Array<{name: string, displayName?: string}>}} catalog
 * @returns {Array<object>} adapters ready for saveModelAdaptersBatch
 */
export function buildCursorAccountAdapters(catalog, owner = {}) {
  const models = Array.isArray(catalog?.models) ? catalog.models : [];
  const adapters = [];
  const seen = new Set();
  for (const model of models) {
    const adapter = buildCursorAccountAdapter(model, owner);
    if (!adapter) continue;
    if (seen.has(adapter.id)) continue;
    seen.add(adapter.id);
    adapters.push(adapter);
  }
  return adapters;
}

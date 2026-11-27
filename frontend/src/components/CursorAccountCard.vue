<script setup>
import ActionMenu from "@/components/ui/ActionMenu.vue";
import Button from "@/components/ui/Button.vue";
import Card from "@/components/ui/Card.vue";
import Tooltip from "@/components/ui/Tooltip.vue";
import { useMessage } from "@/composables/useMessage";
import { showModal } from "@/composables/useModal";
import {
  beginCursorAccountLogin,
  cancelCursorAccountLogin,
  checkCursorAccountHealth,
  clearCursorAccountLock,
  clearCursorAccountQuotaCooldown,
  deleteCursorAccounts,
  executeCursorAccountRecoveryExport,
  executeCursorClientAccountSwitch,
  getCursorAccountLoginStatus,
  getCursorAccountPlan,
  getCursorAccountSpendLimit,
  listCursorAccountApiKeys,
  createCursorAccountApiKey,
  importCursorAccount,
  listCursorAccounts,
  prepareCursorAccountRecoveryExport,
  prepareCursorClientAccountSwitchWithOptions,
  setCurrentCursorAccount,
  setCursorAccountSpendLimit,
  syncCursorAccountModelsRpc,
  updateCursorAccountTags,
} from "@/services/clientApi";
import copyTextToClipboard from "copy-text-to-clipboard";
import { notifyAccountSync } from "@/utils/accountSync";
import {
  autoBucketCaption,
  autoBucketUsedPercent,
  bonusUsageCaption,
  credentialTypeLabel,
  formatPlanCents,
  formatSpendLimitSummary,
  namedBucketCaption,
  namedBucketUsedPercent,
  onDemandLimitMode,
  onDemandUsageCaption,
  renewalLabel,
  subscriptionStatusLabel,
  usageBarClass,
  usageRemainingCaption,
  usedBarClass,
  usedUsagePercent,
  validateSpendLimitInput,
} from "@/utils/cursorAccountPlan";
import { appState, reloadUserConfig, toUserError, toUserErrorWithCause } from "@/state/appState";
import { Browser } from "@wailsio/runtime";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useRouter } from "vue-router";

const props = defineProps({
  showControlCenterLink: { type: Boolean, default: true },
  variant: { type: String, default: "home" }, // home | panel
});

const CURSOR_ACCOUNT_CONTRIBUTOR_URL = "https://github.com/aike0210";
const message = useMessage();
const router = useRouter();
const accounts = ref([]);
const busy = ref(false);
const syncingAccountID = ref("");
const loginSessionId = ref("");
const loginTimer = ref(0);
const importDialog = ref(null);
const importValue = ref("");
const tagDialog = ref(null);
const tagValue = ref("");
const spendLimitDialog = ref(null);
const spendLimitLoading = ref(false);
const spendLimitSaving = ref(false);
const spendLimitSnapshot = ref(null);
const spendLimitMode = ref("disabled");
const spendLimitAmount = ref("50");
const spendLimitError = ref("");
const apiKeysDialog = ref(null);
const apiKeysLoading = ref(false);
const apiKeys = ref([]);
const apiKeysError = ref("");
const apiKeyShowCreate = ref(false);
const apiKeyNameMode = ref("random");
const apiKeyCustomName = ref("");
const apiKeyCreating = ref(false);
const createdApiKey = ref(null);
const createdApiKeyCopied = ref(false);

const SPEND_LIMIT_PRESETS = [20, 50, 100, 200, 500];

const isPanel = computed(() => props.variant === "panel");
const loginWaiting = computed(() => Boolean(loginSessionId.value));

function accountInitial(email) {
  const text = String(email || "?").trim();
  return text.charAt(0).toUpperCase();
}

function accountHint(account) {
  const hint = String(account?.authIdHint || "").trim();
  return hint ? `ID ${hint}` : "";
}

// Live count & slot label of synced cursor_account models owned by each account.
const syncedStatsByAccountID = computed(() => {
  const adapters = Array.isArray(appState?.modelAdapters) ? appState.modelAdapters : [];
  const map = new Map();
  for (const adapter of adapters) {
    if (String(adapter?.source || "").trim().toLowerCase() !== "cursor_account") continue;
    const owner = String(adapter?.cursorAccountID || "").trim();
    if (!owner) continue;
    const slot = String(adapter?.supplierID || adapter?.groupName || "").trim();
    const entry = map.get(owner) || { count: 0, slot: "" };
    entry.count += 1;
    if (!entry.slot && slot) entry.slot = slot;
    map.set(owner, entry);
  }
  return map;
});

function syncedSummaryFor(account) {
  if (!account?.id) return null;
  return syncedStatsByAccountID.value.get(account.id) || null;
}

const syncedModelCount = computed(() => {
  const adapters = Array.isArray(appState?.modelAdapters) ? appState.modelAdapters : [];
  return adapters.filter((adapter) => String(adapter?.source || "").trim().toLowerCase() === "cursor_account").length;
});

// Cursor caps distinct devices per account per 24h. Once tripped, every attempt
// fails until the window rolls over, so the card shows the cooldown instead of
// letting the user retry blindly.
function isAccountLocked(account) {
  const until = Number(account?.lockedUntilUnixMs || 0);
  return until > 0 && Date.now() < until;
}

function formatLockRemaining(account) {
  const until = Number(account?.lockedUntilUnixMs || 0);
  const remainingMs = until - Date.now();
  if (!(remainingMs > 0)) return "";
  const totalMinutes = Math.ceil(remainingMs / 60000);
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours <= 0) return `${minutes}m`;
  return minutes > 0 ? `${hours}h ${minutes}m` : `${hours}h`;
}

async function handleForceUnlock(account) {
  const confirmed = await showModal({
    title: "Try anyway?",
    content:
      "Cursor locked this account because too many devices used it in the last 24 hours. " +
      "Clearing the cooldown lets you attempt a request now, but it will most likely fail again " +
      "and may extend the lock.",
    confirmText: "Try anyway",
    cancelText: "Keep waiting",
  });
  if (!confirmed) return;
  busy.value = true;
  try {
    await clearCursorAccountLock(account.id);
    await afterAccountChanged();
    message.success("Cooldown cleared. You can try again.");
  } catch (error) {
    await showActionError("Could not clear the cooldown", error);
  } finally {
    busy.value = false;
  }
}

async function handleClearQuotaCooldown(account) {
  if (!account?.id || busy.value) return;
  busy.value = true;
  try {
    const cleared = await clearCursorAccountQuotaCooldown(account.id);
    await refreshAccountPlan(account);
    message.success(cleared > 0 ? `Cleared quota cooldown for ${cleared} model(s).` : "No active model quota cooldowns.");
  } catch (error) {
    await showActionError("Could not clear quota cooldown", error);
  } finally {
    busy.value = false;
  }
}

// ─── Account health & live plan usage ────────────────────────────────────────
const healthByID = ref({});
const plansByID = ref({});
const planLoadingIDs = ref({});
const refreshingAllPlans = ref(false);
const checkingAccountID = ref("");

function healthFor(account) {
  if (!account?.id) return null;
  return healthByID.value[account.id] || null;
}

function planFor(account) {
  if (!account?.id) return null;
  return plansByID.value[account.id] || null;
}

function isChecking(account) {
  return checkingAccountID.value !== "" && checkingAccountID.value === account?.id;
}

function isSyncingAccount(account) {
  return syncingAccountID.value !== "" && (syncingAccountID.value === "*" || syncingAccountID.value === account?.id);
}

function isPlanLoading(account) {
  return Boolean(account?.id && planLoadingIDs.value[account.id]);
}

function statusClass(status) {
  switch (String(status || "").toLowerCase()) {
    case "active":
    case "trialing":
      return "border-[#2f6b49] text-[#7dd3a0]";
    case "past_due":
    case "unpaid":
    case "incomplete":
      return "border-[#6b4f1a] text-[#f2c66d]";
    case "canceled":
    case "cancelled":
      return "border-[#4b1d1d] text-[#fca5a5]";
    default:
      return "border-[#3f3f3f] text-[#a3a3a3]";
  }
}

const HEALTH_LABELS = {
  ok: "Working",
  locked: "Locked",
  rejected: "Rejected",
  expired: "Session expired",
  plan_required: "Plan required",
  usage_exhausted: "Quota exhausted",
  error: "Error",
  signed_out: "Not signed in",
};

function healthLabel(entry) {
  if (!entry) return "";
  return HEALTH_LABELS[entry.state] || entry.state || "Unknown";
}

function healthBadgeClass(entry) {
  if (!entry) return "border-[#3f3f3f] text-[#a3a3a3]";
  switch (entry.state) {
    case "ok":
      return "border-[#2f6b49] bg-[#16281e] text-[#7dd3a0]";
    case "locked":
    case "plan_required":
    case "usage_exhausted":
      return "border-[#6b4f1a] bg-[#302512] text-[#f2c66d]";
    case "signed_out":
      return "border-[#3f3f3f] text-[#a3a3a3]";
    default:
      return "border-[#4b1d1d] bg-[#2a1313] text-[#fca5a5]";
  }
}

function healthIcon(entry) {
  if (!entry) return "icon-[mdi--help-circle-outline] text-[12px]";
  switch (entry.state) {
    case "ok":
      return "icon-[mdi--check-circle] text-[12px]";
    case "locked":
      return "icon-[mdi--lock-clock] text-[12px]";
    case "usage_exhausted":
      return "icon-[mdi--gauge-full] text-[12px]";
    case "plan_required":
      return "icon-[mdi--credit-card-off-outline] text-[12px]";
    case "signed_out":
      return "icon-[mdi--account-off-outline] text-[12px]";
    default:
      return "icon-[mdi--alert-circle] text-[12px]";
  }
}

function healthPanelClass(entry) {
  if (!entry) return "border border-[#3f3f3f] text-[#a3a3a3]";
  switch (entry.state) {
    case "ok":
      return "border border-[#2f6b49] bg-[#16281e] text-[#7dd3a0]";
    case "locked":
    case "plan_required":
    case "usage_exhausted":
      return "border border-[#6b4f1a] bg-[#302512] text-[#f2c66d]";
    case "signed_out":
      return "border border-[#3f3f3f] text-[#a3a3a3]";
    default:
      return "border border-[#4b1d1d] bg-[#2a1313] text-[#fca5a5]";
  }
}

const accountStatusCacheKey = "cursor-byok.cursor-account-status";

function readAccountStatusCache() {
  try {
    const parsed = JSON.parse(localStorage.getItem(accountStatusCacheKey) || "{}");
    return parsed && typeof parsed === "object" ? parsed : {};
  } catch {
    return {};
  }
}

function restoreAccountStatus(list) {
  const cache = readAccountStatusCache();
  const health = {};
  const plans = {};
  for (const account of list || []) {
    const saved = cache[account?.id];
    if (!saved) continue;
    if (saved.health) health[account.id] = saved.health;
    if (saved.plan) plans[account.id] = saved.plan;
  }
  healthByID.value = health;
  plansByID.value = plans;
}

function persistAccountStatus() {
  try {
    const cache = readAccountStatusCache();
    for (const account of accounts.value || []) {
      if (!account?.id) continue;
      cache[account.id] = {
        health: healthByID.value[account.id] || null,
        plan: plansByID.value[account.id] || null,
      };
    }
    localStorage.setItem(accountStatusCacheKey, JSON.stringify(cache));
  } catch {
    /* ignore storage quota errors */
  }
}

function clearCachedAccountHealth(accountID) {
  const id = String(accountID || "").trim();
  if (!id) return;
  if (healthByID.value[id]) {
    const next = { ...healthByID.value };
    delete next[id];
    healthByID.value = next;
  }
  try {
    const cache = readAccountStatusCache();
    if (cache[id]?.health) {
      cache[id] = { ...cache[id], health: null };
      localStorage.setItem(accountStatusCacheKey, JSON.stringify(cache));
    }
  } catch {
    /* ignore storage quota errors */
  }
}

async function refreshAccountPlan(account) {
  if (!account?.id || isAccountLocked(account)) return;
  planLoadingIDs.value = { ...planLoadingIDs.value, [account.id]: true };
  try {
    const plan = await getCursorAccountPlan(account.id);
    if (plan && typeof plan === "object") {
      plansByID.value = { ...plansByID.value, [account.id]: plan };
      persistAccountStatus();
    }
  } catch {
    /* non-fatal background plan refresh */
  } finally {
    const next = { ...planLoadingIDs.value };
    delete next[account.id];
    planLoadingIDs.value = next;
  }
}

async function refreshAllAccountPlans(force = false) {
  const list = Array.isArray(accounts.value) ? accounts.value : [];
  if (list.length === 0 || refreshingAllPlans.value) return;
  refreshingAllPlans.value = true;
  try {
    const targets = force
      ? list
      : list.filter((item) => item?.id && !plansByID.value[item.id] && !isAccountLocked(item));
    await Promise.allSettled(targets.map((item) => refreshAccountPlan(item)));
  } finally {
    refreshingAllPlans.value = false;
  }
}

async function runHealthCheck(account) {
  const target = account?.id ? account : accounts.value.find((item) => item.isCurrent);
  if (!target?.id || checkingAccountID.value) return;
  checkingAccountID.value = target.id;
  try {
    const result = await checkCursorAccountHealth(target.id);
    const id = result?.accountId || target.id;
    healthByID.value = { ...healthByID.value, [id]: result };
    if (result?.plan) plansByID.value = { ...plansByID.value, [id]: result.plan };
    persistAccountStatus();
  } catch (error) {
    healthByID.value = {
      ...healthByID.value,
      [target.id]: {
        state: "error",
        message: toUserErrorWithCause(error) || "Health check failed.",
        checkedAtUnixMs: Date.now(),
        accountId: target.id,
      },
    };
  } finally {
    if (checkingAccountID.value === target.id) checkingAccountID.value = "";
  }
}

function openControlCenter() {
  void router.push({ path: "/control-center", query: { tab: "accounts" } });
}

async function showActionError(title, error) {
  await showModal({
    title,
    content: toUserErrorWithCause(error) || toUserError(error),
    showCancel: false,
    confirmText: "OK",
  });
}

async function reloadAccounts() {
  const listed = await listCursorAccounts();
  accounts.value = Array.isArray(listed) ? listed : [];
  restoreAccountStatus(accounts.value);
}

async function afterAccountChanged() {
  await Promise.allSettled([reloadAccounts(), reloadUserConfig()]);
  restoreAccountStatus(accounts.value);
  notifyAccountSync();
  void refreshAllAccountPlans(false);
}

function clearImportDialog() {
  importValue.value = "";
  importDialog.value = null;
}

function clearTagDialog() {
  tagValue.value = "";
  tagDialog.value = null;
}

function clearSpendLimitDialog() {
  spendLimitDialog.value = null;
  spendLimitLoading.value = false;
  spendLimitSaving.value = false;
  spendLimitSnapshot.value = null;
  spendLimitError.value = "";
}

function applySpendLimitState(snapshot, cachedPlan = null) {
  const mode = snapshot?.mode || onDemandLimitMode(cachedPlan) || "disabled";
  spendLimitMode.value = mode;
  if (mode === "fixed") {
    const dollars = Number(snapshot?.hardLimitDollars || cachedPlan?.onDemandHardLimitDollars || Math.round((Number(cachedPlan?.onDemandLimitCents) || 0) / 100));
    spendLimitAmount.value = dollars > 0 ? String(dollars) : "50";
  } else if (!spendLimitAmount.value || Number(spendLimitAmount.value) <= 0) {
    spendLimitAmount.value = "50";
  }
}

async function openSpendLimitDialog(account) {
  if (!account?.id || busy.value) return;
  spendLimitDialog.value = account;
  spendLimitError.value = "";
  spendLimitSaving.value = false;
  spendLimitSnapshot.value = null;
  const cachedPlan = planFor(account);
  applySpendLimitState(null, cachedPlan);
  spendLimitLoading.value = true;
  try {
    const live = await getCursorAccountSpendLimit(account.id);
    if (spendLimitDialog.value?.id !== account.id) return;
    spendLimitSnapshot.value = live;
    applySpendLimitState(live, cachedPlan);
  } catch (error) {
    if (spendLimitDialog.value?.id !== account.id) return;
    spendLimitError.value = toUserErrorWithCause(error) || toUserError(error) || "Could not load the current on-demand spend limit.";
  } finally {
    if (spendLimitDialog.value?.id === account.id) {
      spendLimitLoading.value = false;
    }
  }
}

function selectSpendLimitMode(mode) {
  spendLimitMode.value = mode;
  spendLimitError.value = "";
  if (mode === "fixed" && (!spendLimitAmount.value || Number(spendLimitAmount.value) <= 0)) {
    spendLimitAmount.value = "50";
  }
}

function applySpendLimitPreset(dollars) {
  spendLimitMode.value = "fixed";
  spendLimitAmount.value = String(dollars);
  spendLimitError.value = "";
}

const spendLimitCurrentSummary = computed(() => {
  const snap = spendLimitSnapshot.value;
  if (snap?.mode) return formatSpendLimitSummary(snap.mode, snap.hardLimitDollars);
  const cached = planFor(spendLimitDialog.value);
  const mode = onDemandLimitMode(cached);
  if (mode) {
    const dollars = cached?.onDemandHardLimitDollars || Math.round((Number(cached?.onDemandLimitCents) || 0) / 100);
    return formatSpendLimitSummary(mode, dollars);
  }
  return "Unknown";
});

const spendLimitReadOnlyReason = computed(() => {
  const snap = spendLimitSnapshot.value;
  if (snap && snap.editable === false && snap.readOnlyReason) return snap.readOnlyReason;
  const cached = planFor(spendLimitDialog.value);
  if (cached && cached.onDemandLimitKnown && cached.onDemandEditable === false && cached.onDemandReadOnlyReason) {
    return cached.onDemandReadOnlyReason;
  }
  return "";
});

async function confirmSpendLimit() {
  const account = spendLimitDialog.value;
  if (!account?.id || spendLimitSaving.value || spendLimitLoading.value) return;

  const validated = validateSpendLimitInput(spendLimitMode.value, spendLimitAmount.value);
  if (!validated.ok) {
    spendLimitError.value = validated.errorMessage;
    return;
  }

  const prev = spendLimitSnapshot.value;
  const targetLabel = formatSpendLimitSummary(validated.mode, validated.hardLimitDollars);
  const isUnlimited = validated.mode === "unlimited";
  const isRaisingOrEnabling = isUnlimited
    || (validated.mode === "fixed" && (!prev || prev.mode === "disabled" || (prev.mode === "fixed" && validated.hardLimitDollars > Number(prev.hardLimitDollars || 0))));

  if (isUnlimited) {
    const confirmed = await showModal({
      title: "Set unlimited on-demand spending?",
      content: `Remove the monthly on-demand spend cap for ${account.email || "this account"}? Cursor will bill all usage beyond your included plan with no upper limit.`,
      confirmText: "Set Unlimited",
      cancelText: "Cancel",
    });
    if (!confirmed) return;
  } else if (isRaisingOrEnabling) {
    const confirmed = await showModal({
      title: `Set on-demand limit to ${targetLabel}?`,
      content: `Update the monthly on-demand spend limit for ${account.email || "this account"} to ${targetLabel}. Usage beyond your included allowance will be billed by Cursor up to this limit.`,
      confirmText: `Set ${targetLabel}`,
      cancelText: "Cancel",
    });
    if (!confirmed) return;
  }

  spendLimitSaving.value = true;
  spendLimitError.value = "";
  try {
    const updated = await setCursorAccountSpendLimit({
      accountId: account.id,
      mode: validated.mode,
      hardLimitDollars: validated.hardLimitDollars,
      teamId: Number(prev?.teamId || 0),
      personalScope: Boolean(prev?.personalScope),
    });
    const savedLabel = formatSpendLimitSummary(updated?.mode || validated.mode, updated?.hardLimitDollars ?? validated.hardLimitDollars);
    const cleared = Number(updated?.clearedQuotaCooldowns || 0);
    clearSpendLimitDialog();
    await refreshAccountPlan(account);
    message.success(
      cleared > 0
        ? `On-demand limit updated to ${savedLabel} (cleared ${cleared} model cooldown(s)).`
        : `On-demand limit updated to ${savedLabel}.`,
    );
  } catch (error) {
    spendLimitError.value = toUserErrorWithCause(error) || toUserError(error) || "Failed to update on-demand spend limit.";
  } finally {
    spendLimitSaving.value = false;
  }
}

function clearApiKeysDialog() {
  apiKeysDialog.value = null;
  apiKeysLoading.value = false;
  apiKeys.value = [];
  apiKeysError.value = "";
  apiKeyShowCreate.value = false;
  apiKeyNameMode.value = "random";
  apiKeyCustomName.value = "";
  apiKeyCreating.value = false;
  createdApiKey.value = null;
  createdApiKeyCopied.value = false;
}

async function openApiKeysDialog(account) {
  if (!account?.id || busy.value) return;
  apiKeysDialog.value = account;
  apiKeysError.value = "";
  apiKeyShowCreate.value = false;
  apiKeyNameMode.value = "random";
  apiKeyCustomName.value = "";
  createdApiKey.value = null;
  createdApiKeyCopied.value = false;
  await loadApiKeys(account);
}

async function loadApiKeys(account) {
  if (!account?.id) return;
  apiKeysLoading.value = true;
  apiKeysError.value = "";
  try {
    const result = await listCursorAccountApiKeys(account.id);
    if (apiKeysDialog.value?.id !== account.id) return;
    apiKeys.value = Array.isArray(result?.keys) ? result.keys : [];
  } catch (error) {
    if (apiKeysDialog.value?.id !== account.id) return;
    apiKeysError.value = toUserErrorWithCause(error) || toUserError(error) || "Could not load API keys for this account.";
  } finally {
    if (apiKeysDialog.value?.id === account.id) apiKeysLoading.value = false;
  }
}

function toggleCreateApiKey() {
  apiKeyShowCreate.value = !apiKeyShowCreate.value;
  apiKeysError.value = "";
  if (!apiKeyShowCreate.value) apiKeyCustomName.value = "";
}

// Mints a key only when the user explicitly confirms here — never automatically.
async function confirmCreateApiKey() {
  const account = apiKeysDialog.value;
  if (!account?.id || apiKeyCreating.value) return;
  const mode = apiKeyNameMode.value === "custom" ? "custom" : "random";
  if (mode === "custom" && !apiKeyCustomName.value.trim()) {
    apiKeysError.value = "Enter a name for the API key, or switch to a random name.";
    return;
  }
  apiKeyCreating.value = true;
  apiKeysError.value = "";
  try {
    const result = await createCursorAccountApiKey({
      accountId: account.id,
      nameMode: mode,
      name: mode === "custom" ? apiKeyCustomName.value.trim() : "",
    });
    if (apiKeysDialog.value?.id !== account.id) return;
    createdApiKey.value = result || null;
    createdApiKeyCopied.value = false;
    apiKeyShowCreate.value = false;
    apiKeyCustomName.value = "";
    message.success(`API key "${result?.name || "new key"}" created`);
    await loadApiKeys(account);
  } catch (error) {
    if (apiKeysDialog.value?.id !== account.id) return;
    apiKeysError.value = toUserErrorWithCause(error) || toUserError(error) || "Failed to create the API key.";
  } finally {
    if (apiKeysDialog.value?.id === account.id) apiKeyCreating.value = false;
  }
}

function copyCreatedApiKey() {
  const secret = createdApiKey.value?.apiKey;
  if (!secret) return;
  copyTextToClipboard(secret);
  createdApiKeyCopied.value = true;
  message.success("API key copied to clipboard");
}

function dismissCreatedApiKey() {
  createdApiKey.value = null;
  createdApiKeyCopied.value = false;
}

function formatApiKeyDate(ms) {
  const value = Number(ms);
  if (!value) return "";
  try {
    return new Date(value).toLocaleDateString();
  } catch {
    return "";
  }
}

async function handleOAuthLogin() {
  if (busy.value) return;
  busy.value = true;
  try {
    const session = await beginCursorAccountLogin();
    loginSessionId.value = String(session?.sessionId || "");
    if (!loginSessionId.value) return;
    await pollLogin(loginSessionId.value);
  } catch (error) {
    await showActionError("Cursor sign-in failed", error);
  } finally {
    busy.value = false;
  }
}

async function pollLogin(sessionId) {
  for (let attempt = 0; attempt < 40; attempt += 1) {
    const status = await getCursorAccountLoginStatus(sessionId);
    if (status?.state === "signed_in") {
      loginSessionId.value = "";
      const synced = await syncCursorAccountModels(null, "merge");
      await afterAccountChanged();
      if (synced.added > 0 || synced.updated > 0) {
        message.success(`Cursor account added (${synced.total || synced.added} models synced)`);
      } else {
        message.success("Cursor account added");
      }
      return;
    }
    if (status?.state === "error") {
      loginSessionId.value = "";
      throw new Error(status.error || "Sign-in failed");
    }
    await new Promise((resolve) => {
      loginTimer.value = window.setTimeout(resolve, 400);
    });
  }
  loginSessionId.value = "";
  throw new Error("Sign-in timed out");
}

// Performs atomic backend-native model sync via Go ProxyService.SyncCursorAccountModels,
// completely avoiding WebView2 IPC body truncation on large configs.
async function syncCursorAccountModels(targetAccount = null, mode = "merge") {
  try {
    let accountID = "";
    if (mode === "remove" && !targetAccount?.id) {
      accountID = "*";
    } else if (mode !== "all") {
      const listed = accounts.value.length ? accounts.value : await listCursorAccounts();
      const account = targetAccount?.id
        ? targetAccount
        : (Array.isArray(listed) ? listed : []).find((item) => item?.isCurrent) || null;
      if (!account?.id) {
        return {
          total: 0,
          added: 0,
          updated: 0,
          removed: 0,
          unchanged: false,
          error: "Select or import a Cursor account before syncing models.",
          label: "",
        };
      }
      accountID = account.id;
    }

    const result = await syncCursorAccountModelsRpc({ accountId: accountID, mode });
    return {
      total: result?.total ?? 0,
      added: result?.added ?? 0,
      updated: result?.updated ?? 0,
      removed: result?.removed ?? 0,
      accountsSynced: result?.accountsSynced ?? 1,
      unchanged: Boolean(result?.unchanged),
      error: "",
      label: result?.ownerLabel || result?.email || "",
    };
  } catch (error) {
    return {
      total: 0,
      added: 0,
      updated: 0,
      removed: 0,
      unchanged: false,
      error: toUserErrorWithCause(error),
      label: "",
    };
  }
}

async function handleRemoveSyncedModels() {
  if (busy.value || syncedModelCount.value === 0) return;
  const confirmed = await showModal({
    title: "Remove synced models?",
    content: `Remove all ${syncedModelCount.value} Cursor models from the model list? Saved accounts stay. Sync models again later if you want them back.`,
    confirmText: "Remove",
    cancelText: "Cancel",
  });
  if (!confirmed) return;
  await handleSyncModels(null, "remove");
}

async function handleSyncModels(account = null, mode = "merge") {
  if (busy.value) return;
  busy.value = true;
  syncingAccountID.value = mode === "all" || (mode === "remove" && !account?.id) ? "*" : (account?.id || "");
  try {
    const result = await syncCursorAccountModels(account, mode);
    if (result.error) {
      await showModal({
        title: "Could not sync models",
        content: result.error,
        showCancel: false,
        confirmText: "OK",
      });
      return;
    }
    await afterAccountChanged();
    if (mode === "remove") {
      message.success(result.removed > 0
        ? `Removed ${result.removed} synced model(s).`
        : "No synced models found for this account.");
      return;
    }
    if (mode === "all") {
      if (result.unchanged) {
        message.success(`All ${result.accountsSynced} account(s) are already up to date.`);
      } else {
        message.success(`Synced ${result.accountsSynced} account(s): ${result.added} added, ${result.updated} updated.`);
      }
      return;
    }
    if (mode === "replace") {
      if (account?.id && !account.isCurrent) {
        await setCurrentCursorAccount(account.id).catch(() => {});
        await afterAccountChanged();
      }
      message.success(`Switched Cursor models to ${account?.email || result.label || "this account"} (${result.total} models ready).`);
      return;
    }
    if (result.added > 0) {
      message.success(`Synced ${result.total} model(s) to ${result.label || "this Cursor account"} (${result.added} new).`);
      return;
    }
    if (result.updated > 0 || result.removed > 0) {
      message.success(`Updated ${result.label || "this Cursor account"} (${result.updated} updated).`);
      return;
    }
    message.success(result.unchanged
      ? `${result.label || "This account"} is already up to date (${result.total} models).`
      : "No models were returned for this account.");
  } finally {
    syncingAccountID.value = "";
    busy.value = false;
  }
}

async function handleImport(mode) {
  if (mode === "local_cursor") {
    busy.value = true;
    try {
      const imported = await importCursorAccount({ mode });
      clearCachedAccountHealth(imported?.id);
      const synced = await syncCursorAccountModels(imported, "merge");
      await afterAccountChanged();
      if (synced.total > 0) {
        message.success(`Imported from local Cursor (${synced.total} models synced)`);
      } else {
        message.success("Imported from local Cursor");
        if (synced.error) console.warn("cursor account model sync:", synced.error);
      }
    } catch (error) {
      await showActionError("Import failed", error);
    } finally {
      busy.value = false;
    }
    return;
  }
  importDialog.value = mode;
  importValue.value = "";
}

async function confirmImport() {
  const mode = importDialog.value;
  const value = importValue.value;
  importValue.value = "";
  if (!mode) return;
  busy.value = true;
  try {
    let imported = null;
    if (mode === "token") {
      imported = await importCursorAccount({ mode, token: value });
    } else {
      imported = await importCursorAccount({ mode: "recovery_json", jsonContent: value });
    }
    clearCachedAccountHealth(imported?.id);
    const synced = mode === "recovery_json"
      ? await syncCursorAccountModels(null, "all")
      : await syncCursorAccountModels(imported, "merge");
    clearImportDialog();
    await afterAccountChanged();
    if (synced.total > 0) {
      message.success(`Account imported (${synced.total} models synced)`);
    } else {
      message.success("Account imported");
      if (synced.error) console.warn("cursor account model sync:", synced.error);
    }
  } catch (error) {
    clearImportDialog();
    await showActionError("Import failed", error);
  } finally {
    busy.value = false;
  }
}

async function handleSetCurrent(account) {
  busy.value = true;
  try {
    await setCurrentCursorAccount(account.id);
    await afterAccountChanged();
    message.success(`${account.email || "Account"} is now current (automatic failover active across saved accounts).`);
  } catch (error) {
    await showActionError("Failed to set the current account", error);
  } finally {
    busy.value = false;
  }
}

async function handleSwitch(account) {
  if (busy.value) return;
  const skipBackup = await showModal({
    title: "Back up Cursor state?",
    content: "Backing up state.vscdb lets the switch roll back automatically if it fails, but copying a large state database can take a long time. Skip the backup to switch faster.",
    confirmText: "Skip backup (faster)",
    cancelText: "Back up and switch",
  });
  busy.value = true;
  try {
    const preparation = await prepareCursorClientAccountSwitchWithOptions(account.id, {
      skipBackup: Boolean(skipBackup),
    });
    const backupNote = preparation?.backupSkipped
      ? "No backup will be taken; a failed switch cannot be rolled back automatically."
      : `Will back up ${preparation?.backupFileCount ?? 0} state file(s).`;
    const confirmed = await showModal({
      title: "Switch Cursor account",
      content: `Cursor will close and switch to ${account.email || "the selected account"}. ${backupNote}`,
      confirmText: "Close and switch",
    });
    if (!confirmed) return;
    const result = await executeCursorClientAccountSwitch(preparation.confirmationToken);
    if (result?.state === "succeeded") {
      await afterAccountChanged();
      message.success("Switched and restarted Cursor");
      return;
    }
    await showActionError("Failed to switch Cursor account", result?.errorCode || "Switch failed");
  } catch (error) {
    await showActionError("Failed to switch Cursor account", error);
  } finally {
    busy.value = false;
  }
}

async function handleExport(account) {
  const first = await showModal({
    title: "Export recovery pack",
    content: "This writes the selected account credentials to a local recovery file. Make sure the target device is trusted.",
    confirmText: "Continue",
  });
  if (!first) return;
  const second = await showModal({
    title: "Confirm export again",
    content: "The recovery pack contains login credentials. Export it?",
    confirmText: "Export",
  });
  if (!second) return;
  busy.value = true;
  try {
    const prepared = await prepareCursorAccountRecoveryExport({ accountIds: [account.id] });
    await executeCursorAccountRecoveryExport(prepared.confirmationToken);
    message.success("Recovery pack exported");
  } catch (error) {
    await showActionError("Export failed", error);
  } finally {
    busy.value = false;
  }
}

function openTagDialog(account) {
  tagDialog.value = account;
  tagValue.value = Array.isArray(account.tags) ? account.tags.join(", ") : "";
}

async function confirmTags() {
  const account = tagDialog.value;
  const raw = tagValue.value;
  clearTagDialog();
  if (!account) return;
  const tags = raw.split(/[,，]/).map((item) => item.trim()).filter(Boolean);
  busy.value = true;
  try {
    await updateCursorAccountTags(account.id, tags);
    await reloadAccounts();
  } catch (error) {
    await showActionError("Failed to update tags", error);
  } finally {
    busy.value = false;
  }
}

async function handleDelete(account) {
  if (account.isCurrent) {
    const others = accounts.value.filter((item) => item.id !== account.id);
    if (others.length > 0) {
      const confirmed = await showModal({
        title: "Delete current account",
        content: `After deleting, ${others[0].email || "another account"} will become the current account and inherit its synced Cursor models.`,
        confirmText: "Delete",
      });
      if (!confirmed) return;
      busy.value = true;
      try {
        await deleteCursorAccounts({ accountIds: [account.id], replacementId: others[0].id });
        await afterAccountChanged();
      } catch (error) {
        await showActionError("Delete failed", error);
      } finally {
        busy.value = false;
      }
      return;
    }
    const confirmed = await showModal({
      title: "Delete current account",
      content: "There is no replacement account. Synced Cursor models for this account will also be removed.",
      confirmText: "Clear and delete",
    });
    if (!confirmed) return;
    busy.value = true;
    try {
      await deleteCursorAccounts({ accountIds: [account.id], clearCurrent: true });
      await afterAccountChanged();
    } catch (error) {
      await showActionError("Delete failed", error);
    } finally {
      busy.value = false;
    }
    return;
  }
  const confirmed = await showModal({
    title: "Delete account",
    content: `Delete ${account.email || "this account"}?`,
    confirmText: "Delete",
  });
  if (!confirmed) return;
  busy.value = true;
  try {
    await deleteCursorAccounts({ accountIds: [account.id] });
    await afterAccountChanged();
  } catch (error) {
    await showActionError("Delete failed", error);
  } finally {
    busy.value = false;
  }
}

onMounted(() => {
  void reloadAccounts()
    .then(() => {
      restoreAccountStatus(accounts.value);
      void refreshAllAccountPlans(false);
    })
    .catch((error) => {
      void showActionError("Failed to load accounts", error);
    });
});

onBeforeUnmount(() => {
  if (loginTimer.value) window.clearTimeout(loginTimer.value);
  if (loginSessionId.value) {
    void cancelCursorAccountLogin(loginSessionId.value).catch(() => {});
  }
  clearImportDialog();
  clearTagDialog();
  clearSpendLimitDialog();
});
</script>

<template>
  <Card>
    <div class="flex flex-col gap-3">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0">
          <div class="center-row gap-2">
            <h2 class="text-base font-semibold text-white">Cursor accounts</h2>
            <Tooltip content="Saved accounts share one cursor_account model list. Set as current chooses the account that serves it. If that account runs out of quota for a model, another saved account is used automatically.">
              <span class="icon-[mdi--information-outline] text-[15px] text-[#858585]" />
            </Tooltip>
          </div>
          <p v-if="!isPanel" class="mt-1 text-xs text-[#8a8a8a]">
            Thanks to
            <button type="button" class="text-[#7dd3a0] underline-offset-2 hover:underline" @click="Browser.OpenURL(CURSOR_ACCOUNT_CONTRIBUTOR_URL)">@aike0210</button>
            for sponsoring development
          </p>
          <p v-else class="mt-1 text-xs text-[#8a8a8a]">
            Manages the control-plane identity, synced Cursor models, and automatic multi-account quota failover.
          </p>
          <p class="mt-1 text-xs leading-5 text-[#8f8f8f]">
            Cursor models stay in one <span class="text-[#d4d4d4]">cursor_account</span> list. Set as current chooses which account serves that list. Other saved accounts are used only when the current account runs out of quota. Account credentials are never sent to third-party APIs.
          </p>
        </div>
        <div class="center-row flex-wrap gap-2">
          <Button
            v-if="accounts.length > 0"
            variant="text"
            :disabled="busy || refreshingAllPlans"
            @click="refreshAllAccountPlans(true)"
          >
            <span v-if="refreshingAllPlans" class="icon-[mdi--loading] mr-1 animate-spin text-[13px]" />
            Refresh usage
          </Button>
          <Button
            v-if="syncedModelCount > 0"
            variant="default"
            :disabled="busy"
            @click="handleRemoveSyncedModels"
          >
            <span v-if="syncingAccountID === '*'" class="icon-[mdi--loading] mr-1 animate-spin text-[13px]" />
            Remove models
          </Button>
          <Button v-if="props.showControlCenterLink" variant="text" @click="openControlCenter">Open control center</Button>
          <ActionMenu>
            <template #trigger>
              <Button variant="primary" :disabled="busy || loginWaiting">
                <span v-if="loginWaiting" class="icon-[mdi--loading] animate-spin text-[14px]" />
                {{ loginWaiting ? "Waiting for sign-in…" : "Add account" }}
              </Button>
            </template>
            <template #items="{ close }">
              <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void handleOAuthLogin(); }">
                <span class="icon-[mdi--login]" /> Official OAuth sign-in
              </button>
              <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void handleImport('local_cursor'); }">
                <span class="icon-[mdi--download-outline]" /> Import from local Cursor
              </button>
              <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void handleImport('token'); }">
                <span class="icon-[mdi--key-outline]" /> Import token / API key
              </button>
              <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void handleImport('recovery_json'); }">
                <span class="icon-[mdi--file-restore-outline]" /> Import recovery pack
              </button>
            </template>
          </ActionMenu>
        </div>
      </div>

      <div v-if="busy && !accounts.length" class="rounded-[8px] border border-[#343434] bg-[#252525]/70 px-3 py-6 text-center text-sm text-[#8a8a8a]">
        <span class="icon-[mdi--loading] animate-spin text-[18px]" /> Loading accounts…
      </div>

      <div
        v-else-if="accounts.length === 0"
        class="flex flex-col items-center rounded-[8px] border border-dashed border-[#3f3f3f] bg-[#252525]/40 px-4 py-8 text-center"
      >
        <span class="icon-[mdi--account-plus-outline] text-[36px] text-[#4a4a4a]" aria-hidden="true" />
        <p class="mt-3 text-sm text-[#a3a3a3]">No saved Cursor accounts yet</p>
        <p class="mt-1 max-w-sm text-xs text-[#737373]">Add an OAuth login or crsr_ / grok_ API key to sync official Cursor models and enable automatic multi-account failover.</p>
      </div>

      <div v-else class="flex flex-col gap-2">
        <div
          v-for="account in accounts"
          :key="account.id"
          class="rounded-[8px] border px-3 py-3 transition-colors"
          :class="isChecking(account) || isSyncingAccount(account)
            ? 'border-[#3b6ea5] bg-[#1b2836]/70'
            : account.isCurrent
              ? 'border-[#2f6b49]/60 bg-[#1f3a2c]/40'
              : 'border-[#343434] bg-[#252525]/70'"
        >
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div class="center-row min-w-0 gap-3">
              <div
                class="flex h-10 w-10 shrink-0 items-center justify-center rounded-full border text-sm font-semibold"
                :class="account.isCurrent ? 'border-[#2f6b49] bg-[#1f3a2c] text-[#7dd3a0]' : 'border-[#3f3f3f] bg-[#232323] text-[#d4d4d4]'"
              >
                {{ accountInitial(account.email) }}
              </div>
              <div class="min-w-0">
                <div class="center-row min-w-0 flex-wrap gap-1.5">
                  <span class="truncate text-sm font-medium text-white">{{ account.email || "Unnamed account" }}</span>
                  <span
                    v-if="credentialTypeLabel(account.credentialType)"
                    class="rounded-full border border-[#3f3f3f] bg-[#232323] px-2 py-0.5 text-[10px] text-[#b5b5b5]"
                  >
                    {{ credentialTypeLabel(account.credentialType) }}
                  </span>
                  <span v-if="account.isCurrent" class="rounded-full border border-[#2f6b49] px-2 py-0.5 text-[10px] text-[#7dd3a0]">Current</span>
                  <span
                    v-if="syncedSummaryFor(account)"
                    class="rounded-full border border-[#3b6ea5]/70 bg-[#182738] px-2 py-0.5 text-[10px] text-[#9ec5ef]"
                    :title="`Synced slot: ${syncedSummaryFor(account).slot}`"
                  >
                    {{ syncedSummaryFor(account).count }} models
                  </span>
                  <span
                    v-if="healthFor(account)"
                    class="center-row gap-1 rounded-full border px-2 py-0.5 text-[10px]"
                    :class="healthBadgeClass(healthFor(account))"
                  >
                    <span :class="healthIcon(healthFor(account))" />
                    {{ healthLabel(healthFor(account)) }}
                  </span>
                </div>
                <div v-if="accountHint(account)" class="mt-0.5 text-[11px] text-[#737373]">{{ accountHint(account) }}</div>
                <div v-if="isChecking(account)" class="mt-1.5 flex items-center gap-1.5 text-[11px] text-[#9ec5ef]">
                  <span class="icon-[mdi--loading] animate-spin text-[13px]" />
                  Checking this account…
                </div>
                <div v-else-if="isSyncingAccount(account)" class="mt-1.5 flex items-center gap-1.5 text-[11px] text-[#9ec5ef]">
                  <span class="icon-[mdi--loading] animate-spin text-[13px]" />
                  Syncing Cursor model catalog…
                </div>
                <div v-if="planFor(account)?.planName || planFor(account)?.usageKnown || planFor(account)?.onDemandLimitKnown" class="mt-2 max-w-[440px]">
                  <div class="flex flex-wrap items-center gap-1.5">
                    <span class="rounded-full border border-[#3f3f3f] bg-[#232323] px-2 py-0.5 text-[10px] font-medium text-white">
                      {{ planFor(account).planName || "Cursor" }}
                    </span>
                    <span
                      v-if="subscriptionStatusLabel(planFor(account).subscriptionStatus)"
                      class="rounded-full border px-2 py-0.5 text-[10px]"
                      :class="statusClass(planFor(account).subscriptionStatus)"
                    >
                      {{ subscriptionStatusLabel(planFor(account).subscriptionStatus) }}
                    </span>
                    <span v-if="planFor(account).price" class="text-[10px] text-[#a3a3a3]">{{ planFor(account).price }}</span>
                    <span v-if="bonusUsageCaption(planFor(account))" class="text-[10px] text-[#7dd3a0]" :title="planFor(account).bonusTooltip || ''">
                      · {{ bonusUsageCaption(planFor(account)) }}
                    </span>
                  </div>
                  <template v-if="planFor(account).usageKnown">
                    <div class="mt-1.5 flex items-center justify-between gap-3 text-[11px] text-[#d4d4d4]">
                      <span>{{ usageRemainingCaption(planFor(account)) }}</span>
                      <span class="shrink-0 text-[#8a8a8a]">{{ usedUsagePercent(planFor(account)) }}% used</span>
                    </div>
                    <div class="mt-1 h-1.5 overflow-hidden rounded-full bg-[#343434]" role="meter" :aria-valuenow="usedUsagePercent(planFor(account))" aria-valuemin="0" aria-valuemax="100" :aria-label="usageRemainingCaption(planFor(account))">
                      <div class="h-full rounded-full transition-all" :class="usageBarClass(planFor(account))" :style="{ width: `${usedUsagePercent(planFor(account))}%` }" />
                    </div>
                    <div v-if="planFor(account).hasBucketBreakdown" class="mt-1.5 grid grid-cols-1 gap-1 sm:grid-cols-2">
                      <div
                        v-if="namedBucketUsedPercent(planFor(account)) != null"
                        class="rounded bg-black/25 px-2 py-1 text-[10px] text-[#d4d4d4]"
                        title="Consumed by explicitly selected named models (Claude, Gemini, GPT, etc.)"
                      >
                        <div class="truncate">{{ namedBucketCaption(planFor(account)) }}</div>
                        <div class="mt-1 h-1 overflow-hidden rounded-full bg-[#343434]">
                          <div class="h-full rounded-full transition-all" :class="usedBarClass(namedBucketUsedPercent(planFor(account)))" :style="{ width: `${namedBucketUsedPercent(planFor(account))}%` }" />
                        </div>
                      </div>
                      <div
                        v-if="autoBucketUsedPercent(planFor(account)) != null"
                        class="rounded bg-black/25 px-2 py-1 text-[10px] text-[#d4d4d4]"
                        title="Includes Auto, Cursor Grok 4.5–4.7, and Composer 2.5"
                      >
                        <div class="truncate">{{ autoBucketCaption(planFor(account)) }}</div>
                        <div class="mt-1 h-1 overflow-hidden rounded-full bg-[#343434]">
                          <div class="h-full rounded-full transition-all" :class="usedBarClass(autoBucketUsedPercent(planFor(account)))" :style="{ width: `${autoBucketUsedPercent(planFor(account))}%` }" />
                        </div>
                      </div>
                    </div>
                    <div v-if="renewalLabel(planFor(account)) || onDemandUsageCaption(planFor(account)) || planFor(account).namedDisplayMessage || planFor(account).displayMessage" class="mt-1 flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-[10px] text-[#8a8a8a]">
                      <span v-if="renewalLabel(planFor(account))">{{ renewalLabel(planFor(account)) }}</span>
                      <span v-if="renewalLabel(planFor(account)) && onDemandUsageCaption(planFor(account))">·</span>
                      <span v-if="onDemandUsageCaption(planFor(account))">{{ onDemandUsageCaption(planFor(account)) }}</span>
                      <button
                        v-if="!isAccountLocked(account)"
                        type="button"
                        class="cursor-pointer text-[10px] text-[#7dd3a0] underline-offset-2 hover:underline"
                        :disabled="busy"
                        @click="openSpendLimitDialog(account)"
                      >
                        Edit limit
                      </button>
                      <span v-if="(renewalLabel(planFor(account)) || onDemandUsageCaption(planFor(account))) && (planFor(account).namedDisplayMessage || planFor(account).displayMessage)">·</span>
                      <span v-if="planFor(account).namedDisplayMessage || planFor(account).displayMessage">{{ planFor(account).namedDisplayMessage || planFor(account).displayMessage }}</span>
                    </div>
                  </template>
                  <div v-else class="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-[#8a8a8a]">
                    <span>{{ onDemandUsageCaption(planFor(account)) || planFor(account).message || "Included usage was not reported for this key." }}</span>
                    <button
                      v-if="!isAccountLocked(account)"
                      type="button"
                      class="cursor-pointer text-[10px] text-[#7dd3a0] underline-offset-2 hover:underline"
                      :disabled="busy"
                      @click="openSpendLimitDialog(account)"
                    >
                      Edit limit
                    </button>
                  </div>
                </div>
                <div v-else-if="isPlanLoading(account)" class="mt-1.5 flex items-center gap-1.5 text-[11px] text-[#8a8a8a]">
                  <span class="icon-[mdi--loading] animate-spin text-[13px]" />
                  Loading plan & usage…
                </div>
                <div v-else-if="planFor(account)?.message" class="mt-1.5 text-[11px] text-[#8a8a8a]">
                  {{ planFor(account).message }}
                </div>
                <div
                  v-if="planFor(account)?.exhaustedModels?.length"
                  class="mt-1.5 flex flex-wrap items-center gap-2 rounded-[6px] border border-[#6b4f1a] bg-[#302512] px-2 py-1 text-[11px] text-[#f2c66d]"
                >
                  <span>Quota cooling down for: {{ planFor(account).exhaustedModels.join(", ") }}</span>
                  <button
                    type="button"
                    class="cursor-pointer text-[11px] text-[#7dd3a0] underline underline-offset-2 hover:text-white"
                    :disabled="busy"
                    @click="handleClearQuotaCooldown(account)"
                  >
                    Clear cooldown
                  </button>
                </div>
                <div
                  v-if="healthFor(account)"
                  class="mt-1.5 rounded-[6px] px-2 py-1.5 text-[11px]"
                  :class="healthPanelClass(healthFor(account))"
                >
                  <div class="font-medium">{{ healthFor(account).message }}</div>
                  <div v-if="healthFor(account).email" class="mt-0.5 opacity-80">
                    {{ healthFor(account).email }}
                    <span v-if="healthFor(account).model">· probed {{ healthFor(account).model }}</span>
                  </div>
                </div>
                <div
                  v-if="isAccountLocked(account)"
                  class="mt-1.5 rounded-[6px] border border-[#6b4f1a] bg-[#302512] px-2 py-1.5 text-[11px] text-[#f2c66d]"
                >
                  <div class="flex items-center gap-1.5 font-medium">
                    <span class="icon-[mdi--lock-clock] text-[13px]" />
                    Temporarily locked by Cursor
                  </div>
                  <div class="mt-0.5 text-[#c9a86a]">
                    Too many devices used this account in 24 hours. Retrying now will fail.
                    <span class="font-medium">{{ formatLockRemaining(account) }}</span> remaining.
                  </div>
                  <button
                    type="button"
                    class="mt-1 cursor-pointer text-[11px] text-[#7dd3a0] underline underline-offset-2 hover:text-white"
                    :disabled="busy"
                    @click="handleForceUnlock(account)"
                  >
                    Try anyway (may fail)
                  </button>
                </div>
                <div v-if="account.tags?.length" class="mt-2 flex flex-wrap gap-1">
                  <span v-for="tag in account.tags" :key="tag" class="rounded-full border border-[#3f3f3f] px-2 py-0.5 text-[10px] text-[#a3a3a3]">{{ tag }}</span>
                </div>
              </div>
            </div>

            <div class="center-row flex-wrap justify-end gap-1.5">
              <Button
                variant="default"
                :disabled="busy"
                @click="handleSyncModels(account, 'merge')"
              >
                <span v-if="isSyncingAccount(account)" class="icon-[mdi--loading] mr-1 animate-spin text-[12px]" />
                <span v-else class="icon-[mdi--sync] mr-1 text-[13px]" />
                Sync models
              </Button>
              <Button
                variant="default"
                :disabled="busy || checkingAccountID !== ''"
                :title="checkingAccountID && !isChecking(account) ? 'Another account is being checked' : ''"
                @click="runHealthCheck(account)"
              >
                <span v-if="isChecking(account)" class="icon-[mdi--loading] mr-1 animate-spin text-[12px]" />
                {{ isChecking(account) ? "Checking…" : "Check status" }}
              </Button>
              <Button variant="default" :disabled="busy || account.isCurrent" @click="handleSetCurrent(account)">
                Set as current
              </Button>
              <Button
                variant="primary"
                :disabled="busy"
                @click="handleSwitch(account)"
              >
                Switch to Cursor
              </Button>
              <ActionMenu>
                <template #trigger>
                  <Button variant="text" :disabled="busy">More</Button>
                </template>
                <template #items="{ close }">
                  <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void handleSyncModels(account, 'replace'); }">
                    <span class="icon-[mdi--swap-horizontal]" /> Sync & use exclusively
                  </button>
                  <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void refreshAccountPlan(account); }">
                    <span class="icon-[mdi--chart-box-outline]" /> Refresh plan & usage
                  </button>
                  <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void openSpendLimitDialog(account); }">
                    <span class="icon-[mdi--cash-fast]" /> On-demand spend limit
                  </button>
                  <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void openApiKeysDialog(account); }">
                    <span class="icon-[mdi--key-variant]" /> API keys
                  </button>
                  <button
                    v-if="syncedSummaryFor(account)"
                    type="button"
                    class="menu-item"
                    role="menuitem"
                    @click="() => { close(); void handleSyncModels(account, 'remove'); }"
                  >
                    <span class="icon-[mdi--playlist-remove]" /> Remove synced models
                  </button>
                  <button type="button" class="menu-item" role="menuitem" @click="() => { close(); openTagDialog(account); }">
                    <span class="icon-[mdi--tag-outline]" /> Edit tags
                  </button>
                  <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void handleExport(account); }">
                    <span class="icon-[mdi--export]" /> Export recovery pack
                  </button>
                  <button type="button" class="menu-item text-[#fca5a5]" role="menuitem" @click="() => { close(); void handleDelete(account); }">
                    <span class="icon-[mdi--delete-outline]" /> Delete account
                  </button>
                </template>
              </ActionMenu>
            </div>
          </div>
        </div>
      </div>
    </div>
  </Card>

  <Teleport to="body">
    <div
      v-if="importDialog"
      class="fixed inset-0 z-[100000] flex items-center justify-center bg-black/50 p-4"
      @click.self="clearImportDialog"
    >
      <div class="w-full max-w-[420px] rounded-[8px] border border-[#343434] bg-[#292929] p-5" role="dialog" aria-modal="true" :aria-label="importDialog === 'token' ? 'Import token' : 'Import recovery pack'">
        <h3 class="mb-3 text-base font-medium text-white">{{ importDialog === "token" ? "Import token" : "Import recovery pack" }}</h3>
        <p class="mb-3 text-sm text-[#a3a3a3]">
          {{ importDialog === "token" ? "Paste a Cursor access token, or a crsr_ / grok_ user API key from the dashboard (it is exchanged for an IDE session and renewed automatically). Cleared from memory immediately after submitting." : "Paste the recovery pack JSON. Cleared from memory immediately after submitting." }}
        </p>
        <textarea
          v-model="importValue"
          class="mb-4 h-28 w-full rounded-[6px] border border-[#3f3f3f] bg-[#232323] p-2 text-sm text-[#e5e5e5] outline-none focus:border-[#10AD5D]"
          :placeholder="importDialog === 'token' ? 'eyJ..., crsr_..., or grok_...' : '{ }'"
          autocomplete="off"
        />
        <div class="flex justify-end gap-2">
          <Button variant="default" @click="clearImportDialog">Cancel</Button>
          <Button variant="primary" :disabled="busy || !importValue.trim()" @click="confirmImport">Import</Button>
        </div>
      </div>
    </div>
    <div
      v-if="tagDialog"
      class="fixed inset-0 z-[100000] flex items-center justify-center bg-black/50 p-4"
      @click.self="clearTagDialog"
    >
      <div class="w-full max-w-[380px] rounded-[8px] border border-[#343434] bg-[#292929] p-5" role="dialog" aria-modal="true" aria-label="Edit tags">
        <h3 class="mb-3 text-base font-medium text-white">Edit tags</h3>
        <input
          v-model="tagValue"
          class="mb-4 h-9 w-full rounded-[6px] border border-[#3f3f3f] bg-[#232323] px-3 text-sm text-[#e5e5e5] outline-none focus:border-[#10AD5D]"
          placeholder="Comma separated"
        />
        <div class="flex justify-end gap-2">
          <Button variant="default" @click="clearTagDialog">Cancel</Button>
          <Button variant="primary" :disabled="busy" @click="confirmTags">Save</Button>
        </div>
      </div>
    </div>
    <div
      v-if="spendLimitDialog"
      class="fixed inset-0 z-[100000] flex items-center justify-center bg-black/50 p-4"
      @click.self="clearSpendLimitDialog"
    >
      <div class="w-full max-w-[440px] rounded-[8px] border border-[#343434] bg-[#292929] p-5" role="dialog" aria-modal="true" aria-label="On-demand spend limit">
        <div class="mb-3 flex items-start justify-between gap-3">
          <div class="min-w-0">
            <h3 class="text-base font-medium text-white">On-demand spend limit</h3>
            <p class="mt-0.5 truncate text-xs text-[#8a8a8a]">
              {{ spendLimitDialog.email || "Unnamed account" }}
              <span v-if="credentialTypeLabel(spendLimitDialog.credentialType)"> · {{ credentialTypeLabel(spendLimitDialog.credentialType) }}</span>
            </p>
          </div>
          <span class="shrink-0 rounded-full border border-[#3f3f3f] bg-[#232323] px-2.5 py-0.5 text-[11px] text-[#d4d4d4]">
            Current: {{ spendLimitCurrentSummary }}
          </span>
        </div>

        <div v-if="spendLimitLoading" class="mb-4 flex items-center gap-2 rounded-[6px] border border-[#343434] bg-[#232323] px-3 py-3 text-xs text-[#a3a3a3]">
          <span class="icon-[mdi--loading] animate-spin text-[14px]" />
          Loading current limit from Cursor…
        </div>

        <template v-else>
          <div
            v-if="spendLimitReadOnlyReason"
            class="mb-3 rounded-[6px] border border-[#6b4f1a] bg-[#302512] px-3 py-2 text-xs text-[#f2c66d]"
          >
            {{ spendLimitReadOnlyReason }}
          </div>

          <div
            v-if="planFor(spendLimitDialog)?.onDemandUsedCents > 0"
            class="mb-3 text-xs text-[#a3a3a3]"
          >
            Spent this cycle: <span class="font-medium text-white">{{ formatPlanCents(planFor(spendLimitDialog).onDemandUsedCents) }}</span>
          </div>

          <div class="mb-3 grid grid-cols-3 gap-1.5">
            <button
              type="button"
              class="cursor-pointer rounded-[6px] border px-2.5 py-2 text-xs font-medium transition-colors"
              :class="spendLimitMode === 'disabled'
                ? 'border-[#2f6b49] bg-[#1f3a2c] text-[#7dd3a0]'
                : 'border-[#3f3f3f] bg-[#232323] text-[#c4c4c4] hover:bg-[#2d2d2d]'"
              :disabled="spendLimitSaving"
              @click="selectSpendLimitMode('disabled')"
            >
              Disabled ($0)
            </button>
            <button
              type="button"
              class="cursor-pointer rounded-[6px] border px-2.5 py-2 text-xs font-medium transition-colors"
              :class="spendLimitMode === 'fixed'
                ? 'border-[#2f6b49] bg-[#1f3a2c] text-[#7dd3a0]'
                : 'border-[#3f3f3f] bg-[#232323] text-[#c4c4c4] hover:bg-[#2d2d2d]'"
              :disabled="spendLimitSaving"
              @click="selectSpendLimitMode('fixed')"
            >
              Fixed limit
            </button>
            <button
              type="button"
              class="cursor-pointer rounded-[6px] border px-2.5 py-2 text-xs font-medium transition-colors"
              :class="spendLimitMode === 'unlimited'
                ? 'border-[#6b4f1a] bg-[#302512] text-[#f2c66d]'
                : 'border-[#3f3f3f] bg-[#232323] text-[#c4c4c4] hover:bg-[#2d2d2d]'"
              :disabled="spendLimitSaving"
              @click="selectSpendLimitMode('unlimited')"
            >
              Unlimited
            </button>
          </div>

          <div v-if="spendLimitMode === 'fixed'" class="mb-3">
            <label class="mb-1.5 block text-xs text-[#a3a3a3]" for="cursor-spend-limit-input">
              Monthly cap (whole USD dollars — raise or lower anytime)
            </label>
            <div class="relative">
              <span class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-[#8a8a8a]">$</span>
              <input
                id="cursor-spend-limit-input"
                v-model="spendLimitAmount"
                type="number"
                min="1"
                step="1"
                class="h-9 w-full rounded-[6px] border border-[#3f3f3f] bg-[#232323] pl-7 pr-3 text-sm text-[#e5e5e5] outline-none focus:border-[#10AD5D]"
                placeholder="50"
                :disabled="spendLimitSaving"
                @keyup.enter="confirmSpendLimit"
              />
            </div>
            <div class="mt-2 flex flex-wrap gap-1.5">
              <button
                v-for="preset in SPEND_LIMIT_PRESETS"
                :key="preset"
                type="button"
                class="cursor-pointer rounded-full border border-[#3f3f3f] bg-[#232323] px-2.5 py-0.5 text-[11px] text-[#d4d4d4] hover:border-[#2f6b49] hover:text-white"
                :disabled="spendLimitSaving"
                @click="applySpendLimitPreset(preset)"
              >
                ${{ preset }}
              </button>
            </div>
          </div>

          <p v-else-if="spendLimitMode === 'disabled'" class="mb-3 text-xs leading-5 text-[#a3a3a3]">
            Turns off on-demand spending (<span class="text-[#d4d4d4]">no_usage_based_allowed</span>). Requests will stop once your included plan allowance is reached.
          </p>

          <div v-else class="mb-3 rounded-[6px] border border-[#6b4f1a] bg-[#302512] px-3 py-2 text-xs leading-5 text-[#f2c66d]">
            Removes the monthly on-demand cap entirely. All usage beyond your included plan will be billed by Cursor with no upper limit.
          </div>

          <div
            v-if="spendLimitError"
            class="mb-3 rounded-[6px] border border-[#4b1d1d] bg-[#2a1313] px-3 py-2 text-xs text-[#fca5a5]"
          >
            {{ spendLimitError }}
          </div>
        </template>

        <div class="flex justify-end gap-2">
          <Button variant="default" :disabled="spendLimitSaving" @click="clearSpendLimitDialog">Cancel</Button>
          <Button
            variant="primary"
            :disabled="spendLimitLoading || spendLimitSaving"
            @click="confirmSpendLimit"
          >
            <span v-if="spendLimitSaving" class="icon-[mdi--loading] mr-1 animate-spin text-[12px]" />
            {{ spendLimitSaving ? "Saving…" : "Save limit" }}
          </Button>
        </div>
      </div>
    </div>
    <div
      v-if="apiKeysDialog"
      class="fixed inset-0 z-[100000] flex items-center justify-center bg-black/50 p-4"
      @click.self="clearApiKeysDialog"
    >
      <div class="w-full max-w-[520px] rounded-[8px] border border-[#343434] bg-[#292929] p-5" role="dialog" aria-modal="true" aria-label="API keys">
        <div class="mb-3 flex items-start justify-between gap-3">
          <div class="min-w-0">
            <h3 class="text-base font-medium text-white">API keys</h3>
            <p class="mt-0.5 truncate text-xs text-[#8a8a8a]">
              {{ apiKeysDialog.email || "Unnamed account" }}
              <span v-if="credentialTypeLabel(apiKeysDialog.credentialType)"> · {{ credentialTypeLabel(apiKeysDialog.credentialType) }}</span>
            </p>
          </div>
          <button type="button" class="shrink-0 text-[#8a8a8a] transition-colors hover:text-white" aria-label="Close" @click="clearApiKeysDialog">
            <span class="icon-[mdi--close] text-[18px]" />
          </button>
        </div>

        <div v-if="createdApiKey" class="mb-4 rounded-[6px] border border-[#2f6b49] bg-[#16281e] p-3">
          <div class="mb-1 flex items-center gap-1.5 text-xs font-medium text-[#7dd3a0]">
            <span class="icon-[mdi--check-circle-outline] text-[14px]" />
            New key “{{ createdApiKey.name }}” created
          </div>
          <p class="mb-2 text-[11px] leading-4 text-[#9db8a8]">Copy it now — Cursor only reveals the full secret once.</p>
          <div class="flex items-center gap-2">
            <code class="min-w-0 flex-1 truncate rounded-[6px] border border-[#2f6b49] bg-[#0f1a14] px-2.5 py-1.5 font-mono text-xs text-[#d6f3e2]">{{ createdApiKey.apiKey }}</code>
            <Button variant="primary" @click="copyCreatedApiKey">
              <span class="mr-1 text-[12px]" :class="createdApiKeyCopied ? 'icon-[mdi--check]' : 'icon-[mdi--content-copy]'" />
              {{ createdApiKeyCopied ? "Copied" : "Copy" }}
            </Button>
          </div>
          <div class="mt-2 text-right">
            <button type="button" class="text-[11px] text-[#8a8a8a] transition-colors hover:text-white" @click="dismissCreatedApiKey">Dismiss</button>
          </div>
        </div>

        <div
          v-if="apiKeysError"
          class="mb-3 rounded-[6px] border border-[#4b1d1d] bg-[#2a1313] px-3 py-2 text-xs text-[#fca5a5]"
        >
          {{ apiKeysError }}
        </div>

        <div class="mb-3">
          <div class="mb-1.5 flex items-center justify-between">
            <span class="text-xs font-medium text-[#a3a3a3]">Keys on this account</span>
            <button type="button" class="flex items-center text-[11px] text-[#8a8a8a] transition-colors hover:text-white disabled:opacity-50" :disabled="apiKeysLoading" @click="loadApiKeys(apiKeysDialog)">
              <span class="icon-[mdi--refresh] mr-0.5 text-[11px]" :class="apiKeysLoading ? 'animate-spin' : ''" /> Refresh
            </button>
          </div>

          <div v-if="apiKeysLoading" class="flex items-center gap-2 rounded-[6px] border border-[#343434] bg-[#232323] px-3 py-3 text-xs text-[#a3a3a3]">
            <span class="icon-[mdi--loading] animate-spin text-[14px]" /> Loading keys from Cursor…
          </div>
          <div v-else-if="apiKeys.length === 0" class="rounded-[6px] border border-dashed border-[#3f3f3f] bg-[#232323] px-3 py-4 text-center text-xs text-[#8a8a8a]">
            No API keys on this account yet.
          </div>
          <ul v-else class="max-h-[220px] space-y-1.5 overflow-y-auto pr-0.5">
            <li
              v-for="key in apiKeys"
              :key="key.id || key.maskedKey || key.name"
              class="flex items-center justify-between gap-3 rounded-[6px] border border-[#343434] bg-[#232323] px-3 py-2"
            >
              <div class="min-w-0">
                <div class="truncate text-xs font-medium text-[#e5e5e5]" :title="key.name">{{ key.name || "(unnamed)" }}</div>
                <div class="mt-0.5 flex items-center gap-2 text-[11px] text-[#8a8a8a]">
                  <span class="font-mono">{{ key.maskedKey || "crsr_…" }}</span>
                  <span v-if="formatApiKeyDate(key.createdAtMs)">· {{ formatApiKeyDate(key.createdAtMs) }}</span>
                </div>
              </div>
            </li>
          </ul>
        </div>

        <div class="border-t border-[#343434] pt-3">
          <div v-if="!apiKeyShowCreate" class="flex items-center justify-between gap-3">
            <p class="text-xs text-[#8a8a8a]">Create a new user API key for this account.</p>
            <Button variant="default" :disabled="apiKeysLoading" @click="toggleCreateApiKey">
              <span class="icon-[mdi--plus] mr-1 text-[12px]" /> Create key
            </Button>
          </div>

          <div v-else>
            <div class="mb-2 grid grid-cols-2 gap-1.5">
              <button
                type="button"
                class="cursor-pointer rounded-[6px] border px-2.5 py-2 text-xs font-medium transition-colors"
                :class="apiKeyNameMode === 'random'
                  ? 'border-[#2f6b49] bg-[#1f3a2c] text-[#7dd3a0]'
                  : 'border-[#3f3f3f] bg-[#232323] text-[#c4c4c4] hover:bg-[#2d2d2d]'"
                :disabled="apiKeyCreating"
                @click="apiKeyNameMode = 'random'"
              >
                Random name
              </button>
              <button
                type="button"
                class="cursor-pointer rounded-[6px] border px-2.5 py-2 text-xs font-medium transition-colors"
                :class="apiKeyNameMode === 'custom'
                  ? 'border-[#2f6b49] bg-[#1f3a2c] text-[#7dd3a0]'
                  : 'border-[#3f3f3f] bg-[#232323] text-[#c4c4c4] hover:bg-[#2d2d2d]'"
                :disabled="apiKeyCreating"
                @click="apiKeyNameMode = 'custom'"
              >
                Custom name
              </button>
            </div>

            <div v-if="apiKeyNameMode === 'custom'" class="mb-2">
              <input
                v-model="apiKeyCustomName"
                type="text"
                maxlength="100"
                class="h-9 w-full rounded-[6px] border border-[#3f3f3f] bg-[#232323] px-3 text-sm text-[#e5e5e5] outline-none focus:border-[#10AD5D]"
                placeholder="My API key"
                autocomplete="off"
                :disabled="apiKeyCreating"
                @keyup.enter="confirmCreateApiKey"
              />
            </div>
            <p v-else class="mb-2 text-[11px] leading-4 text-[#8a8a8a]">
              A name is generated to match this account's existing keys, with no device- or user-identifying parts.
            </p>

            <div class="flex justify-end gap-2">
              <Button variant="default" :disabled="apiKeyCreating" @click="toggleCreateApiKey">Cancel</Button>
              <Button
                variant="primary"
                :disabled="apiKeyCreating || (apiKeyNameMode === 'custom' && !apiKeyCustomName.trim())"
                @click="confirmCreateApiKey"
              >
                <span v-if="apiKeyCreating" class="icon-[mdi--loading] mr-1 animate-spin text-[12px]" />
                {{ apiKeyCreating ? "Creating…" : "Create key" }}
              </Button>
            </div>
          </div>
        </div>

        <div class="mt-4 flex justify-end">
          <Button variant="default" @click="clearApiKeysDialog">Close</Button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.menu-item {
  display: flex;
  width: 100%;
  cursor: pointer;
  align-items: center;
  gap: 0.5rem;
  padding: 0.375rem 0.75rem;
  text-align: left;
  font-size: 0.875rem;
  line-height: 1.25rem;
  color: #d4d4d4;
}
.menu-item:hover {
  background: rgb(255 255 255 / 0.05);
}
</style>

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
  deleteCursorAccounts,
  executeCursorAccountRecoveryExport,
  executeCursorClientAccountSwitch,
  fetchCursorAccountModels,
  getCursorAccountLoginStatus,
  importCursorAccount,
  listCursorAccounts,
  prepareCursorAccountRecoveryExport,
  prepareCursorClientAccountSwitchWithOptions,
  setCurrentCursorAccount,
  updateCursorAccountTags,
} from "@/services/clientApi";
import { notifyAccountSync } from "@/utils/accountSync";
import { buildCursorAccountAdapters, cursorAccountOwnerLabel, isSharedCursorAccountAdapter } from "@/utils/cursorAccountModels";
import {
  renewalLabel,
  remainingUsagePercent,
  subscriptionStatusLabel,
  usageBarClass,
  usageRemainingCaption,
} from "@/utils/cursorAccountPlan";
import { appState, removeModelAdaptersWhere, saveModelAdaptersBatch, toUserError } from "@/state/appState";
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
const loginSessionId = ref("");
const loginTimer = ref(0);
const importDialog = ref(null);
const importValue = ref("");
const tagDialog = ref(null);
const tagValue = ref("");

const isPanel = computed(() => props.variant === "panel");

function accountInitial(email) {
  const text = String(email || "?").trim();
  return text.charAt(0).toUpperCase();
}

function accountHint(account) {
  const hint = String(account?.authIdHint || "").trim();
  return hint ? `ID ${hint}` : "";
}

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

// ─── Account health ──────────────────────────────────────────────────────────
// "Control plane current" only says the account is selected. Health says whether
// Cursor actually accepts its requests, which is what users need to know.
const healthByID = ref({});
const plansByID = ref({});
const planLoadingIDs = ref({});
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
  const cache = readAccountStatusCache();
  for (const account of accounts.value || []) {
    if (!account?.id) continue;
    cache[account.id] = {
      health: healthByID.value[account.id] || null,
      plan: plansByID.value[account.id] || null,
    };
  }
  localStorage.setItem(accountStatusCacheKey, JSON.stringify(cache));
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
        message: toUserError(error) || "Health check failed.",
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
    content: toUserError(error),
    showCancel: false,
    confirmText: "OK",
  });
}

async function reloadAccounts() {
  const listed = await listCursorAccounts();
  accounts.value = Array.isArray(listed) ? listed : [];
  restoreAccountStatus(accounts.value);
}

// 账号变更后仅广播刷新事件；余额同步由后端在账号变更钩子中后台执行，
// 前端不再重复触发全量上游查询（此前会触发 2-3 轮重复同步）。
async function afterAccountChanged() {
  await reloadAccounts();
  restoreAccountStatus(accounts.value);
  notifyAccountSync();
}

function clearImportDialog() {
  importValue.value = "";
  importDialog.value = null;
}

function clearTagDialog() {
  tagValue.value = "";
  tagDialog.value = null;
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
      await afterAccountChanged();
      message.success("Cursor account added");
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

// Imports the account's model catalog as cursor_account adapters so the account
// can serve as a BYOK upstream. Returns a result so the caller can report the
// outcome instead of failing silently.
async function syncCursorAccountModels() {
  try {
    const catalog = await fetchCursorAccountModels();
    const listed = accounts.value.length ? accounts.value : await listCursorAccounts();
    const current = (Array.isArray(listed) ? listed : []).find((item) => item?.isCurrent) || null;
    if (!current?.id) {
      return { count: 0, error: "Select a Cursor account before syncing models", label: "" };
    }
    const ownerLabel = cursorAccountOwnerLabel(current, listed, appStateAdapters());
    const adapters = buildCursorAccountAdapters(catalog, {
      label: ownerLabel,
      email: current?.email || "",
      accountID: current?.id || "",
    });
    if (adapters.length === 0) {
      return { count: 0, error: "Cursor returned no models for this account", label: ownerLabel };
    }
    await removeModelAdaptersWhere(isSharedCursorAccountAdapter);
    const keep = new Set(adapters.map((adapter) => adapter.id));
    await removeModelAdaptersWhere((adapter) => (
      String(adapter?.source || "").trim().toLowerCase() === "cursor_account"
      && String(adapter?.cursorAccountID || "").trim() === String(current.id)
      && !keep.has(adapter.id)
    ));
    const result = await saveModelAdaptersBatch(adapters);
    if (!result?.ok) {
      return { count: 0, error: result?.error || "Saving the model list failed", label: ownerLabel };
    }
    const added = result.added ?? 0;
    const updated = result.updated ?? 0;
    return {
      count: added + updated,
      added,
      unchanged: added === 0 && updated === 0,
      error: "",
      label: ownerLabel,
    };
  } catch (error) {
    return { count: 0, added: 0, unchanged: false, error: error?.message || toUserError(error), label: "" };
  }
}

function appStateAdapters() {
  return Array.isArray(appState?.modelAdapters) ? appState.modelAdapters : [];
}

// Populates models for an account that was imported before model sync existed.
async function handleSyncModels() {
  busy.value = true;
  try {
    const result = await syncCursorAccountModels();
    if (result.error) {
      await showModal({
        title: "Could not sync models",
        content: result.error,
        showCancel: false,
        confirmText: "OK",
      });
      return;
    }
    if (result.added > 0) {
      await afterAccountChanged();
      message.success(`Added ${result.added} new model(s) to ${result.label || "this Cursor account"}`);
      return;
    }
    if (result.count > 0) {
      await afterAccountChanged();
      message.success(`Updated ${result.label || "this Cursor account"}`);
      return;
    }
    message.success(result.unchanged
      ? `${result.label || "This account"} is already up to date. Sync again when Cursor adds models.`
      : "No models were returned for this account");
  } finally {
    busy.value = false;
  }
}

async function handleImport(mode) {
  if (mode === "local_cursor") {
    busy.value = true;
    try {
      await importCursorAccount({ mode });
      const synced = await syncCursorAccountModels();
      await afterAccountChanged();
      if (synced.count > 0) {
        message.success(`Imported from local Cursor (${synced.count} models added)`);
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
    if (mode === "token") {
      await importCursorAccount({ mode, token: value });
    } else {
      await importCursorAccount({ mode: "recovery_json", jsonContent: value });
    }
    const synced = await syncCursorAccountModels();
    clearImportDialog();
    await afterAccountChanged();
    if (synced.count > 0) {
      message.success(`Account imported (${synced.count} models added)`);
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
  } catch (error) {
    await showActionError("Failed to set the current account", error);
  } finally {
    busy.value = false;
  }
}

async function handleSwitch(account) {
  if (busy.value) return;
  // Ask about the backup up front: state.vscdb can reach tens of gigabytes and
  // copying it dominates switch time. Skipping it removes automatic rollback.
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
        content: `After deleting, ${others[0].email || "another account"} will become the current account.`,
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
      content: "There is no replacement account. The current account pointer will be cleared.",
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
});
</script>

<template>
  <Card>
    <div class="flex flex-col gap-3">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0">
          <div class="center-row gap-2">
            <h2 class="text-base font-semibold text-white">Cursor accounts</h2>
            <Tooltip content="A separate Cursor identity used for Skills and MCP. Multiple accounts can be stored.">
              <span class="icon-[mdi--information-outline] text-[15px] text-[#858585]" />
            </Tooltip>
          </div>
          <p v-if="!isPanel" class="mt-1 text-xs text-[#8a8a8a]">
            Thanks to
            <button type="button" class="text-[#7dd3a0] underline-offset-2 hover:underline" @click="Browser.OpenURL(CURSOR_ACCOUNT_CONTRIBUTOR_URL)">@aike0210</button>
            for sponsoring development
          </p>
          <p v-else class="mt-1 text-xs text-[#8a8a8a]">
            Manages the control-plane identity and the Cursor client login. Switching refreshes supplier balances automatically.
          </p>
          <p class="mt-1 text-xs leading-5 text-[#8f8f8f]">
            The built-in agent execution channel still needs protocol verification against real Cursor. Account authorization is not used for third-party APIs.
          </p>
        </div>
        <div class="center-row flex-wrap gap-2">
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
        <p class="mt-1 max-w-sm text-xs text-[#737373]">After adding one you can set it as the control-plane account, or write it into the Cursor client login.</p>
      </div>

      <div v-else class="flex flex-col gap-2">
        <div
          v-for="account in accounts"
          :key="account.id"
          class="rounded-[8px] border px-3 py-3 transition-colors"
          :class="isChecking(account)
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
                <div class="center-row min-w-0 flex-wrap gap-2">
                  <span class="truncate text-sm font-medium text-white">{{ account.email || "Unnamed account" }}</span>
                  <span v-if="account.isCurrent" class="rounded-full border border-[#2f6b49] px-2 py-0.5 text-[10px] text-[#7dd3a0]">Control plane current</span>
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
                <div v-if="planFor(account)?.planName || planFor(account)?.usageKnown" class="mt-2 max-w-[420px]">
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
                  </div>
                  <template v-if="planFor(account).usageKnown">
                    <div class="mt-1.5 flex items-center justify-between gap-3 text-[11px] text-[#d4d4d4]">
                      <span>{{ usageRemainingCaption(planFor(account)) }}</span>
                      <span class="shrink-0 text-[#8a8a8a]">{{ remainingUsagePercent(planFor(account)) }}% left</span>
                    </div>
                    <div class="mt-1 h-1.5 overflow-hidden rounded-full bg-[#343434]" role="meter" :aria-valuenow="remainingUsagePercent(planFor(account))" aria-valuemin="0" aria-valuemax="100" :aria-label="usageRemainingCaption(planFor(account))">
                      <div class="h-full rounded-full" :class="usageBarClass(planFor(account))" :style="{ width: `${remainingUsagePercent(planFor(account))}%` }" />
                    </div>
                    <div v-if="renewalLabel(planFor(account)) || planFor(account).displayMessage" class="mt-1 text-[10px] text-[#8a8a8a]">
                      <span v-if="renewalLabel(planFor(account))">{{ renewalLabel(planFor(account)) }}</span>
                      <span v-if="renewalLabel(planFor(account)) && planFor(account).displayMessage"> · </span>
                      <span v-if="planFor(account).displayMessage">{{ planFor(account).displayMessage }}</span>
                    </div>
                  </template>
                  <div v-else class="mt-1 text-[11px] text-[#8a8a8a]">
                    {{ planFor(account).message || "Included usage was not reported for this key." }}
                  </div>
                </div>
                <div v-else-if="isPlanLoading(account)" class="mt-1.5 flex items-center gap-1.5 text-[11px] text-[#8a8a8a]">
                  <span class="icon-[mdi--loading] animate-spin text-[13px]" />
                  Loading plan…
                </div>
                <div v-else-if="planFor(account)?.message" class="mt-1.5 text-[11px] text-[#8a8a8a]">
                  {{ planFor(account).message }}
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
                variant="primary"
                :disabled="busy"
                @click="handleSwitch(account)"
              >
                Switch to Cursor
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
              <ActionMenu>
                <template #trigger>
                  <Button variant="text" :disabled="busy">More</Button>
                </template>
                <template #items="{ close }">
                  <button type="button" class="menu-item" role="menuitem" @click="() => { close(); void handleSyncModels(); }">
                    <span class="icon-[mdi--sync]" /> Sync models
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
          {{ importDialog === "token" ? "Paste a Cursor access token, or a crsr_ user API key from the dashboard (it is exchanged for an IDE session and renewed automatically). Cleared from memory immediately after submitting." : "Paste the recovery pack JSON. Cleared from memory immediately after submitting." }}
        </p>
        <textarea
          v-model="importValue"
          class="mb-4 h-28 w-full rounded-[6px] border border-[#3f3f3f] bg-[#232323] p-2 text-sm text-[#e5e5e5] outline-none focus:border-[#10AD5D]"
          :placeholder="importDialog === 'token' ? 'eyJ... or crsr_...' : '{ }'"
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

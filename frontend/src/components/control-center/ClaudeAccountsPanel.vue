<script setup>
// 控制中心 → Claude 订阅：保存的 Claude 账号，以及这些账号提供的模型。
//
// 凭据只有一个来源：用户在「添加账号」里粘贴，或亲手选择一个文件。这里不会
// 去读取任何路径上的文件。提交之后，本面板拿到的账号信息里没有令牌，只有它的
// 前缀和末四位；令牌作为模型的访问密钥存进配置，和其他 API Key 一样对待。
import Button from "@/components/ui/Button.vue";
import Card from "@/components/ui/Card.vue";
import ControlCenterSection from "@/components/control-center/ControlCenterSection.vue";
import { useMessage } from "@/composables/useMessage";
import { showModal } from "@/composables/useModal";
import { useLocale } from "@/i18n/runtime";
import {
  addClaudeAccountModels,
  fetchClaudeAccountModels,
  importClaudeAccount,
  listClaudeAccounts,
  refreshClaudeAccount,
  removeClaudeAccount,
  removeClaudeAccountModels,
  setClaudeAccountModelFastMode,
} from "@/services/clientApi";
import { appState, reloadUserConfig } from "@/state/appState";
import {
  CLAUDE_CREDENTIALS_MAX_LENGTH,
  claudeAccountInitial,
  claudeAccountName,
  claudeExhaustedWindow,
  claudeFastModeUnavailable,
  claudePlanLabel,
  claudeRemainingLabel,
  claudeRemovalImpact,
  claudeTokenExpiry,
  claudeUsageIsStale,
  claudeUsageWindows,
  formatClaudeDate,
  formatClaudeDateTime,
} from "@/utils/claudeAccounts";
import { toUserError } from "@/utils/errorContract";
import { isMacOS } from "@/utils/isMacOS";
import { isWindows } from "@/utils/isWindows";
import copyTextToClipboard from "copy-text-to-clipboard";
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from "vue";
import { useRouter } from "vue-router";

// 账号或模型有变化时通知控制中心刷新标签上的计数。
const emit = defineEmits(["changed"]);

const DETAILS_UNAVAILABLE_CODE = "claude_subscription_details_unavailable";
const KEYCHAIN_COMMAND = 'security find-generic-password -s "Claude Code-credentials" -w';
const CREDENTIAL_PLACEHOLDER = '{"claudeAiOauth":{"accessToken":"sk-ant-oat01-…"}}';

const message = useMessage();
const router = useRouter();
const { locale } = useLocale();

const accounts = ref([]);
const models = ref([]);
const loading = ref(true);
const loadError = ref("");
// 正在执行的操作，空串表示空闲。同一时间只跑一个会改动数据的操作。
const busy = ref("");
// 定时推进，让「剩余 N 小时」「今天 / 改天」这类文字保持正确。
const now = ref(Date.now());

const credentialDialog = ref(null);
const credentialText = ref("");
const credentialFile = ref(null);
const credentialError = ref("");
const credentialInput = ref(null);
const fileInput = ref(null);
const copiedHint = ref(false);

const modelDialog = ref(false);
const catalog = ref([]);
const catalogLoading = ref(false);
const catalogError = ref("");
const selectedModelIds = ref([]);

let alive = true;
let clockTimer = 0;
let copyTimer = 0;
let loadSeq = 0;

const isBusy = computed(() => busy.value !== "");
const usableAccountCount = computed(() => accounts.value.filter((entry) => entry.account.status === "ok").length);
const anyFastModel = computed(() => models.value.some((model) => model.fastMode));
const fastModeUnavailable = computed(() => claudeFastModeUnavailable(accounts.value.map((entry) => entry.account)));
const selectableCatalog = computed(() => catalog.value.filter((model) => !model.added));
const allSelected = computed(() =>
  selectableCatalog.value.length > 0 && selectedModelIds.value.length === selectableCatalog.value.length);
const credentialsPath = computed(() =>
  isWindows.value ? "%USERPROFILE%\\.claude\\.credentials.json" : "~/.claude/.credentials.json");
const credentialReady = computed(() => Boolean(credentialFile.value) || credentialText.value.trim() !== "");

function applyState(state) {
  accounts.value = Array.isArray(state?.accounts) ? state.accounts : [];
  models.value = Array.isArray(state?.models) ? state.models : [];
}

function errorText(error, fallback) {
  return String(error?.message || fallback);
}

async function load() {
  const seq = ++loadSeq;
  loading.value = true;
  loadError.value = "";
  try {
    const state = await listClaudeAccounts();
    if (seq === loadSeq) applyState(state);
  } catch (error) {
    if (seq === loadSeq) loadError.value = errorText(error, "读取 Claude 账号失败");
  } finally {
    if (seq === loadSeq) loading.value = false;
  }
}

// 模型行变了：让应用里其他页面拿到新的模型列表，并刷新标签计数。
async function afterModelsChanged() {
  try {
    await reloadUserConfig();
  } catch {
    // 模型配置页自己会再读一次配置；这里失败不影响本页已经拿到的状态。
  }
  emit("changed");
}

function accountName(account) {
  return claudeAccountName(account) || "未命名账号";
}

function accountSubtitle(account) {
  const parts = [];
  if (account.email && account.displayName) parts.push(account.displayName);
  if (account.organizationName) parts.push(account.organizationName);
  return parts.join(" · ");
}

function statusLabel(status) {
  if (status === "expired") return "令牌已过期";
  if (status === "rejected") return "令牌已失效";
  return "正常";
}

function statusClass(status) {
  return status === "ok"
    ? "border-[#2f6b49] text-[#7dd3a0]"
    : "border-[#7c3b3b] bg-[#2a1515] text-[#fca5a5]";
}

function statusProblem(status) {
  if (status === "expired") return "令牌已过期，这个账号暂时提供不了模型。请用「更换凭据」提交新的凭据。";
  if (status === "rejected") return "Anthropic 拒绝了这个令牌，这个账号暂时提供不了模型。请用「更换凭据」提交新的凭据。";
  return "";
}

function detailsUnavailable(account) {
  return String(account.detailsError || "").startsWith(DETAILS_UNAVAILABLE_CODE);
}

// 上一次读取账号资料或用量失败的原因；账号本身有问题时由 statusProblem 说明。
function detailsProblem(account) {
  if (account.status !== "ok" || !account.detailsError || detailsUnavailable(account)) return "";
  return toUserError(account.detailsError);
}

function usedLabel(window) {
  // 没到上限就不显示 100%，免得和「已用完」混淆。
  const percent = window.level === "exhausted" ? 100 : Math.min(99, Math.round(window.usedPercent));
  return `已用 ${percent}%`;
}

function resetLabel(window) {
  const time = formatClaudeDateTime(window.resetsAt, locale.value, now.value);
  return time ? `${time} 重置` : "";
}

function usageTextClass(level) {
  if (level === "exhausted") return "text-[#fca5a5]";
  if (level === "warning") return "text-[#fcd34d]";
  return "text-[#a7f3d0]";
}

function usageBarClass(level) {
  if (level === "exhausted") return "bg-[#f87171]";
  if (level === "warning") return "bg-[#fbbf24]";
  return "bg-[#10AD5D]";
}

// 有用量窗口显示为 100% 时的说明。每种情况都是一整句，方便翻译成通顺的话。
//
// 这里只说 Anthropic 报告的数字，不断言后果：实测（2026-11-27）窗口显示 100%
// 时请求仍然被接受，所以只能说「可能会拒绝」。开了额外用量的账号，按 Anthropic
// 自己的说明，超出套餐的部分由额外用量支付。
function exhaustedNote(account) {
  const window = claudeExhaustedWindow(account);
  if (!window) return "";
  const time = formatClaudeDateTime(window.resetsAt, locale.value, now.value);
  if (account.usageCredits?.enabled) {
    return time
      ? `「${window.label}」的用量显示为 100%，${time} 重置。超出套餐的用量由额外用量支付。`
      : `「${window.label}」的用量显示为 100%。超出套餐的用量由额外用量支付。`;
  }
  return time
    ? `「${window.label}」的用量显示为 100%，${time} 重置。Anthropic 可能会因此拒绝这个账号的请求。`
    : `「${window.label}」的用量显示为 100%。Anthropic 可能会因此拒绝这个账号的请求。`;
}

function expiryOf(account) {
  return claudeTokenExpiry(account.expiresAtUnixMs, now.value);
}

function expiryLabel(account) {
  const expiry = expiryOf(account);
  if (!expiry.known) return "未知";
  const date = formatClaudeDate(account.expiresAtUnixMs, locale.value);
  const note = expiry.expired ? "已过期" : claudeRemainingLabel(expiry.remainingMs);
  return [date, note].join(" · ");
}

function expiryClass(account) {
  const expiry = expiryOf(account);
  if (expiry.expired) return "text-[#fca5a5]";
  if (expiry.soon) return "text-[#fcd34d]";
  return "text-[#d4d4d4]";
}

function creditsLabel(credits) {
  if (!credits) return "未知";
  if (!credits.enabled) return "未开启";
  if (credits.usedPercent === null || credits.usedPercent === undefined) return "已开启";
  return `已开启（已用 ${Math.round(credits.usedPercent)}%）`;
}

function usageReadLabel(account) {
  const time = formatClaudeDateTime(account.usageAtUnixMs, locale.value, now.value);
  return time ? `用量更新于 ${time}` : "";
}

function modelCountLabel(count) {
  return `${count} 个模型`;
}

function modelOwnersLabel(model) {
  const count = Array.isArray(model.accountIds) ? model.accountIds.length : 0;
  return count < accounts.value.length ? `仅 ${count} 个账号提供` : "";
}

function modelOwnersTitle(model) {
  const ids = new Set(Array.isArray(model.accountIds) ? model.accountIds : []);
  return accounts.value
    .filter((entry) => ids.has(entry.account.id))
    .map((entry) => String(accountName(entry.account)))
    .join("\n");
}

// ---- 账号 ---------------------------------------------------------------

async function refreshAccount(account, { quiet = false } = {}) {
  if (isBusy.value) return;
  busy.value = `refresh:${account.id}`;
  try {
    applyState(await refreshClaudeAccount(account.id));
    emit("changed");
    if (quiet) return;
    const refreshed = accounts.value.find((entry) => entry.account.id === account.id)?.account;
    const problem = refreshed ? statusProblem(refreshed.status) || detailsProblem(refreshed) : "";
    if (problem) message.error(problem, { duration: 5000 });
    else message.success("已刷新");
  } catch (error) {
    if (!quiet) message.error(errorText(error, "刷新失败"), { duration: 5000 });
  } finally {
    busy.value = "";
  }
}

async function refreshAll() {
  if (isBusy.value) return;
  let failure = "";
  for (const entry of [...accounts.value]) {
    if (!alive) return;
    busy.value = `refresh:${entry.account.id}`;
    try {
      applyState(await refreshClaudeAccount(entry.account.id));
    } catch (error) {
      failure = errorText(error, "刷新失败");
    }
  }
  busy.value = "";
  emit("changed");
  if (failure) message.error(failure, { duration: 5000 });
  else message.success("已刷新");
}

// 打开页面时，把读取时间过久的用量重新读一遍。只碰令牌可用、且读得到用量的
// 账号；失败不打扰用户：账号卡片上会写明原因。
async function refreshStaleAccounts() {
  for (const entry of [...accounts.value]) {
    if (!alive || isBusy.value) return;
    const account = entry.account;
    if (account.status !== "ok" || detailsUnavailable(account) || !claudeUsageIsStale(account)) continue;
    await refreshAccount(account, { quiet: true });
  }
}

async function removeAccount(account) {
  if (isBusy.value) return;
  const name = accountName(account);
  const impact = claudeRemovalImpact(models.value, account.id);
  const lines = [`将移除 ${name}，保存在本机的令牌会一并删除。`];
  if (impact.kept > 0) lines.push(`${impact.kept} 个模型会继续由其他账号提供。`);
  if (impact.removed > 0) lines.push(`${impact.removed} 个只由它提供的模型会从模型列表中移除。`);
  const confirmed = await showModal({
    title: "移除 Claude 账号",
    content: lines.join("\n"),
    confirmText: "移除",
  });
  if (!confirmed || isBusy.value) return;
  busy.value = `remove:${account.id}`;
  try {
    const result = await removeClaudeAccount(account.id);
    applyState(result.state);
    message.success(`已移除 ${name}`);
    await afterModelsChanged();
  } catch (error) {
    message.error(errorText(error, "移除失败"), { duration: 5000 });
  } finally {
    busy.value = "";
  }
}

// ---- 提交凭据 -----------------------------------------------------------

function openCredentialDialog(account = null) {
  credentialDialog.value = { account };
  credentialText.value = "";
  credentialFile.value = null;
  credentialError.value = "";
  void nextTick(() => credentialInput.value?.focus());
}

function closeCredentialDialog() {
  if (busy.value === "import") return;
  credentialDialog.value = null;
  // 凭据不在界面里多留。
  credentialText.value = "";
  credentialFile.value = null;
  credentialError.value = "";
}

function chooseCredentialFile() {
  fileInput.value?.click();
}

async function onCredentialFileSelected(event) {
  const file = event.target.files?.[0];
  event.target.value = "";
  if (!file) return;
  credentialError.value = "";
  if (file.size > CLAUDE_CREDENTIALS_MAX_LENGTH) {
    credentialError.value = String("这个文件太大，不像是 Claude 的凭据文件。");
    return;
  }
  try {
    credentialFile.value = { name: file.name, text: await file.text() };
    credentialText.value = "";
  } catch {
    credentialError.value = String("读取这个文件失败，请改为粘贴它的内容。");
  }
}

function clearCredentialFile() {
  credentialFile.value = null;
  void nextTick(() => credentialInput.value?.focus());
}

async function submitCredential() {
  if (isBusy.value || !credentialReady.value) return;
  const text = credentialFile.value ? credentialFile.value.text : credentialText.value;
  credentialError.value = "";
  busy.value = "import";
  let result;
  try {
    result = await importClaudeAccount(text);
  } catch (error) {
    credentialError.value = errorText(error, "添加账号失败");
    busy.value = "";
    return;
  }
  busy.value = "";
  closeCredentialDialog();
  applyState(result.state);
  const account = accounts.value.find((entry) => entry.account.id === result.accountId)?.account;
  const name = accountName(account || {});
  if (!result.created) message.success(`已更新 ${name} 的凭据`);
  else if (result.updatedModels > 0) message.success(`已添加 ${name}，并让它一起提供 ${result.updatedModels} 个模型`);
  else message.success(`已添加 ${name}`);
  if (result.updatedModels > 0) await afterModelsChanged();
  else emit("changed");
  // 第一个账号加好后还没有模型：直接带用户去选模型。
  if (result.created && models.value.length === 0 && alive) void openModelDialog();
}

function copyKeychainCommand() {
  copyTextToClipboard(KEYCHAIN_COMMAND);
  copiedHint.value = true;
  window.clearTimeout(copyTimer);
  copyTimer = window.setTimeout(() => {
    copiedHint.value = false;
  }, 1500);
}

// ---- 模型 ---------------------------------------------------------------

async function openModelDialog() {
  modelDialog.value = true;
  await loadCatalog();
}

function closeModelDialog() {
  if (busy.value === "add-models") return;
  modelDialog.value = false;
  catalog.value = [];
  selectedModelIds.value = [];
  catalogError.value = "";
}

async function loadCatalog() {
  catalog.value = [];
  selectedModelIds.value = [];
  catalogError.value = "";
  catalogLoading.value = true;
  try {
    const result = await fetchClaudeAccountModels();
    catalog.value = Array.isArray(result?.models) ? result.models : [];
  } catch (error) {
    catalogError.value = errorText(error, "读取模型列表失败");
  } finally {
    catalogLoading.value = false;
  }
}

function toggleCatalogModel(model) {
  if (model.added) return;
  selectedModelIds.value = selectedModelIds.value.includes(model.id)
    ? selectedModelIds.value.filter((id) => id !== model.id)
    : [...selectedModelIds.value, model.id];
}

function toggleAllCatalogModels() {
  selectedModelIds.value = allSelected.value ? [] : selectableCatalog.value.map((model) => model.id);
}

async function submitModels() {
  if (isBusy.value || selectedModelIds.value.length === 0) return;
  const chosen = catalog.value
    .filter((model) => selectedModelIds.value.includes(model.id))
    .map((model) => ({ id: model.id, displayName: model.displayName }));
  busy.value = "add-models";
  try {
    const result = await addClaudeAccountModels(chosen);
    applyState(result.state);
    busy.value = "";
    closeModelDialog();
    message.success(`已添加 ${result.added} 个模型`);
    await afterModelsChanged();
  } catch (error) {
    catalogError.value = errorText(error, "添加模型失败");
  } finally {
    busy.value = "";
  }
}

async function toggleFastMode(model) {
  if (isBusy.value) return;
  busy.value = `fast:${model.id}`;
  try {
    applyState(await setClaudeAccountModelFastMode(model.id, !model.fastMode));
    await afterModelsChanged();
  } catch (error) {
    message.error(errorText(error, "切换 Fast 模式失败"), { duration: 5000 });
  } finally {
    busy.value = "";
  }
}

async function removeModel(model) {
  if (isBusy.value) return;
  const confirmed = await showModal({
    title: "移除模型",
    content: `将把 ${model.displayName} 从模型列表中移除，账号不受影响。之后可以再添加回来。`,
    confirmText: "移除",
  });
  if (!confirmed || isBusy.value) return;
  busy.value = `remove-model:${model.id}`;
  try {
    const result = await removeClaudeAccountModels([model.id]);
    applyState(result.state);
    message.success(`已移除 ${model.displayName}`);
    await afterModelsChanged();
  } catch (error) {
    message.error(errorText(error, "移除失败"), { duration: 5000 });
  } finally {
    busy.value = "";
  }
}

// 模型的其余设置（思考强度、上下文窗口等）在模型编辑页里改。
async function editModel(model) {
  try {
    await reloadUserConfig();
  } catch {
    // 用内存里已有的模型列表继续找。
  }
  const index = appState.modelAdapters.findIndex((adapter) => adapter.id === model.id);
  if (index < 0) {
    message.error("没有找到这个模型，请刷新后重试");
    return;
  }
  await router.push({ path: "/model-editor", query: { index: String(index) } });
}

function openModelConfig() {
  void router.push("/model-config");
}

onMounted(async () => {
  clockTimer = window.setInterval(() => {
    now.value = Date.now();
  }, 30_000);
  await load();
  if (alive && !loadError.value) void refreshStaleAccounts();
});

onBeforeUnmount(() => {
  alive = false;
  window.clearInterval(clockTimer);
  window.clearTimeout(copyTimer);
});
</script>

<template>
  <div
    class="flex min-h-0 flex-1 flex-col gap-4 pb-2"
    data-testid="claude-accounts-panel"
  >
    <div class="flex items-start gap-2.5 rounded-[8px] border border-[#343434] bg-[#252525]/50 px-4 py-3">
      <span
        class="icon-[logos--claude-icon] mt-0.5 shrink-0 text-[15px]"
        aria-hidden="true"
      />
      <p class="text-xs leading-relaxed text-[#a3a3a3]">
        添加 Claude Pro / Max 账号后，这里的模型用量计入订阅额度，请求按 Claude CLI 的方式发给 Anthropic。凭据只在你提交时读取，应用不会自己去找或读取任何凭据文件。
      </p>
    </div>

    <div
      v-if="loading"
      class="rounded-[8px] border border-[#343434] bg-[#252525]/70 px-3 py-8 text-center text-sm text-[#8a8a8a]"
    >
      <span class="icon-[mdi--loading] animate-spin text-[18px]" /> 正在读取 Claude 账号…
    </div>

    <div
      v-else-if="loadError"
      class="flex flex-wrap items-center justify-between gap-3 rounded-[8px] border border-[#4b1d1d] bg-[#2a1313] px-3 py-3 text-xs text-[#fca5a5]"
      role="alert"
    >
      <span>{{ loadError }}</span>
      <Button @click="load">
        重试
      </Button>
    </div>

    <template v-else>
      <!-- 账号 -->
      <Card>
        <ControlCenterSection
          title="Claude 账号"
          description="令牌保存在本机。有多个账号时，它们一起为下面的模型提供额度：新对话轮流使用各个账号，某个账号被限额时自动换下一个。"
          icon="icon-[mdi--account-key-outline]"
        >
          <template #actions>
            <div class="center-row gap-2">
              <Button
                v-if="accounts.length > 0"
                variant="text"
                :disabled="isBusy"
                @click="refreshAll"
              >
                刷新用量
              </Button>
              <Button
                variant="primary"
                :disabled="isBusy"
                @click="openCredentialDialog()"
              >
                <span class="icon-[mdi--plus] text-[14px]" />
                添加账号
              </Button>
            </div>
          </template>

          <div
            v-if="accounts.length === 0"
            class="rounded-[8px] border border-dashed border-[#3f3f3f] bg-[#252525]/40 px-4 py-6"
            data-testid="claude-accounts-empty"
          >
            <div class="flex flex-col items-center text-center">
              <span
                class="icon-[mdi--account-plus-outline] text-[34px] text-[#4a4a4a]"
                aria-hidden="true"
              />
              <p class="mt-2 text-sm text-[#d4d4d4]">
                还没有 Claude 账号
              </p>
              <p class="mt-1 text-xs text-[#737373]">
                三步即可在 Cursor 里用上订阅里的模型。
              </p>
            </div>
            <ol class="mx-auto mt-4 grid max-w-[900px] gap-2 text-xs sm:grid-cols-3">
              <li class="rounded-[6px] border border-[#343434] bg-[#232323] px-3 py-2.5">
                <div class="center-row gap-1.5 font-medium text-[#e5e5e5]">
                  <span
                    class="icon-[mdi--numeric-1-circle-outline] text-[15px] text-[#6ee7a5]"
                    aria-hidden="true"
                  />
                  准备凭据
                </div>
                <p class="mt-1 leading-relaxed text-[#8a8a8a]">
                  在已登录的 Claude CLI 里拿到凭据文件，或者生成一个长期令牌。
                </p>
              </li>
              <li class="rounded-[6px] border border-[#343434] bg-[#232323] px-3 py-2.5">
                <div class="center-row gap-1.5 font-medium text-[#e5e5e5]">
                  <span
                    class="icon-[mdi--numeric-2-circle-outline] text-[15px] text-[#6ee7a5]"
                    aria-hidden="true"
                  />
                  添加账号
                </div>
                <p class="mt-1 leading-relaxed text-[#8a8a8a]">
                  粘贴凭据，或者选择凭据文件。可以添加多个账号。
                </p>
              </li>
              <li class="rounded-[6px] border border-[#343434] bg-[#232323] px-3 py-2.5">
                <div class="center-row gap-1.5 font-medium text-[#e5e5e5]">
                  <span
                    class="icon-[mdi--numeric-3-circle-outline] text-[15px] text-[#6ee7a5]"
                    aria-hidden="true"
                  />
                  添加模型
                </div>
                <p class="mt-1 leading-relaxed text-[#8a8a8a]">
                  选好的模型会出现在 Cursor 的模型列表里。
                </p>
              </li>
            </ol>
          </div>

          <div
            v-else
            class="flex flex-col gap-2"
          >
            <div
              v-for="entry in accounts"
              :key="entry.account.id"
              class="rounded-[8px] border px-3 py-3 transition-colors"
              :class="busy === `refresh:${entry.account.id}`
                ? 'border-[#3b6ea5] bg-[#1b2836]/70'
                : entry.account.status === 'ok'
                  ? 'border-[#343434] bg-[#252525]/70'
                  : 'border-[#5c2b2b] bg-[#2a1515]/40'"
              data-testid="claude-account"
            >
              <div class="flex flex-wrap items-start justify-between gap-3">
                <div class="center-row min-w-0 gap-3">
                  <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-full border border-[#3f3f3f] bg-[#232323] text-sm font-semibold text-[#d4d4d4]">
                    {{ claudeAccountInitial(entry.account) }}
                  </div>
                  <div class="min-w-0">
                    <div class="center-row min-w-0 flex-wrap gap-1.5">
                      <span class="truncate text-sm font-medium text-white">{{ accountName(entry.account) }}</span>
                      <span
                        v-if="claudePlanLabel(entry.account.plan)"
                        class="rounded-full border border-[#3f3f3f] bg-[#232323] px-2 py-0.5 text-[10px] font-medium text-white"
                      >
                        {{ claudePlanLabel(entry.account.plan) }}
                      </span>
                      <span
                        class="rounded-full border px-2 py-0.5 text-[10px]"
                        :class="statusClass(entry.account.status)"
                        data-testid="claude-account-status"
                      >
                        {{ statusLabel(entry.account.status) }}
                      </span>
                      <span class="rounded-full border border-[#3b6ea5]/70 bg-[#182738] px-2 py-0.5 text-[10px] text-[#9ec5ef]">
                        {{ modelCountLabel(entry.modelCount) }}
                      </span>
                    </div>
                    <div
                      v-if="accountSubtitle(entry.account)"
                      class="mt-0.5 truncate text-[11px] text-[#8a8a8a]"
                    >
                      {{ accountSubtitle(entry.account) }}
                    </div>
                  </div>
                </div>

                <div class="center-row flex-wrap justify-end gap-2">
                  <Button
                    :disabled="isBusy"
                    @click="refreshAccount(entry.account)"
                  >
                    <span
                      class="text-[13px]"
                      :class="busy === `refresh:${entry.account.id}` ? 'icon-[mdi--loading] animate-spin' : 'icon-[mdi--refresh]'"
                    />
                    刷新
                  </Button>
                  <Button
                    :variant="entry.account.status === 'ok' ? 'default' : 'primary'"
                    :disabled="isBusy"
                    @click="openCredentialDialog(entry.account)"
                  >
                    更换凭据
                  </Button>
                  <button
                    type="button"
                    class="shrink-0 cursor-pointer whitespace-nowrap px-1 text-sm text-[#a3a3a3] transition-colors duration-150 hover:text-[#fca5a5] disabled:cursor-not-allowed disabled:opacity-55"
                    :disabled="isBusy"
                    @click="removeAccount(entry.account)"
                  >
                    移除
                  </button>
                </div>
              </div>

              <div class="mt-3 flex flex-col gap-2 sm:pl-[52px]">
                <div
                  v-if="statusProblem(entry.account.status)"
                  class="rounded-[6px] border border-[#5c2b2b] bg-[#2a1515] px-2.5 py-2 text-[11px] leading-relaxed text-[#fca5a5]"
                  role="alert"
                >
                  {{ statusProblem(entry.account.status) }}
                </div>

                <div
                  v-if="claudeUsageWindows(entry.account).length > 0"
                  class="grid gap-2 sm:grid-cols-2 xl:grid-cols-3"
                >
                  <div
                    v-for="window in claudeUsageWindows(entry.account)"
                    :key="window.id"
                    class="rounded-[6px] bg-black/25 px-2.5 py-2"
                    data-testid="claude-usage-window"
                  >
                    <div class="flex items-center justify-between gap-2 text-[11px]">
                      <span class="truncate text-[#d4d4d4]">{{ window.label }}</span>
                      <span
                        class="shrink-0 font-medium"
                        :class="usageTextClass(window.level)"
                        style="font-family: var(--font-num)"
                      >
                        {{ usedLabel(window) }}
                      </span>
                    </div>
                    <div
                      class="mt-1.5 h-1.5 overflow-hidden rounded-full bg-[#343434]"
                      role="progressbar"
                      :aria-label="window.label"
                      aria-valuemin="0"
                      aria-valuemax="100"
                      :aria-valuenow="Math.round(window.usedPercent)"
                    >
                      <div
                        class="h-full rounded-full transition-[width] duration-300"
                        :class="usageBarClass(window.level)"
                        :style="{ width: `${window.usedPercent}%` }"
                      />
                    </div>
                    <div class="mt-1 min-h-[14px] text-[10px] text-[#8a8a8a]">
                      {{ resetLabel(window) }}
                    </div>
                  </div>
                </div>

                <div
                  v-if="entry.account.status === 'ok' && exhaustedNote(entry.account)"
                  class="rounded-[6px] border border-[#6b4f1a] bg-[#302512] px-2.5 py-2 text-[11px] leading-relaxed text-[#f2c66d]"
                  data-testid="claude-account-exhausted"
                >
                  {{ exhaustedNote(entry.account) }}
                </div>

                <div
                  v-if="entry.account.status === 'ok' && detailsUnavailable(entry.account)"
                  class="rounded-[6px] border border-[#343434] bg-[#232323] px-2.5 py-2 text-[11px] leading-relaxed text-[#a3a3a3]"
                >
                  这个令牌可以正常使用，但没有读取账号资料和用量的权限，所以这里显示不了这些信息。
                </div>

                <div
                  v-if="detailsProblem(entry.account)"
                  class="rounded-[6px] border border-[#6b4f1a] bg-[#302512] px-2.5 py-2 text-[11px] leading-relaxed text-[#f2c66d]"
                >
                  {{ detailsProblem(entry.account) }}
                </div>

                <dl class="grid gap-x-6 gap-y-1 text-[11px] sm:grid-cols-2">
                  <div class="flex min-w-0 gap-2">
                    <dt class="shrink-0 text-[#737373]">
                      令牌
                    </dt>
                    <dd class="truncate font-mono text-[#d4d4d4]">
                      {{ entry.account.tokenHint }}
                    </dd>
                  </div>
                  <div class="flex min-w-0 gap-2">
                    <dt class="shrink-0 text-[#737373]">
                      有效期至
                    </dt>
                    <dd
                      class="truncate"
                      :class="expiryClass(entry.account)"
                      data-testid="claude-account-expiry"
                    >
                      {{ expiryLabel(entry.account) }}
                    </dd>
                  </div>
                  <div class="flex min-w-0 gap-2">
                    <dt class="shrink-0 text-[#737373]">
                      额外用量
                    </dt>
                    <dd
                      class="truncate text-[#d4d4d4]"
                      :title="entry.account.usageCredits?.disabledReason || ''"
                    >
                      {{ creditsLabel(entry.account.usageCredits) }}
                    </dd>
                  </div>
                  <div
                    v-if="entry.account.rateLimitTier"
                    class="flex min-w-0 gap-2"
                  >
                    <dt class="shrink-0 text-[#737373]">
                      速率档位
                    </dt>
                    <dd class="truncate font-mono text-[#d4d4d4]">
                      {{ entry.account.rateLimitTier }}
                    </dd>
                  </div>
                  <div
                    v-if="entry.account.scopes?.length"
                    class="flex min-w-0 gap-2 sm:col-span-2"
                  >
                    <dt class="shrink-0 text-[#737373]">
                      令牌权限
                    </dt>
                    <dd class="truncate font-mono text-[#d4d4d4]">
                      {{ entry.account.scopes.join("  ") }}
                    </dd>
                  </div>
                </dl>

                <div
                  v-if="expiryOf(entry.account).soon"
                  class="text-[11px] text-[#fcd34d]"
                >
                  令牌快到期了。到期后需要用「更换凭据」重新提交，应用不会自动续期。
                </div>
                <div
                  v-if="usageReadLabel(entry.account)"
                  class="text-[10px] text-[#737373]"
                >
                  {{ usageReadLabel(entry.account) }}
                </div>
              </div>
            </div>
          </div>
        </ControlCenterSection>
      </Card>

      <!-- 模型 -->
      <Card v-if="accounts.length > 0 || models.length > 0">
        <ControlCenterSection
          title="订阅模型"
          description="这些模型会出现在 Cursor 的模型列表里，请求由上面的账号发出。"
          icon="icon-[mdi--brain]"
        >
          <template #actions>
            <div class="center-row gap-2">
              <Button
                variant="text"
                @click="openModelConfig"
              >
                模型配置
              </Button>
              <Button
                variant="primary"
                :disabled="isBusy || usableAccountCount === 0"
                :title="usableAccountCount === 0 ? '需要先有一个令牌可用的账号' : ''"
                @click="openModelDialog"
              >
                <span class="icon-[mdi--plus] text-[14px]" />
                添加模型
              </Button>
            </div>
          </template>

          <div
            v-if="models.length === 0"
            class="rounded-[8px] border border-dashed border-[#3f3f3f] bg-[#252525]/40 px-3 py-6 text-center text-sm text-[#8a8a8a]"
            data-testid="claude-models-empty"
          >
            还没有模型。添加后才会出现在 Cursor 的模型列表里。
          </div>

          <div
            v-else
            class="flex flex-col gap-2"
          >
            <div
              v-if="anyFastModel && fastModeUnavailable"
              class="rounded-[6px] border border-[#6b4f1a] bg-[#302512] px-2.5 py-2 text-[11px] leading-relaxed text-[#f2c66d]"
              data-testid="claude-fast-unavailable"
            >
              Anthropic 只对开启了额外用量的账号提供 Fast 模式。现在可用的账号都没有开启，开了 Fast 的模型会按标准速度回答。
            </div>
            <div class="overflow-hidden rounded-[8px] border border-[#343434]">
              <div
                v-for="model in models"
                :key="model.id"
                class="flex flex-wrap items-center justify-between gap-3 border-b border-[#343434] bg-[#252525]/70 px-3 py-2.5 last:border-b-0"
                data-testid="claude-model"
              >
                <div class="min-w-0">
                  <div class="center-row min-w-0 flex-wrap gap-1.5">
                    <span class="truncate text-sm text-white">{{ model.displayName || model.modelId }}</span>
                    <span
                      v-if="model.disabled"
                      class="rounded-full border border-[#4b3b3b] px-2 py-0.5 text-[10px] text-[#a3a3a3]"
                    >
                      已停用
                    </span>
                  </div>
                  <div class="mt-0.5 flex flex-wrap items-center gap-x-2 text-[11px] text-[#737373]">
                    <span class="font-mono">{{ model.modelId }}</span>
                    <span
                      v-if="modelOwnersLabel(model)"
                      :title="modelOwnersTitle(model)"
                    >· {{ modelOwnersLabel(model) }}</span>
                  </div>
                </div>

                <div class="center-row shrink-0 gap-3">
                  <label
                    class="center-row gap-1.5 text-[11px]"
                    :class="model.fastMode ? 'text-[#6ee7a5]' : 'text-[#8a8a8a]'"
                    title="Fast 模式：让 Anthropic 以更快的速度输出。账号不支持时会自动按标准速度发送。"
                  >
                    <span>Fast</span>
                    <button
                      type="button"
                      role="switch"
                      :aria-checked="model.fastMode ? 'true' : 'false'"
                      :aria-label="`${model.displayName || model.modelId} 的 Fast 模式`"
                      :disabled="isBusy"
                      class="relative inline-flex h-[18px] w-[32px] shrink-0 cursor-pointer rounded-full outline-none transition-colors duration-200 focus-visible:ring-2 focus-visible:ring-[#10AD5D]/35 disabled:cursor-not-allowed disabled:opacity-55"
                      :class="model.fastMode ? 'bg-[#10AD5D]' : 'bg-[rgba(255,255,255,0.22)]'"
                      @click="toggleFastMode(model)"
                    >
                      <span
                        class="absolute left-[2px] top-[2px] h-[14px] w-[14px] rounded-full bg-white shadow-[0_2px_5px_rgba(0,0,0,0.22)] transition-transform duration-200"
                        :class="model.fastMode ? 'translate-x-[14px]' : 'translate-x-0'"
                      />
                    </button>
                  </label>
                  <Button
                    variant="text"
                    @click="editModel(model)"
                  >
                    编辑
                  </Button>
                  <button
                    type="button"
                    class="shrink-0 cursor-pointer whitespace-nowrap text-sm text-[#a3a3a3] transition-colors duration-150 hover:text-[#fca5a5] disabled:cursor-not-allowed disabled:opacity-55"
                    :disabled="isBusy"
                    @click="removeModel(model)"
                  >
                    移除
                  </button>
                </div>
              </div>
            </div>
          </div>
        </ControlCenterSection>
      </Card>
    </template>
  </div>

  <Teleport to="body">
    <!-- 提交凭据 -->
    <div
      v-if="credentialDialog"
      class="fixed inset-0 z-[100000] flex items-center justify-center bg-black/50 p-4"
      @click.self="closeCredentialDialog"
      @keydown.esc="closeCredentialDialog"
    >
      <div
        class="flex max-h-full w-full max-w-[520px] flex-col overflow-y-auto rounded-[8px] border border-[#343434] bg-[#292929] p-5"
        role="dialog"
        aria-modal="true"
        :aria-label="credentialDialog.account ? '更换凭据' : '添加 Claude 账号'"
        data-testid="claude-credential-dialog"
      >
        <h3 class="text-base font-medium text-white">
          {{ credentialDialog.account ? "更换凭据" : "添加 Claude 账号" }}
        </h3>
        <p
          v-if="credentialDialog.account"
          class="mt-2 text-xs leading-relaxed text-[#a3a3a3]"
        >
          {{ `提交 ${accountName(credentialDialog.account)} 的新凭据，旧令牌会被替换。如果凭据属于另一个账号，会作为新账号添加。` }}
        </p>
        <p
          v-else
          class="mt-2 text-xs leading-relaxed text-[#a3a3a3]"
        >
          粘贴凭据，或者选择凭据文件。提交后会先向 Anthropic 确认令牌可用，再保存到本机。
        </p>

        <div class="mt-3 rounded-[6px] border border-[#343434] bg-[#232323] px-3 py-2.5 text-[11px] leading-relaxed text-[#a3a3a3]">
          <div class="font-medium text-[#d4d4d4]">
            可以提交的凭据
          </div>
          <ul class="mt-1 flex list-disc flex-col gap-1 pl-4">
            <li v-if="isMacOS">
              Claude CLI 的登录凭据。macOS 把它存在钥匙串里，在终端运行下面的命令，把输出粘贴到这里：
              <span class="mt-1 flex items-center gap-2">
                <code class="min-w-0 flex-1 truncate rounded bg-black/30 px-1.5 py-0.5 font-mono text-[#d4d4d4]">{{ KEYCHAIN_COMMAND }}</code>
                <button
                  type="button"
                  class="shrink-0 cursor-pointer text-[#7dd3a0] underline-offset-2 hover:underline"
                  @click="copyKeychainCommand"
                >
                  {{ copiedHint ? "已复制" : "拷贝" }}
                </button>
              </span>
            </li>
            <li v-else>
              Claude CLI 的凭据文件，能显示账号资料和用量。文件位置：
              <code class="rounded bg-black/30 px-1.5 py-0.5 font-mono text-[#d4d4d4]">{{ credentialsPath }}</code>
            </li>
            <li>
              <code class="rounded bg-black/30 px-1.5 py-0.5 font-mono text-[#d4d4d4]">claude setup-token</code>
              生成的长期令牌，一年有效。它只能用来发请求，所以读不到账号资料和用量。
            </li>
          </ul>
          <p class="mt-1.5">
            凭据文件里的令牌到期后需要重新提交，应用不会自动续期。
          </p>
        </div>

        <div
          v-if="credentialFile"
          class="mt-3 flex items-center justify-between gap-2 rounded-[6px] border border-[#2f6b49] bg-[#1f3a2c]/50 px-3 py-2 text-xs text-[#d4d4d4]"
          data-testid="claude-credential-file"
        >
          <span class="center-row min-w-0 gap-1.5">
            <span
              class="icon-[mdi--file-check-outline] shrink-0 text-[15px] text-[#6ee7a5]"
              aria-hidden="true"
            />
            <span class="truncate">{{ `已选择文件 ${credentialFile.name}` }}</span>
          </span>
          <button
            type="button"
            class="shrink-0 cursor-pointer text-[#a3a3a3] hover:text-white"
            :disabled="busy === 'import'"
            @click="clearCredentialFile"
          >
            改为粘贴
          </button>
        </div>
        <textarea
          v-else
          ref="credentialInput"
          v-model="credentialText"
          rows="5"
          spellcheck="false"
          autocomplete="off"
          aria-label="凭据内容"
          :placeholder="CREDENTIAL_PLACEHOLDER"
          class="mt-3 min-h-[104px] w-full resize-none rounded-[6px] border border-[#3f3f3f] bg-[#232323] px-3 py-2 font-mono text-xs text-[#e5e5e5] outline-none focus:border-[#10AD5D]"
        />

        <div
          v-if="credentialError"
          class="mt-2 rounded-[6px] border border-[#4b1d1d] bg-[#2a1313] px-2.5 py-2 text-xs leading-relaxed text-[#fca5a5]"
          role="alert"
        >
          {{ credentialError }}
        </div>

        <div class="mt-4 flex flex-wrap items-center justify-between gap-2">
          <input
            ref="fileInput"
            type="file"
            accept=".json,application/json"
            class="hidden"
            data-testid="claude-credential-file-input"
            @change="onCredentialFileSelected"
          >
          <Button
            :disabled="busy === 'import'"
            @click="chooseCredentialFile"
          >
            <span class="icon-[mdi--file-upload-outline] text-[14px]" />
            选择凭据文件
          </Button>
          <div class="center-row gap-2">
            <Button
              :disabled="busy === 'import'"
              @click="closeCredentialDialog"
            >
              取消
            </Button>
            <Button
              variant="primary"
              :disabled="isBusy || !credentialReady"
              @click="submitCredential"
            >
              <span
                v-if="busy === 'import'"
                class="icon-[mdi--loading] animate-spin text-[14px]"
              />
              {{ busy === "import" ? "正在确认令牌…" : credentialDialog.account ? "提交凭据" : "添加账号" }}
            </Button>
          </div>
        </div>
      </div>
    </div>

    <!-- 添加模型 -->
    <div
      v-if="modelDialog"
      class="fixed inset-0 z-[100000] flex items-center justify-center bg-black/50 p-4"
      @click.self="closeModelDialog"
      @keydown.esc="closeModelDialog"
    >
      <div
        class="flex max-h-full w-full max-w-[460px] flex-col rounded-[8px] border border-[#343434] bg-[#292929] p-5"
        role="dialog"
        aria-modal="true"
        aria-label="添加模型"
        data-testid="claude-model-dialog"
      >
        <h3 class="text-base font-medium text-white">
          添加模型
        </h3>
        <p class="mt-2 text-xs leading-relaxed text-[#a3a3a3]">
          选择要在 Cursor 里使用的模型，之后可以随时增减。
        </p>

        <div
          v-if="catalogLoading"
          class="mt-3 rounded-[6px] border border-[#343434] bg-[#232323] px-3 py-6 text-center text-xs text-[#8a8a8a]"
        >
          <span class="icon-[mdi--loading] animate-spin text-[16px]" /> 正在读取模型列表…
        </div>
        <template v-else>
          <div
            v-if="catalogError"
            class="mt-3 flex flex-wrap items-center justify-between gap-2 rounded-[6px] border border-[#4b1d1d] bg-[#2a1313] px-2.5 py-2 text-xs leading-relaxed text-[#fca5a5]"
            role="alert"
          >
            <span class="min-w-0 flex-1">{{ catalogError }}</span>
            <Button
              v-if="catalog.length === 0"
              @click="loadCatalog"
            >
              重试
            </Button>
          </div>
          <div
            v-if="catalog.length > 0"
            class="mt-3 flex min-h-0 flex-col"
          >
            <div class="mb-1.5 flex items-center justify-between text-[11px] text-[#8a8a8a]">
              <span>{{ `已选 ${selectedModelIds.length} 个` }}</span>
              <button
                v-if="selectableCatalog.length > 0"
                type="button"
                class="cursor-pointer text-[#7dd3a0] underline-offset-2 hover:underline"
                @click="toggleAllCatalogModels"
              >
                {{ allSelected ? "取消全选" : "全选" }}
              </button>
            </div>
            <div class="max-h-[46vh] min-h-0 overflow-y-auto rounded-[6px] border border-[#343434]">
              <label
                v-for="model in catalog"
                :key="model.id"
                class="flex items-center gap-2.5 border-b border-[#343434] px-3 py-2 text-xs last:border-b-0"
                :class="model.added ? 'cursor-default opacity-60' : 'cursor-pointer hover:bg-[#2f2f2f]'"
                data-testid="claude-catalog-model"
              >
                <input
                  type="checkbox"
                  class="size-4 shrink-0 accent-[#10AD5D]"
                  :checked="model.added || selectedModelIds.includes(model.id)"
                  :disabled="model.added"
                  @change="toggleCatalogModel(model)"
                >
                <span class="min-w-0 flex-1">
                  <span class="block truncate text-[#e5e5e5]">{{ model.displayName }}</span>
                  <span class="block truncate font-mono text-[10px] text-[#737373]">{{ model.id }}</span>
                </span>
                <span
                  v-if="model.added"
                  class="shrink-0 text-[10px] text-[#8a8a8a]"
                >
                  已添加
                </span>
              </label>
            </div>
          </div>
          <div
            v-else-if="!catalogError"
            class="mt-3 rounded-[6px] border border-dashed border-[#3f3f3f] px-3 py-6 text-center text-xs text-[#8a8a8a]"
          >
            Anthropic 没有返回可用的模型。
          </div>
        </template>

        <div class="mt-4 flex justify-end gap-2">
          <Button
            :disabled="busy === 'add-models'"
            @click="closeModelDialog"
          >
            取消
          </Button>
          <Button
            variant="primary"
            :disabled="isBusy || selectedModelIds.length === 0"
            @click="submitModels"
          >
            <span
              v-if="busy === 'add-models'"
              class="icon-[mdi--loading] animate-spin text-[14px]"
            />
            {{ `添加 ${selectedModelIds.length} 个模型` }}
          </Button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
/* 这个面板里被禁用的按钮（包括 Button 组件的根元素）统一变淡，
   让「还不能点」一眼可见；应用没有全局的禁用样式。 */
button:disabled {
  cursor: not-allowed;
  opacity: 0.55;
}
</style>

import { expect, test } from "@playwright/test";
import {
  PREVIEW_ADAPTERS,
  TEST_PLAN_KEY,
  basePreviewConfig,
  readStoredPreviewConfig,
  seedPreviewTestPlan,
} from "./helpers.mjs";

// 控制中心 → Claude 订阅。浏览器预览里没有 Anthropic：claudeAccountDirectory
// 说明哪个令牌属于哪个账号，不在其中的令牌一律被「Anthropic」拒绝。
const HOUR = 3_600_000;
const DAY = 24 * HOUR;
const CALLS_KEY = "cursor-byok.browser-preview.calls";

const TOKEN_ALICE = "sk-ant-oat01-alice-e2e-AAAAAAAAAAAAAAAAAAAA1111";
const TOKEN_ALICE_NEW = "sk-ant-oat01-alice-e2e-BBBBBBBBBBBBBBBBBBBB2222";
const TOKEN_BOB = "sk-ant-oat01-bob-e2e-CCCCCCCCCCCCCCCCCCCCCC3333";

function aliceAccount(extra = {}) {
  return {
    id: "acct-alice",
    email: "alice@example.test",
    displayName: "Alice",
    organizationName: "Alice's Organization",
    plan: "pro",
    rateLimitTier: "default_claude_pro",
    usageCredits: { enabled: false },
    usageWindows: [
      { id: "5h", usedPercent: 100, resetsAt: new Date(Date.now() + 3 * HOUR).toISOString() },
      { id: "7d", usedPercent: 74, resetsAt: new Date(Date.now() + 2 * DAY).toISOString() },
    ],
    ...extra,
  };
}

function bobAccount(extra = {}) {
  return {
    id: "acct-bob",
    email: "bob@example.test",
    displayName: "Bob",
    organizationName: "Bob's Team",
    plan: "max",
    rateLimitTier: "default_claude_max_20x",
    usageCredits: { enabled: true, usedPercent: 40 },
    usageWindows: [
      { id: "5h", usedPercent: 12 },
      { id: "7d", usedPercent: 48 },
      { id: "7d:fable", label: "Fable", usedPercent: 86 },
    ],
    ...extra,
  };
}

// 页面加载前就已保存的账号。
function savedAccount(extra = {}) {
  return aliceAccount({
    scopes: ["user:profile", "user:inference"],
    tokenHint: "sk-ant-oat01-…1111",
    expiresAtUnixMs: Date.now() + 364.5 * DAY,
    ...extra,
  });
}

const DIRECTORY = [
  { token: TOKEN_ALICE, account: aliceAccount() },
  { token: TOKEN_ALICE_NEW, account: aliceAccount() },
  { token: TOKEN_BOB, account: bobAccount() },
];

function credentialsFile(token, expiresAt = Date.now() + 364.5 * DAY) {
  return JSON.stringify({
    claudeAiOauth: {
      accessToken: token,
      refreshToken: null,
      expiresAt,
      scopes: ["user:profile", "user:inference"],
      subscriptionType: "pro",
    },
  });
}

function claudeRow(id, displayName, modelID) {
  return {
    id,
    displayName,
    groupName: "Claude Subscription",
    type: "anthropic",
    supplierID: "anthropic",
    baseURL: "https://api.anthropic.com",
    tooltipData: "Claude subscription",
    modelID,
    anthropicAuthMode: "oauth",
    balanceProfile: "none",
    fastMode: false,
  };
}

async function openClaudeTab(page, plan = {}, config = basePreviewConfig()) {
  await seedPreviewTestPlan(page, { claudeAccountDirectory: DIRECTORY, ...plan }, config);
  await page.goto("/control-center?tab=claude");
}

async function recordedCalls(page, name) {
  const calls = await page.evaluate((key) => JSON.parse(localStorage.getItem(key) || "[]"), CALLS_KEY);
  return calls.filter((call) => call.name === name);
}

// 页面不重新加载时改测试计划：预览实现每次调用都会重新读取它。
async function updatePlan(page, patch) {
  await page.evaluate(({ key, value }) => {
    const plan = JSON.parse(localStorage.getItem(key) || "{}");
    localStorage.setItem(key, JSON.stringify({ ...plan, ...value }));
  }, { key: TEST_PLAN_KEY, value: patch });
}

function fieldInput(page, labelText) {
  return page.locator("label").filter({ hasText: labelText }).locator("input").first();
}

test("添加第一个账号：粘贴凭据后显示账号详情，并引导选择模型", async ({ page }) => {
  await openClaudeTab(page, { recordCalls: true });
  const tab = page.getByRole("tab", { name: /Claude 订阅/ });
  await expect(tab).toHaveAttribute("aria-selected", "true");
  await expect(page.getByTestId("claude-accounts-empty")).toBeVisible();

  await page.getByRole("button", { name: "添加账号" }).click();
  const dialog = page.getByTestId("claude-credential-dialog");
  const submit = dialog.getByRole("button", { name: "添加账号", exact: true });
  await expect(submit).toBeDisabled();

  // 不是凭据的内容：说明原因，对话框留着。
  await dialog.getByLabel("凭据内容").fill("{not json");
  await submit.click();
  await expect(dialog.getByRole("alert")).toHaveText("This is neither a Claude credentials file nor an access token.");

  const credentials = credentialsFile(TOKEN_ALICE);
  await dialog.getByLabel("凭据内容").fill(credentials);
  await submit.click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByText("已添加 alice@example.test", { exact: true })).toBeVisible();

  // 还没有模型：直接进入选模型。
  const modelDialog = page.getByTestId("claude-model-dialog");
  await expect(modelDialog.getByTestId("claude-catalog-model")).toHaveCount(3);
  await modelDialog.getByText("Claude Opus 4.8").click();
  await modelDialog.getByText("Claude Haiku 4.5").click();
  await expect(modelDialog.getByText("已选 2 个")).toBeVisible();
  await modelDialog.getByRole("button", { name: "添加 2 个模型", exact: true }).click();
  await expect(modelDialog).toHaveCount(0);
  await expect(page.getByText("已添加 2 个模型", { exact: true })).toBeVisible();

  // 账号详情：身份、套餐、状态、用量窗口、令牌提示与有效期。
  const account = page.getByTestId("claude-account");
  await expect(account).toHaveCount(1);
  await expect(account).toContainText("alice@example.test");
  await expect(account).toContainText("Alice · Alice's Organization");
  await expect(account).toContainText("Pro");
  await expect(account.getByTestId("claude-account-status")).toHaveText("正常");
  await expect(account).toContainText("2 个模型");
  const windows = account.getByTestId("claude-usage-window");
  await expect(windows).toHaveCount(2);
  await expect(windows.nth(0)).toContainText("当前会话（5 小时）");
  await expect(windows.nth(0)).toContainText("已用 100%");
  await expect(windows.nth(0)).toContainText("重置");
  await expect(windows.nth(1)).toContainText("本周（全部模型）");
  await expect(windows.nth(1)).toContainText("已用 74%");
  await expect(account.getByTestId("claude-account-exhausted")).toContainText("「当前会话（5 小时）」额度已用完");
  await expect(account.getByTestId("claude-account-exhausted")).toContainText("重置前 Anthropic 会拒绝这个账号的请求。");
  await expect(account).toContainText("sk-ant-oat01-…1111");
  await expect(account.getByTestId("claude-account-expiry")).toContainText("剩余 364 天");
  await expect(account).toContainText("未开启");
  await expect(account).toContainText("default_claude_pro");

  const models = page.getByTestId("claude-model");
  await expect(models).toHaveCount(2);
  await expect(models.nth(0)).toContainText("Claude Opus 4.8");
  await expect(models.nth(0)).toContainText("claude-opus-4-8");
  await expect(models.nth(1)).toContainText("Claude Haiku 4.5");
  await expect(tab).toContainText("1");

  // 模型行写进了配置，按订阅 OAuth 鉴权。
  const stored = await readStoredPreviewConfig(page);
  const rows = stored.modelAdapters.filter((row) => row.groupName === "Claude Subscription");
  expect(rows.map((row) => row.modelID)).toEqual(["claude-opus-4-8", "claude-haiku-4-5"]);
  for (const row of rows) {
    expect(row.type).toBe("anthropic");
    expect(row.baseURL).toBe("https://api.anthropic.com");
    expect(row.anthropicAuthMode).toBe("oauth");
  }

  // 令牌不出现在页面、本地存储和调用记录里。（测试计划本身带着令牌，不在检查之列。）
  expect(await page.content()).not.toContain(TOKEN_ALICE);
  const storedElsewhere = await page.evaluate((planKey) => {
    const entries = { ...localStorage };
    delete entries[planKey];
    return JSON.stringify(entries);
  }, TEST_PLAN_KEY);
  expect(storedElsewhere).toContain("claude-opus-4-8");
  expect(storedElsewhere).not.toContain(TOKEN_ALICE);
  const imports = await recordedCalls(page, "ImportClaudeAccount");
  expect(imports.map((call) => call.args)).toEqual([[{ length: 9 }], [{ length: credentials.length }]]);
});

test("多个账号：选择文件添加第二个账号，更换凭据不新增账号，移除账号时按归属处理模型", async ({ page }) => {
  const config = basePreviewConfig();
  config.modelAdapters.push(claudeRow("claude-row-sonnet", "Claude Sonnet 4.6", "claude-sonnet-4-6"));
  await openClaudeTab(page, {
    claudeAccounts: [savedAccount()],
    claudeAccountModels: [{ id: "claude-row-sonnet", accountIds: ["acct-alice"] }],
  }, config);

  const accounts = page.getByTestId("claude-account");
  const models = page.getByTestId("claude-model");
  await expect(accounts).toHaveCount(1);
  await expect(models).toHaveCount(1);

  // 第二个账号：亲手选择凭据文件。文件内容不显示在界面上。
  await page.getByRole("button", { name: "添加账号" }).click();
  const dialog = page.getByTestId("claude-credential-dialog");
  await dialog.getByTestId("claude-credential-file-input").setInputFiles({
    name: ".credentials.json",
    mimeType: "application/json",
    buffer: Buffer.from(credentialsFile(TOKEN_BOB)),
  });
  await expect(dialog.getByTestId("claude-credential-file")).toContainText("已选择文件 .credentials.json");
  await expect(dialog.getByLabel("凭据内容")).toHaveCount(0);
  expect(await page.content()).not.toContain(TOKEN_BOB);
  await dialog.getByRole("button", { name: "添加账号", exact: true }).click();
  await expect(page.getByText("已添加 bob@example.test，并让它一起提供 1 个模型", { exact: true })).toBeVisible();
  // 已经有模型了，不再弹出选模型。
  await expect(page.getByTestId("claude-model-dialog")).toHaveCount(0);

  await expect(accounts).toHaveCount(2);
  const alice = accounts.filter({ hasText: "alice@example.test" });
  const bob = accounts.filter({ hasText: "bob@example.test" });
  await expect(bob).toContainText("Max");
  await expect(bob).toContainText("Bob · Bob's Team");
  await expect(bob).toContainText("1 个模型");
  await expect(bob.getByTestId("claude-usage-window")).toHaveCount(3);
  await expect(bob).toContainText("本周（Fable）");
  await expect(bob).toContainText("已开启（已用 40%）");
  await expect(bob).toContainText("default_claude_max_20x");
  await expect(page.getByRole("tab", { name: /Claude 订阅/ })).toContainText("2");

  // 给已有账号提交新凭据：替换令牌，不新增账号。
  await expect(alice).toContainText("sk-ant-oat01-…1111");
  await alice.getByRole("button", { name: "更换凭据", exact: true }).click();
  await expect(dialog).toContainText("提交 alice@example.test 的新凭据，旧令牌会被替换。");
  await dialog.getByLabel("凭据内容").fill(TOKEN_ALICE_NEW);
  await dialog.getByRole("button", { name: "提交凭据", exact: true }).click();
  await expect(page.getByText("已更新 alice@example.test 的凭据", { exact: true })).toBeVisible();
  await expect(accounts).toHaveCount(2);
  await expect(alice).toContainText("sk-ant-oat01-…2222");
  await expect(alice).not.toContainText("sk-ant-oat01-…1111");

  // 移除第二个账号：模型还有第一个账号提供，留下。
  await bob.getByRole("button", { name: "移除", exact: true }).click();
  const confirm = page.getByRole("dialog", { name: "移除 Claude 账号" });
  await expect(confirm).toContainText("将移除 bob@example.test，保存在本机的令牌会一并删除。");
  await expect(confirm).toContainText("1 个模型会继续由其他账号提供。");
  await confirm.getByRole("button", { name: "移除", exact: true }).click();
  await expect(page.getByText("已移除 bob@example.test", { exact: true })).toBeVisible();
  await expect(accounts).toHaveCount(1);
  await expect(models).toHaveCount(1);

  // 移除最后一个账号：只由它提供的模型一并移除，其他模型不动。
  await alice.getByRole("button", { name: "移除", exact: true }).click();
  await expect(confirm).toContainText("1 个只由它提供的模型会从模型列表中移除。");
  await confirm.getByRole("button", { name: "移除", exact: true }).click();
  await expect(page.getByTestId("claude-accounts-empty")).toBeVisible();
  await expect(models).toHaveCount(0);
  const stored = await readStoredPreviewConfig(page);
  expect(stored.modelAdapters.map((row) => row.id)).toEqual(PREVIEW_ADAPTERS.map((row) => row.id));
});

test("账号状态：过期、被拒绝、读不到资料、快到期各有说明；打开页面和点刷新都会更新用量", async ({ page }) => {
  await openClaudeTab(page, {
    recordCalls: true,
    claudeAccounts: [
      savedAccount({ id: "acct-expired", email: "expired@example.test", expiresAtUnixMs: Date.now() - DAY }),
      savedAccount({
        id: "acct-rejected",
        email: "rejected@example.test",
        status: "rejected",
        detailsError: "claude_subscription_token_rejected: Anthropic refused this token",
      }),
      {
        id: "acct-bare",
        tokenHint: "sk-ant-oat01-…9999",
        detailsError: "claude_subscription_details_unavailable: this token may not read the account's profile",
        usageAtUnixMs: 0,
      },
      // 用量读取于很久以前：打开页面时会重新读取。
      savedAccount({ id: "acct-soon", email: "soon@example.test", expiresAtUnixMs: Date.now() + 5.5 * HOUR, usageAtUnixMs: 0 }),
    ],
    claudeAccountRefresh: {
      "acct-soon": { usageWindows: [{ id: "5h", usedPercent: 33 }, { id: "7d", usedPercent: 81 }] },
    },
  });

  const accounts = page.getByTestId("claude-account");
  await expect(accounts).toHaveCount(4);

  const expired = accounts.filter({ hasText: "expired@example.test" });
  await expect(expired.getByTestId("claude-account-status")).toHaveText("令牌已过期");
  await expect(expired.getByRole("alert")).toHaveText("令牌已过期，这个账号暂时提供不了模型。请用「更换凭据」提交新的凭据。");
  await expect(expired.getByTestId("claude-account-expiry")).toContainText("已过期");

  const rejected = accounts.filter({ hasText: "rejected@example.test" });
  await expect(rejected.getByTestId("claude-account-status")).toHaveText("令牌已失效");
  await expect(rejected.getByRole("alert")).toHaveText("Anthropic 拒绝了这个令牌，这个账号暂时提供不了模型。请用「更换凭据」提交新的凭据。");

  // 令牌可用但读不到资料：不算故障，只是没有资料和用量可显示。
  const bare = accounts.filter({ hasText: "sk-ant-oat01-…9999" });
  await expect(bare).toContainText("未命名账号");
  await expect(bare.getByTestId("claude-account-status")).toHaveText("正常");
  await expect(bare).toContainText("这个令牌可以正常使用，但没有读取账号资料和用量的权限，所以这里显示不了这些信息。");
  await expect(bare.getByTestId("claude-usage-window")).toHaveCount(0);
  await expect(bare.getByTestId("claude-account-expiry")).toHaveText("未知");

  const soon = accounts.filter({ hasText: "soon@example.test" });
  await expect(soon).toContainText("令牌快到期了。到期后需要用「更换凭据」重新提交，应用不会自动续期。");
  await expect(soon.getByTestId("claude-account-expiry")).toContainText("剩余 5 小时");
  // 打开页面时自动读了一次用量，只针对读得到用量、令牌可用的账号。
  await expect(soon.getByTestId("claude-usage-window").nth(0)).toContainText("已用 33%");
  await expect(soon.getByTestId("claude-usage-window").nth(1)).toContainText("已用 81%");
  expect((await recordedCalls(page, "RefreshClaudeAccount")).map((call) => call.args[0])).toEqual(["acct-soon"]);

  // 有账号的令牌不可用：标签上提示需关注。
  await expect(page.getByRole("tab", { name: /Claude 订阅/ })).toHaveAttribute("title", /需关注/);

  // 点刷新：读到新的用量。这个账号开了额外用量，额度用完后的说明随之不同。
  await updatePlan(page, {
    claudeAccountRefresh: {
      "acct-soon": {
        usageWindows: [{ id: "5h", usedPercent: 100, resetsAt: new Date(Date.now() + HOUR).toISOString() }],
        usageCredits: { enabled: true },
      },
      "acct-bare": {
        status: "rejected",
        detailsError: "claude_subscription_token_rejected: Anthropic refused this token",
      },
    },
  });
  await soon.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByText("已刷新", { exact: true })).toBeVisible();
  await expect(soon.getByTestId("claude-usage-window")).toHaveCount(1);
  await expect(soon.getByTestId("claude-usage-window")).toContainText("已用 100%");
  await expect(soon.getByTestId("claude-account-exhausted")).toContainText("重置前的请求计入额外用量。");

  // 刷新时发现令牌被拒绝：状态随之改变，并直接告诉用户。
  await bare.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(bare.getByTestId("claude-account-status")).toHaveText("令牌已失效");
  await expect(bare.getByRole("alert")).toContainText("Anthropic 拒绝了这个令牌");
});

test("模型：Fast 开关写入配置并说明账号是否支持，移除只删模型，编辑进入模型编辑页", async ({ page }) => {
  const config = basePreviewConfig();
  config.modelAdapters.push(
    claudeRow("claude-row-opus", "Claude Opus 4.8", "claude-opus-4-8"),
    claudeRow("claude-row-sonnet", "Claude Sonnet 4.6", "claude-sonnet-4-6"),
  );
  await openClaudeTab(page, {
    claudeAccounts: [savedAccount(), bobAccount({ tokenHint: "sk-ant-oat01-…3333", status: "rejected" })],
    claudeAccountModels: [
      { id: "claude-row-opus", accountIds: ["acct-alice", "acct-bob"] },
      { id: "claude-row-sonnet", accountIds: ["acct-alice"] },
    ],
  }, config);

  const models = page.getByTestId("claude-model");
  await expect(models).toHaveCount(2);
  const opus = models.filter({ hasText: "Claude Opus 4.8" });
  const sonnet = models.filter({ hasText: "Claude Sonnet 4.6" });
  // 只由部分账号提供的模型会标出来。
  await expect(sonnet).toContainText("仅 1 个账号提供");
  await expect(opus).not.toContainText("个账号提供");

  // 打开 Fast：写入配置。能用的账号都没开额外用量，所以说明 Fast 用不上。
  // （开了额外用量的那个账号令牌已失效，不算数。）
  await expect(page.getByTestId("claude-fast-unavailable")).toHaveCount(0);
  const fast = opus.getByRole("switch");
  await expect(fast).toHaveAttribute("aria-checked", "false");
  await fast.click();
  await expect(fast).toHaveAttribute("aria-checked", "true");
  await expect(page.getByTestId("claude-fast-unavailable")).toBeVisible();
  let stored = await readStoredPreviewConfig(page);
  expect(stored.modelAdapters.find((row) => row.id === "claude-row-opus").fastMode).toBe(true);
  expect(stored.modelAdapters.find((row) => row.id === "claude-row-sonnet").fastMode).toBe(false);

  // 移除模型：账号不受影响。
  await sonnet.getByRole("button", { name: "移除", exact: true }).click();
  const confirm = page.getByRole("dialog", { name: "移除模型" });
  await expect(confirm).toContainText("将把 Claude Sonnet 4.6 从模型列表中移除，账号不受影响。");
  await confirm.getByRole("button", { name: "移除", exact: true }).click();
  await expect(page.getByText("已移除 Claude Sonnet 4.6", { exact: true })).toBeVisible();
  await expect(models).toHaveCount(1);
  await expect(page.getByTestId("claude-account")).toHaveCount(2);
  stored = await readStoredPreviewConfig(page);
  expect(stored.modelAdapters.some((row) => row.id === "claude-row-sonnet")).toBe(false);

  // 再点一次关掉 Fast，说明随之消失。
  await fast.click();
  await expect(fast).toHaveAttribute("aria-checked", "false");
  await expect(page.getByTestId("claude-fast-unavailable")).toHaveCount(0);

  // 其余设置在模型编辑页里改。
  await opus.getByRole("button", { name: "编辑", exact: true }).click();
  await expect(page).toHaveURL(/\/model-editor\?index=\d+/);
  await expect(fieldInput(page, "显示名称")).toHaveValue("Claude Opus 4.8");
});

test("提交的凭据不可用时说明原因；模型列表读取失败可以重试", async ({ page }) => {
  await openClaudeTab(page, {
    claudeAccountFailures: {
      models: "claude_subscription_models_failed: the model list could not be read: HTTP 500: Internal server error",
    },
  });

  await page.getByRole("button", { name: "添加账号" }).click();
  const dialog = page.getByTestId("claude-credential-dialog");
  const input = dialog.getByLabel("凭据内容");
  const submit = dialog.getByRole("button", { name: "添加账号", exact: true });
  const alert = dialog.getByRole("alert");

  await input.fill("sk-ant-api03-not-a-subscription-token");
  await submit.click();
  await expect(alert).toHaveText("This is not a Claude subscription token. Subscription tokens start with sk-ant-oat; an API key goes into Model Settings instead.");

  await input.fill("sk-ant-oat01-unknown-e2e-DDDDDDDDDDDDDDDD4444");
  await submit.click();
  await expect(alert).toHaveText("Anthropic refused this token. Sign in again with the Claude CLI and submit the new credentials.");

  await input.fill(credentialsFile(TOKEN_ALICE, Date.UTC(2026, 0, 2, 12)));
  await submit.click();
  await expect(alert).toHaveText("This token has expired. Sign in again with the Claude CLI and submit the new credentials. (this token expired on 2026-01-02)");

  await input.fill('{"claudeAiOauth":{}}');
  await submit.click();
  await expect(alert).toHaveText("No access token was found in it. A Claude credentials file holds one under claudeAiOauth.accessToken.");
  await expect(page.getByTestId("claude-account")).toHaveCount(0);

  // 取消后重新打开，上次填的内容不留。
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await page.getByRole("button", { name: "添加账号" }).click();
  await expect(input).toHaveValue("");
  await expect(alert).toHaveCount(0);

  // 账号加好后进入选模型，但模型列表读不到：给出原因和重试。
  await input.fill(TOKEN_ALICE);
  await submit.click();
  const modelDialog = page.getByTestId("claude-model-dialog");
  await expect(modelDialog.getByRole("alert")).toContainText(
    "Could not read the model list from Anthropic. (the model list could not be read: HTTP 500: Internal server error)",
  );
  await updatePlan(page, { claudeAccountFailures: {} });
  await modelDialog.getByRole("button", { name: "重试", exact: true }).click();
  await expect(modelDialog.getByTestId("claude-catalog-model")).toHaveCount(3);
  await expect(modelDialog.getByRole("alert")).toHaveCount(0);

  await modelDialog.getByRole("button", { name: "全选", exact: true }).click();
  await expect(modelDialog.getByText("已选 3 个")).toBeVisible();
  await modelDialog.getByRole("button", { name: "取消全选", exact: true }).click();
  await expect(modelDialog.getByRole("button", { name: "添加 0 个模型", exact: true })).toBeDisabled();
  await modelDialog.getByRole("button", { name: "取消", exact: true }).click();
  await expect(page.getByTestId("claude-models-empty")).toBeVisible();
  await expect(page.getByRole("button", { name: "添加模型" })).toBeEnabled();
});

test("英文界面：Claude 订阅页、对话框和确认框里的文字都有译文", async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem("cursor-client:locale:v1", "en-US");
    localStorage.setItem("cursor-client:locale-source:v1", "manual");
  });
  const config = basePreviewConfig();
  config.modelAdapters.push(
    claudeRow("claude-row-opus", "Claude Opus 4.8", "claude-opus-4-8"),
    { ...claudeRow("claude-row-sonnet", "Claude Sonnet 4.6", "claude-sonnet-4-6"), fastMode: true, disabled: true },
  );
  await openClaudeTab(page, {
    claudeAccounts: [
      savedAccount(),
      bobAccount({ tokenHint: "sk-ant-oat01-…3333", expiresAtUnixMs: Date.now() + 5.5 * HOUR }),
      savedAccount({ id: "acct-expired", email: "expired@example.test", expiresAtUnixMs: Date.now() - DAY }),
      { id: "acct-bare", tokenHint: "sk-ant-oat01-…9999", detailsError: "claude_subscription_details_unavailable: this token may not read the account's profile" },
    ],
    claudeAccountModels: [
      { id: "claude-row-opus", accountIds: ["acct-alice", "acct-bob"] },
      { id: "claude-row-sonnet", accountIds: ["acct-alice"] },
    ],
  }, config);

  await expect(page.getByRole("tab", { name: /Claude subscription/ })).toHaveAttribute("aria-selected", "true");
  const panel = page.getByTestId("claude-accounts-panel");
  await expect(panel.getByTestId("claude-account")).toHaveCount(4);
  await expect(panel).toContainText("Current session (5 hours)");
  await expect(panel).toContainText("Current week (all models)");
  await expect(panel).toContainText("Current week (Fable)");
  await expect(panel).toContainText("100% used");
  await expect(panel.getByTestId("claude-account-exhausted").first()).toContainText("Until then, Anthropic refuses this account's requests.");
  await expect(panel).toContainText("364 day(s) left");
  await expect(panel).toContainText("5 h left");
  await expect(panel).toContainText("Token expired");
  await expect(panel).toContainText("Unnamed account");
  await expect(panel).toContainText("served by 1 account(s) only");
  expect(await panel.innerText()).not.toMatch(/\p{Script=Han}/u);

  await page.getByRole("button", { name: "Add account" }).click();
  const dialog = page.getByTestId("claude-credential-dialog");
  await expect(dialog).toContainText("What you can submit");
  await expect(dialog).toContainText("claude setup-token makes a long-lived token, valid for one year.");
  expect(await dialog.innerText()).not.toMatch(/\p{Script=Han}/u);
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();

  await page.getByRole("button", { name: "Add models" }).click();
  const modelDialog = page.getByTestId("claude-model-dialog");
  await expect(modelDialog.getByTestId("claude-catalog-model")).toHaveCount(3);
  await modelDialog.getByText("Claude Haiku 4.5").click();
  await expect(modelDialog.getByRole("button", { name: "Add 1 model(s)", exact: true })).toBeEnabled();
  expect(await modelDialog.innerText()).not.toMatch(/\p{Script=Han}/u);
  await modelDialog.getByRole("button", { name: "Cancel", exact: true }).click();

  const alice = panel.getByTestId("claude-account").filter({ hasText: "alice@example.test" });
  await alice.getByRole("button", { name: "Remove", exact: true }).click();
  const confirm = page.getByRole("dialog", { name: "Remove Claude account" });
  await expect(confirm).toContainText("alice@example.test will be removed, and its token saved on this computer will be deleted.");
  await expect(confirm).toContainText("1 model(s) stay, served by the other accounts.");
  await expect(confirm).toContainText("1 model(s) that only this account serves will be removed from the model list.");
  expect(await confirm.innerText()).not.toMatch(/\p{Script=Han}/u);
  await confirm.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(panel.getByTestId("claude-account")).toHaveCount(4);
});

test("没有令牌可用的账号时不能添加模型", async ({ page }) => {
  await openClaudeTab(page, {
    claudeAccounts: [savedAccount({ expiresAtUnixMs: Date.now() - DAY })],
  });
  await expect(page.getByTestId("claude-account")).toHaveCount(1);
  const add = page.getByRole("button", { name: "添加模型" });
  await expect(add).toBeDisabled();
  await expect(add).toHaveAttribute("title", "需要先有一个令牌可用的账号");
  await expect(page.getByTestId("claude-models-empty")).toBeVisible();
});

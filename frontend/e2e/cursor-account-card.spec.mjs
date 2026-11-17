import { expect, test } from "@playwright/test";
import { basePreviewConfig, seedPreviewTestPlan } from "./helpers.mjs";

const previewAccounts = [
  { id: "account-a", email: "a@example.test", authIdHint: "auth-a", tags: ["工作"], isCurrent: true },
  { id: "account-b", email: "b@example.test", authIdHint: "auth-b", tags: [], isCurrent: false },
];

test("账户面板可选择账户并在确认后请求切换", async ({ page }) => {
  await seedPreviewTestPlan(page, { cursorAccounts: previewAccounts }, basePreviewConfig());
  // 账号管理已拆到控制中心，首页不再渲染账号卡
  await page.goto("/control-center?tab=accounts");
  await expect(page.getByText("a@example.test")).toBeVisible();
  await page.getByRole("button", { name: "Switch to Cursor" }).nth(1).click();
  const backup = page.getByRole("dialog", { name: "Back up Cursor state?" });
  await expect(backup).toBeVisible();
  await backup.getByRole("button", { name: "Back up and switch" }).click();
  const dialog = page.getByRole("dialog", { name: "Switch Cursor account" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Close and switch" }).click();
  await expect(page.getByText("Switched and restarted Cursor")).toBeVisible();
});

test("控制中心非法标签回落到账号页", async ({ page }) => {
  await seedPreviewTestPlan(page, { cursorAccounts: previewAccounts }, basePreviewConfig());
  await page.goto("/control-center?tab=not-a-tab");
  await expect(page.getByRole("tab", { name: "账号" })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByText("a@example.test")).toBeVisible();
});

test("控制中心请求实验室可以对比结构且不展示原文", async ({ page }) => {
  await seedPreviewTestPlan(page, { cursorAccounts: previewAccounts }, basePreviewConfig());
  await page.goto("/control-center?tab=request-lab");
  await expect(page.getByRole("heading", { name: "官方镜像" })).toBeVisible();
  await page.getByRole("button", { name: /gpt-test/ }).first().click();
  await page.getByRole("button", { name: /gpt-test/ }).nth(1).click();
  await page.getByRole("button", { name: "对比结构" }).click();
  await expect(page.getByText("/messages/count")).toBeVisible();
  await expect(page.getByText("sk-")).toHaveCount(0);
});

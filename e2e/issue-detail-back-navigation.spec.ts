import { test, expect } from "@playwright/test";
import { createTestApi } from "./helpers";
import type { TestApiClient } from "./fixtures";

// Mobile web: after following a link out of a long issue and pressing Back,
// the reader must land on the comment they were reading.
test.describe("Issue detail back navigation (390x844)", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  let api: TestApiClient;

  test.beforeEach(async () => {
    api = await createTestApi();
  });

  test.afterEach(async () => {
    await api?.cleanup();
  });

  test("Back returns to the comment the reader scrolled to", async ({ page }) => {
    const stamp = Date.now();
    const linked = await api.createIssue(`Back-nav linked ${stamp}`);
    const issue = await api.createIssue(`Back-nav long issue ${stamp}`);

    const target = 30;
    for (let i = 0; i < 60; i++) {
      const body =
        i === target
          ? `Comment ${i} target: see [${linked.identifier}](mention://issue/${linked.id})`
          : `Comment ${i}: ${"filler text ".repeat(25)}`;
      await api.createComment(issue.id, body);
    }

    const [workspace] = await api.getWorkspaces();
    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token!);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken());
    await page.goto(`/${workspace!.slug}/issues/${issue.identifier}`, { waitUntil: "domcontentloaded" });
    await expect(page.getByRole("button", { name: issue.title, exact: true })).toBeVisible({ timeout: 30_000 });

    // The timeline is virtualized: the target only mounts once scrolled near.
    const targetText = page.getByText(`Comment ${target} target`).first();
    await page.mouse.move(195, 400);
    for (let i = 0; i < 80 && (await targetText.count()) === 0; i++) {
      await page.mouse.wheel(0, 600);
      await page.waitForTimeout(100);
    }
    await targetText.scrollIntoViewIfNeeded();
    await expect(targetText).toBeInViewport();

    await page.getByRole("link", { name: linked.identifier }).first().click();
    await expect(page.getByRole("button", { name: linked.title, exact: true })).toBeVisible({ timeout: 30_000 });

    await page.goBack();
    await expect(page.getByRole("button", { name: issue.title, exact: true })).toBeVisible({ timeout: 30_000 });

    await expect(targetText).toBeInViewport({ timeout: 10_000 });
  });
});

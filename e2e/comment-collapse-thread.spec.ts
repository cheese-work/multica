import { expect, test, type Page } from "@playwright/test";
import { createTestApi, loginAsDefault, waitForPageText } from "./helpers";
import type { TestApiClient } from "./fixtures";

// CHE-5437: ONE click on "Collapse thread" must collapse the thread, on a real
// browser, whatever the thread has or holds.

async function openIssue(page: Page, workspaceSlug: string, issueId: string, hash = "") {
  await page.goto(`/${workspaceSlug}/issues/${issueId}${hash}`, { waitUntil: "domcontentloaded" });
  await page.waitForURL((url) => !url.pathname.includes(issueId), { timeout: 30000, waitUntil: "domcontentloaded" });
}

/** The action buttons take pointer events only while the header row is hovered: hover as a pointer does, press once. */
async function clickOnce(page: Page, name: string) {
  const button = page.getByRole("button", { name, exact: true });
  await button.locator("xpath=ancestor::div[contains(@class, 'gap-2.5')][1]").hover();
  await button.click();
}

/** CHE-5568: the toggle's offset from the thread card's top-right corner. */
async function toggleOffset(page: Page, name: string) {
  const box = await page.getByRole("button", { name, exact: true }).boundingBox();
  const card = await page.locator("[data-thread-root]").first().boundingBox();
  if (!box || !card) throw new Error("toggle or card not laid out");
  return { x: card.x + card.width - (box.x + box.width), y: box.y - card.y };
}

test.describe("Collapse thread, one click", () => {
  let api: TestApiClient;
  let issueId: string;
  let issueTitle: string;
  let workspaceSlug: string;
  let rootId: string;
  const ROOT = "Collapse root body sentinel";

  test.beforeEach(async ({ page }) => {
    api = await createTestApi();
    issueTitle = "E2E Collapse Thread " + Date.now();
    issueId = (await api.createIssue(issueTitle)).id;
    rootId = (await api.createComment(issueId, ROOT)).id;
    workspaceSlug = await loginAsDefault(page);
  });

  test.afterEach(async () => {
    if (api) await api.cleanup();
  });

  test("a thread with no replies collapses on the first click and expands again", async ({ page }) => {
    await openIssue(page, workspaceSlug, issueId);
    await waitForPageText(page, ROOT);

    await clickOnce(page, "Collapse thread");
    await expect(page.getByRole("button", { name: "Expand thread", exact: true })).toBeVisible();
    await expect(page.getByText(ROOT)).toBeVisible(); // one-line preview
    await expect(page.getByPlaceholder("Leave a reply…")).toHaveCount(0);

    await clickOnce(page, "Expand thread");
    await expect(page.getByRole("button", { name: "Collapse thread", exact: true })).toBeVisible();
  });

  test("a thread opened from a comment link collapses on the first click", async ({ page }) => {
    await openIssue(page, workspaceSlug, issueId, `#comment-${rootId}`);
    await waitForPageText(page, ROOT);

    await clickOnce(page, "Collapse thread");
    await expect(page.getByRole("button", { name: "Expand thread", exact: true })).toBeVisible();
  });

  test("a thread whose body text is selected collapses on the first click", async ({ page }) => {
    await openIssue(page, workspaceSlug, issueId);
    await waitForPageText(page, ROOT);

    await page.getByText(ROOT).dblclick();
    await clickOnce(page, "Collapse thread");
    await expect(page.getByRole("button", { name: "Expand thread", exact: true })).toBeVisible();
  });

  for (const replies of [0, 1]) {
    test(`the toggle keeps its position across collapse and expand (${replies} replies)`, async ({ page }) => {
      for (let i = 0; i < replies; i++) await api.createComment(issueId, `reply ${i}`, rootId);
      await openIssue(page, workspaceSlug, issueId);
      await waitForPageText(page, ROOT);

      const expanded = await toggleOffset(page, "Collapse thread");
      await clickOnce(page, "Collapse thread");
      await expect(page.getByRole("button", { name: "Expand thread", exact: true })).toBeVisible();
      const collapsed = await toggleOffset(page, "Expand thread");

      expect(Math.abs(collapsed.x - expanded.x)).toBeLessThanOrEqual(1);
      expect(Math.abs(collapsed.y - expanded.y)).toBeLessThanOrEqual(1);
    });
  }
});

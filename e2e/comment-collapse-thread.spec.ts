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
});

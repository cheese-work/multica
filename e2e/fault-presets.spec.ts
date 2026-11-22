import { test, expect, type Page } from "@playwright/test";
import { createTestApi, loginAsDefault } from "./helpers";
import type { TestApiClient } from "./fixtures";
import { applyFault, isFaultPreset } from "./fault-presets";

// CHE-6753 L2: fault presets (A1) in the e2e harness. Each preset overrides one API
// response in the test's own browser context and shows the marker the app already
// renders for that failure. The markers were read from the app code and confirmed on
// the seeded local instance:
//   error             board column button "Loading more failed — Retry"
//                     (packages/views/issues/components/board-view.tsx, `table.load_more_failed_retry`)
//   permission        heading "You don't have access to this agent"
//                     (packages/views/agents/components/agent-detail-page.tsx, `detail.no_access_title`)
//   not-found         "Issue not found" (IssueNotFound, packages/views/issues/components/issue-detail.tsx)
//   loading           [data-slot="skeleton"] (Skeleton, packages/ui/components/ui/skeleton.tsx)
const ISSUES_LIST = "**/api/issues/table/rows";
const errorMarker = (page: Page) => page.getByRole("button", { name: "Loading more failed — Retry" }).first();
const skeleton = (page: Page) => page.locator('[data-slot="skeleton"]').first();

test.describe("Fault presets", () => {
  let api: TestApiClient;
  let slug: string;
  let issueIdentifier: string;
  let issueTitle: string;

  test.beforeEach(async ({ page }) => {
    api = await createTestApi();
    issueTitle = `E2E Fault ${Date.now()}`;
    const issue = await api.createIssue(issueTitle);
    issueIdentifier = issue.identifier;
    slug = await loginAsDefault(page);
  });

  test.afterEach(async () => {
    if (api) await api.cleanup();
  });

  test("control: without a fault the issues list shows its rows and no error marker", async ({ page }) => {
    await page.goto(`/${slug}/issues`, { waitUntil: "domcontentloaded" });
    await expect(page.getByText(issueTitle)).toBeVisible({ timeout: 30000 });
    await expect(errorMarker(page)).toHaveCount(0);
  });

  test("http-500 on the issues list shows the error marker", async ({ page }) => {
    const fault = await applyFault(page, "http-500", ISSUES_LIST);
    await page.goto(`/${slug}/issues`, { waitUntil: "domcontentloaded" });
    await expect(errorMarker(page)).toBeVisible({ timeout: 30000 });
    await expect(page.getByText(issueTitle)).toHaveCount(0);
    expect(fault.hits()).toBeGreaterThan(0);
  });

  test("http-403 on a private agent shows the permission marker", async ({ page }) => {
    // The issues list has no distinct 403 view (it shows the same error marker as a 500).
    // The permission-refused screen is the agent detail page, which reads the status.
    const agentId = "00000000-0000-4000-8000-000000000403";
    const fault = await applyFault(page, "http-403", `**/api/agents/${agentId}`);
    await page.goto(`/${slug}/agents/${agentId}`, { waitUntil: "domcontentloaded" });
    await expect(page.getByText("You don't have access to this agent").first()).toBeVisible({ timeout: 30000 });
    expect(fault.hits()).toBeGreaterThan(0);
  });

  test("http-404 on issues/[id] shows the not-found marker", async ({ page }) => {
    const fault = await applyFault(page, "http-404", `**/api/issues/${issueIdentifier}`);
    await page.goto(`/${slug}/issues/${issueIdentifier}`, { waitUntil: "domcontentloaded" });
    await expect(page.getByText("Issue not found")).toBeVisible({ timeout: 30000 });
    expect(fault.hits()).toBeGreaterThan(0);
  });

  test("hold on the issues list shows the loading marker and release lets the rows through", async ({ page }) => {
    const fault = await applyFault(page, "hold", ISSUES_LIST);
    await page.goto(`/${slug}/issues`, { waitUntil: "domcontentloaded" });
    await expect(skeleton(page)).toBeVisible({ timeout: 30000 });
    await expect(page.getByText(issueTitle)).toHaveCount(0);
    await expect(errorMarker(page)).toHaveCount(0);
    expect(fault.hits()).toBeGreaterThan(0);

    await fault.release();
    await expect(page.getByText(issueTitle)).toBeVisible({ timeout: 30000 });
  });

  test("clear removes the override", async ({ page }) => {
    const fault = await applyFault(page, "http-500", ISSUES_LIST);
    await fault.clear();
    await page.goto(`/${slug}/issues`, { waitUntil: "domcontentloaded" });
    await expect(page.getByText(issueTitle)).toBeVisible({ timeout: 30000 });
    expect(fault.hits()).toBe(0);
  });

  test("an unknown preset is refused", async ({ page }) => {
    expect(isFaultPreset("http-418")).toBe(false);
    // @ts-expect-error not a preset
    await expect(applyFault(page, "http-418", ISSUES_LIST)).rejects.toThrow(/unknown fault preset/);
  });
});

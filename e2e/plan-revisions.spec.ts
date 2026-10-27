import { _electron, expect, test, type Page } from "@playwright/test";
import { mkdir, writeFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { resolve } from "node:path";
import { TestApiClient } from "./fixtures";

const apiURL = process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;
const lines = Array.from({ length: 360 }, (_, index) => index === 0 ? "# Revision viewer rollout" : `- Step ${index}: Validate workspace permissions, keep the document readable, and retain immutable attachment provenance.`);
const original = lines.join("\n");
const revised = lines.with(8, "- Require explicit selection when two uploads have the same revision number.").with(309, "- Ship the compact toolbar on web and desktop. <script>window.planRevisionInjected = true</script>").join("\n");

async function expectPressedMode(page: Page, mode: "Split" | "Unify") {
  await page.mouse.move(0, 0);
  const pressed = page.getByRole("button", { name: mode, exact: true });
  const unpressed = page.getByRole("button", { name: mode === "Split" ? "Unify" : "Split", exact: true });
  await expect(pressed).toHaveAttribute("aria-pressed", "true");
  await expect(unpressed).toHaveAttribute("aria-pressed", "false");
  await expect(unpressed).toHaveCSS("background-color", "rgba(0, 0, 0, 0)");
  await expect(pressed).not.toHaveCSS("background-color", "rgba(0, 0, 0, 0)");
}

async function expectFocusedChange(page: Page, number: number) {
  const section = page.getByRole("region", { name: `Change ${number} / 2`, exact: true });
  await expect(section).toBeFocused();
  await expect(section).toHaveCSS("outline-style", "solid");
  await expect(section).toHaveCSS("outline-width", "2px");
  await expect(section).toHaveCSS("outline-offset", "-2px");
  await expect(section).not.toHaveCSS("outline-color", "rgba(0, 0, 0, 0)");
}

test.describe("plan revision viewer", () => {
  let author: TestApiClient;
  let workspace: { id: string; slug: string };
  let issue: { id: string };
  let current: { id: string };
  let base: { id: string };

  test.beforeEach(async ({ baseURL }, testInfo) => {
    const baseline = testInfo.title.startsWith("baseline captures");
    test.skip(baseline !== (process.env.PLAN_BASELINE_CAPTURE === "1"), "Separate baseline capture run");
    test.skip(testInfo.title.startsWith("desktop captures") && process.env.PLAN_DESKTOP_CAPTURE !== "1", "Explicit desktop capture only");
    const local = (value: string) => ["localhost", "127.0.0.1", "[::1]"].includes(new URL(value).hostname);
    test.skip(!local(baseURL!) || !local(apiURL), "Local fixtures only");
    const suffix = `${Date.now()}-${test.info().workerIndex}`;
    author = new TestApiClient();
    await author.login(`plan-viewer-${suffix}@multica.ai`, "Plan reviewer");
    workspace = await author.ensureWorkspace("Plan viewer", `plan-viewer-${suffix}`);
    await author.markUserOnboarded();
    issue = await author.createIssue("Plan revision rollout");
    const upload = async (filename: string, text: string) => {
      const body = new FormData();
      body.append("file", new Blob([text], { type: "text/markdown" }), filename);
      body.append("issue_id", issue.id);
      const response = await fetch(`${apiURL}/api/upload-file`, {
        method: "POST", headers: { Authorization: `Bearer ${author.getToken()}`, "X-Workspace-ID": workspace.id }, body,
      });
      expect(response.status, await response.clone().text()).toBe(200);
      return response.json();
    };
    await upload("plan-rev-1.md", "# Initial plan");
    base = await upload("plan-rev-2.md", original);
    current = await upload("plan-rev-3.md", revised);
  });

  test.afterEach(async () => {
    await author?.cleanup();
    await author?.deleteWorkspace();
  });

  async function open(page: Page) {
    await page.addInitScript((token) => localStorage.setItem("multica_token", token!), author.getToken());
    await page.goto(`/${workspace.slug}/attachments/${current.id}/preview`, { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("revision-document")).toBeVisible({ timeout: 120_000 });
  }

  test("reads, compares, navigates and downloads at desktop and phone widths", async ({ page }, testInfo) => {
    test.setTimeout(240_000);
    await page.setViewportSize({ width: 1440, height: 900 });
    await open(page);
    const capture = async (name: string) => {
      const path = testInfo.outputPath(`${name}.png`);
      await page.screenshot({ path, animations: "disabled" });
      await testInfo.attach(name, { path, contentType: "image/png" });
    };
    const diff = page.getByRole("switch", { name: "Diff", exact: true });
    await expect(diff).not.toBeChecked();
    await capture("web-1440-off");
    await expect(diff).toBeEnabled();
    await diff.click();
    await expect(page.getByTestId("revision-diff")).toHaveAttribute("data-mode", "unify");
    await expect(diff).toBeChecked();
    await expect(page.getByText("Change 1 / 2", { exact: true })).toBeVisible();
    await expectPressedMode(page, "Unify");
    await capture("web-1440-unify");
    await page.getByRole("button", { name: "Next change", exact: true }).click();
    await expect(page.getByText("Change 2 / 2", { exact: true })).toBeVisible();
    await expect(page.getByText(/Ship the compact toolbar/)).toBeVisible();
    await expectFocusedChange(page, 2);
    await expect(page.getByRole("button", { name: "Next change", exact: true })).toBeInViewport({ ratio: 1 });
    await expect(page.getByText(/Ship the compact toolbar/)).toBeInViewport({ ratio: 1 });
    await capture("web-1440-next");
    expect(await page.evaluate(() => "planRevisionInjected" in window)).toBe(false);
    await page.getByRole("button", { name: "Split", exact: true }).click();
    await expect(page.getByTestId("revision-diff")).toHaveAttribute("data-mode", "split");
    await expectPressedMode(page, "Split");
    for (const row of await page.getByTestId("revision-split-row").all()) {
      const cells = await row.locator(":scope > div").all();
      const bounds = await Promise.all(cells.map((cell) => cell.boundingBox()));
      expect(bounds[0]?.height).toBe(bounds[1]?.height);
      expect(bounds[0]?.y).toBe(bounds[1]?.y);
    }
    await capture("web-1440-split");
    await page.getByRole("button", { name: "Previous change", exact: true }).click();
    await expectFocusedChange(page, 1);
    await capture("web-1440-previous");
    await page.getByRole("combobox", { name: "Compare with", exact: true }).click();
    await expect(page.getByRole("option", { name: /plan-rev-2.md/ })).toBeVisible();
    await capture("web-1440-base");
    await page.getByRole("option", { name: /plan-rev-2.md/ }).click();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("revision-document")).toBeVisible({ timeout: 120_000 });
    await capture("web-390-off");
    await diff.click();
    await expect(page.getByTestId("revision-diff")).toHaveAttribute("data-mode", "unify");
    await expect(page.getByText("Change 1 / 2", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Next change", exact: true })).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await capture("web-390-unify");
    await page.getByRole("button", { name: "Next change", exact: true }).click();
    await expectFocusedChange(page, 2);
    await expect(page.getByRole("button", { name: "Next change", exact: true })).toBeInViewport({ ratio: 1 });
    await expect(diff).toBeInViewport({ ratio: 1 });
    await expect(page.getByText("Change 2 / 2", { exact: true })).toBeInViewport({ ratio: 1 });
    await expect(page.getByRole("button", { name: "Previous change", exact: true })).toBeInViewport({ ratio: 1 });
    await expect(page.getByRole("button", { name: "More options", exact: true })).toBeInViewport({ ratio: 1 });
    await expect(page.getByText(/Ship the compact toolbar/)).toBeInViewport({ ratio: 1 });
    expect(await page.getByTestId("revision-content").evaluate((pane) => pane.scrollTop)).toBeGreaterThan(0);
    await capture("web-390-next");
    await page.getByRole("button", { name: "More options", exact: true }).click();
    await expect(page.getByRole("menuitemradio", { name: "Split", exact: true })).toBeVisible();
    await expect(page.getByRole("menuitemradio", { name: /plan-rev-2.md/ })).toContainText("ICT · Plan reviewer");
    await capture("web-390-base");
    const downloaded = page.waitForEvent("download");
    await page.getByRole("menuitem", { name: "Download", exact: true }).click();
    expect((await downloaded).suggestedFilename()).toBe("plan-rev-3.md");
  });

  test("enforces membership for discovery, current metadata and either content body", async ({ request }, testInfo) => {
    const outsider = new TestApiClient();
    await outsider.login(`plan-outsider-${Date.now()}@multica.ai`, "Outsider");
    const otherWorkspace = await outsider.ensureWorkspace("Other workspace", `plan-outsider-${Date.now()}`);
    const endpoints = [`/api/issues/${issue.id}/attachments`, `/api/attachments/${current.id}`, `/api/attachments/${current.id}/content`, `/api/attachments/${base.id}/content`];
    const results: { endpoint: string; sameWorkspace: number; noAuth: number; foreignWorkspace: number; notMember: number }[] = [];
    try {
      for (const endpoint of endpoints) {
        const allowed = await request.get(`${apiURL}${endpoint}`, { headers: { Authorization: `Bearer ${author.getToken()}`, "X-Workspace-ID": workspace.id } });
        const noAuth = await request.get(`${apiURL}${endpoint}`, { headers: { "X-Workspace-ID": workspace.id } });
        const foreign = await request.get(`${apiURL}${endpoint}`, { headers: { Authorization: `Bearer ${outsider.getToken()}`, "X-Workspace-ID": otherWorkspace.id } });
        const notMember = await request.get(`${apiURL}${endpoint}`, { headers: { Authorization: `Bearer ${outsider.getToken()}`, "X-Workspace-ID": workspace.id } });
        results.push({ endpoint, sameWorkspace: allowed.status(), noAuth: noAuth.status(), foreignWorkspace: foreign.status(), notMember: notMember.status() });
        expect(allowed.status()).toBe(200);
        expect(noAuth.status()).toBe(401);
        expect(foreign.status()).toBe(404);
        expect(notMember.status()).toBe(404);
      }
    } finally {
      const path = testInfo.outputPath("permission-checks.json");
      await writeFile(path, JSON.stringify(results, null, 2));
      await testInfo.attach("permission-checks", { path, contentType: "application/json" });
      await outsider.deleteWorkspace();
    }
  });

  test("desktop captures use the real isolated Electron client", async ({}, testInfo) => {
    test.skip(process.env.PLAN_DESKTOP_CAPTURE !== "1", "Explicit desktop capture only");
    const home = testInfo.outputPath("home");
    await mkdir(`${home}/.multica`, { recursive: true });
    await writeFile(`${home}/.multica/desktop_prefs.json`, JSON.stringify({ autoStart: false, autoStop: false }));
    const env = Object.fromEntries(Object.entries(process.env).filter(([key, value]) => !key.startsWith("MULTICA_") && value !== undefined)) as Record<string, string>;
    Object.assign(env, { HOME: home, XDG_CONFIG_HOME: `${home}/config`, DESKTOP_APP_SUFFIX: "CHE2124", ELECTRON_DISABLE_SECURITY_WARNINGS: "true" });
    const app = await _electron.launch({
      executablePath: createRequire(resolve("apps/desktop/package.json"))("electron"),
      args: [resolve("apps/desktop"), "--ozone-platform=headless", "--no-sandbox", "--disable-gpu"], env,
    });
    try {
      const page = await app.firstWindow();
      await page.waitForLoadState("domcontentloaded");
      await page.evaluate(({ token, slug, path }) => {
        localStorage.setItem("multica_token", token!);
        localStorage.setItem("multica_tabs", JSON.stringify({ version: 5, state: {
          activeWorkspaceSlug: slug, byWorkspace: { [slug]: {
            activeTabId: "capture", tabs: [{ id: "capture", url: path, resourceKey: path, title: "plan-rev-3.md", pinned: false, history: { stack: [path], index: 0 }, memento: { scroll: {}, view: {} } }],
            browsingHistory: [path], browsingHistoryTitles: {}, recentTabIds: [],
          } },
        } }));
      }, { token: author.getToken(), slug: workspace.slug, path: `/${workspace.slug}/attachments/${current.id}/preview` });
      await page.reload();
      await app.evaluate(({ BrowserWindow }) => { const window = BrowserWindow.getAllWindows()[0]!; window.setSize(1440, 900); });
      await expect(page.getByTestId("revision-document")).toBeVisible({ timeout: 120_000 });
      const capture = async (name: string) => {
        const path = testInfo.outputPath(`${name}.png`);
        await page.screenshot({ path, animations: "disabled" });
        await testInfo.attach(name, { path, contentType: "image/png" });
      };
      await capture("desktop-1440-off");
      await page.getByRole("switch", { name: "Diff", exact: true }).click();
      await expect(page.getByTestId("revision-diff")).toHaveAttribute("data-mode", "unify");
      await expectPressedMode(page, "Unify");
      await capture("desktop-1440-unify");
      await page.getByRole("button", { name: "Next change", exact: true }).click();
      await expectFocusedChange(page, 2);
      await capture("desktop-1440-next");
      await page.getByRole("button", { name: "Split", exact: true }).click();
      await expectPressedMode(page, "Split");
      await capture("desktop-1440-split");
      await page.getByRole("button", { name: "Previous change", exact: true }).click();
      await expectFocusedChange(page, 1);
      await capture("desktop-1440-previous");
      await page.getByRole("combobox", { name: "Compare with", exact: true }).click();
      await expect(page.getByRole("option", { name: /plan-rev-2.md/ })).toBeVisible();
      await capture("desktop-1440-base");
    } finally {
      await app.close();
    }
  });

  test("baseline captures use the pre-change web checkout", async ({ page }, testInfo) => {
    test.skip(process.env.PLAN_BASELINE_CAPTURE !== "1", "Explicit baseline capture only");
    test.setTimeout(240_000);
    await page.addInitScript((token) => localStorage.setItem("multica_token", token!), author.getToken());
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 900 });
      await page.goto(`/${workspace.slug}/attachments/${current.id}/preview`, { waitUntil: "domcontentloaded" });
      await expect(page.getByRole("heading", { name: "Revision viewer rollout", exact: true })).toBeVisible({ timeout: 120_000 });
      await expect(page.getByRole("switch", { name: "Diff", exact: true })).toHaveCount(0);
      const path = testInfo.outputPath(`web-${width}-before.png`);
      await page.screenshot({ path, animations: "disabled" });
      await testInfo.attach(`web-${width}-before`, { path, contentType: "image/png" });
    }
  });
});

import { test, expect, type Page } from "@playwright/test";
import { createTestApi, waitForPageText } from "./helpers";
import type { TestApiClient } from "./fixtures";

test.describe("Issue relations graph", () => {
  let api: TestApiClient;

  test.beforeEach(async ({ page }) => {
    page.on("pageerror", (error) => console.log("Browser error:", error.message));
    api = await createTestApi();
  });
  test.afterEach(async ({ page }, info) => {
    if (info.status !== info.expectedStatus) {
      await page.screenshot({ path: info.outputPath("failure.png"), fullPage: true });
    }
    await api?.cleanup();
  });

  async function signIn(page: Page) {
    const [workspace] = await api.getWorkspaces();
    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token!);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken());
    return workspace!.slug;
  }

  test("prefetches on intent, escapes titles, changes depth and navigates native links", async ({ page }, info) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    const root = await api.createIssue("Relations root", { status: "todo" });
    const child = await api.createIssue('Relations "quoted" <img src=x onerror=window.relationsAttack=1>', { status: "todo", parent_issue_id: root.id });
    const grandchild = await api.createIssue("Relations grandchild", { status: "todo", parent_issue_id: child.id });
    const slug = await signIn(page);
    const relationsRequests: string[] = [];
    const graphChunks: string[] = [];
    page.on("request", (request) => {
      if (request.url().includes(`/issues/${root.id}/relations`)) relationsRequests.push(request.url());
      if (request.resourceType() === "script" && /mermaid/i.test(request.url())) graphChunks.push(request.url());
    });
    await page.goto(`/${slug}/issues/${root.identifier}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, root.title);
    const button = page.getByRole("button", { name: "Relations graph", exact: true });
    await expect(button).toBeVisible();
    expect(relationsRequests).toEqual([]);
    expect(graphChunks).toEqual([]);
    await page.screenshot({ path: info.outputPath("before-1440.png"), fullPage: true });
    await button.click();
    const dialog = page.getByRole("dialog", { name: "Relations graph", exact: true });
    const childNode = dialog.getByRole("link", { name: `${child.identifier}: ${child.title} (todo)`, exact: true });
    await expect(childNode).toBeVisible({ timeout: 45_000 });
    await expect(childNode).toHaveAttribute("href", new RegExp(`/issues/${child.id}$`));
    await expect(childNode).toHaveAttribute("title", `${child.identifier}: ${child.title} (todo)`);
    expect(relationsRequests).toHaveLength(1);
    expect(new URL(relationsRequests[0]!).searchParams.get("depth")).toBe("1");
    expect(new URL(relationsRequests[0]!).searchParams.has("include")).toBe(false);
    expect(graphChunks).toEqual([]);
    await expect(dialog.locator('[aria-current="page"]')).toHaveCount(1);
    await expect(dialog.locator("img,script,foreignObject")).toHaveCount(0);
    expect(await page.evaluate(() => (window as unknown as { relationsAttack?: number }).relationsAttack)).toBeUndefined();
    await expect(dialog.getByRole("link", { name: `${grandchild.identifier}: ${grandchild.title} (todo)`, exact: true })).toHaveCount(0);
    await dialog.getByLabel("Depth", { exact: true }).selectOption("2");
    await expect(dialog.getByRole("link", { name: `${grandchild.identifier}: ${grandchild.title} (todo)`, exact: true })).toBeVisible();
    await page.screenshot({ path: info.outputPath("after-1440.png"), fullPage: true });
    await dialog.getByRole("link", { name: `${child.identifier}: ${child.title} (todo)`, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/(${child.id}|${child.identifier})$`), { timeout: 45_000 });
    await expect(dialog).toHaveCount(0);
  });

  test("shows a first-frame skeleton, loads mentions only on demand and reuses fresh data", async ({ page }) => {
    const root = await api.createIssue("Relations loading", { status: "todo" });
    const mentioned = await api.createIssue("Lazy mentioned issue", { status: "todo" });
    await api.createComment(root.id, `[Mention](mention://issue/${mentioned.id})`);
    const slug = await signIn(page);
    await page.addInitScript(() => localStorage.setItem("multica_relations_filter", JSON.stringify({ state: { hidden: ["mention"] }, version: 0 })));
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const requests: string[] = [];
    await page.route(`**/api/issues/${root.id}/relations?*`, async (route) => {
      requests.push(route.request().url());
      await gate;
      await route.continue();
    });
    await page.goto(`/${slug}/issues/${root.identifier}`, { waitUntil: "domcontentloaded" });
    const button = page.getByRole("button", { name: "Relations graph", exact: true });
    await button.hover();
    await expect.poll(() => requests.length).toBe(1);
    await button.evaluate((element: HTMLButtonElement) => element.click());
    const dialog = page.getByRole("dialog", { name: "Relations graph", exact: true });
    expect(await page.evaluate(() => new Promise<boolean>((resolve) => requestAnimationFrame(() => resolve(!!document.querySelector('[role="status"][aria-label="Loading relations…"]')))))).toBe(true);
    await expect(dialog.getByRole("status", { name: "Loading relations…" })).toBeVisible();
    release();
    await expect(dialog.getByRole("link", { name: `${root.identifier}: ${root.title} (todo)` })).toBeVisible();
    expect(requests.every((url) => !new URL(url).searchParams.has("include"))).toBe(true);
    await dialog.getByRole("button", { name: /mentions/i }).click();
    await expect(dialog.getByRole("link", { name: `${mentioned.identifier}: ${mentioned.title} (todo)` })).toBeVisible();
    expect(requests.filter((url) => new URL(url).searchParams.get("include") === "mentions")).toHaveLength(1);
    await dialog.getByRole("button", { name: /mentions/i }).click();
    await expect(dialog.getByRole("link", { name: `${mentioned.identifier}: ${mentioned.title} (todo)` })).toHaveCount(0);
    await page.keyboard.press("Escape");
    const count = requests.length;
    await button.click();
    await expect(dialog.getByRole("link", { name: `${root.identifier}: ${root.title} (todo)` })).toBeVisible();
    expect(requests).toHaveLength(count);
  });

  test("supports a narrow sidebar, load error recovery, an empty graph and focus restoration", async ({ page }, info) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const root = await api.createIssue("Relations empty", { status: "todo" });
    const slug = await signIn(page);
    await page.goto(`/${slug}/issues/${root.identifier}`, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Toggle right sidebar" }).click();
    const button = page.getByRole("button", { name: "Relations graph", exact: true });
    await expect(button).toBeVisible();
    await page.screenshot({ path: info.outputPath("before-390.png"), fullPage: true });
    const pattern = `**/api/issues/${root.id}/relations?*`;
    await page.route(pattern, (route) => route.fulfill({ status: 500, contentType: "application/json", body: '{"error":"test failure"}' }));
    await button.click();
    const dialog = page.getByRole("dialog", { name: "Relations graph", exact: true });
    await expect(dialog.getByRole("alert")).toContainText("Could not load relations.");
    await page.unroute(pattern);
    await dialog.getByRole("button", { name: "Try again" }).click();
    await expect(dialog.getByText("No related issues.")).toBeVisible();
    await expect(dialog.getByRole("link", { name: `${root.identifier}: ${root.title} (todo)`, exact: true })).toBeVisible();
    await page.screenshot({ path: info.outputPath("after-390.png"), fullPage: true });
    const bounds = await dialog.boundingBox();
    expect(bounds!.width).toBeLessThanOrEqual(390);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(button).toBeFocused();
  });
});

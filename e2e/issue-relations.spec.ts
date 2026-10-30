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

  test("loads only on click, escapes titles, changes depth and navigates nodes", async ({ page }, info) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    const root = await api.createIssue("Relations root", { status: "todo" });
    const child = await api.createIssue('Relations "quoted" <img src=x onerror=window.relationsAttack=1>', { status: "todo", parent_issue_id: root.id });
    const grandchild = await api.createIssue("Relations grandchild", { status: "todo", parent_issue_id: child.id });
    const slug = await signIn(page);
    const relationsRequests: string[] = [];
    const graphChunks: string[] = [];
    page.on("request", (request) => {
      if (request.url().includes(`/issues/${root.id}/relations`)) relationsRequests.push(request.url());
      if (request.resourceType() === "script" && request.url().includes("issue-relations-dialog")) graphChunks.push(request.url());
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
    expect((await childNode.textContent())?.replace(/\s/g, "")).toBe(`${child.identifier}: ${child.title} (todo)`.replace(/\s/g, ""));
    expect(relationsRequests).toHaveLength(1);
    expect(new URL(relationsRequests[0]!).searchParams.get("depth")).toBe("1");
    await expect(dialog.locator('[aria-current="page"]')).toHaveCount(1);
    await expect(dialog.locator("img,script,foreignObject")).toHaveCount(0);
    expect(await page.evaluate(() => (window as unknown as { relationsAttack?: number }).relationsAttack)).toBeUndefined();
    await expect(dialog.getByRole("link", { name: `${grandchild.identifier}: ${grandchild.title} (todo)`, exact: true })).toHaveCount(0);
    await dialog.getByLabel("Depth", { exact: true }).selectOption("2");
    await expect(dialog.getByRole("link", { name: `${grandchild.identifier}: ${grandchild.title} (todo)`, exact: true })).toBeVisible();
    await page.screenshot({ path: info.outputPath("after-1440.png"), fullPage: true });
    await dialog.getByRole("link", { name: `${child.identifier}: ${child.title} (todo)`, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/(${child.id}|${child.identifier})$`));
    await expect(dialog).toHaveCount(0);
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

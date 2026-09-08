import { test, expect } from "@playwright/test";
import { loginAsDefault, waitForPageText } from "./helpers";

const ROUTE_CHANGE_TIMEOUT = 30000;

test.describe("Navigation", () => {
  test.beforeEach(async ({ page }) => {
    await loginAsDefault(page);
    await page.waitForLoadState("networkidle");
  });

  test("sidebar navigation works", async ({ page }) => {
    await page.getByRole("link", { name: "Inbox" }).click();
    await expect(page).toHaveURL(/\/inbox/, { timeout: ROUTE_CHANGE_TIMEOUT });
    await waitForPageText(page, "Inbox");
    // Each destination renames the browser tab after itself (MUL-6222).
    await expect(page).toHaveTitle("Inbox | Multica");

    await page.getByRole("link", { name: "Agents" }).click();
    await expect(page).toHaveURL(/\/agents/, { timeout: ROUTE_CHANGE_TIMEOUT });
    await waitForPageText(page, "Agents");
    await expect(page).toHaveTitle("Agents | Multica");

    await page.getByRole("link", { name: "Issues", exact: true }).click();
    await expect(page).toHaveURL(/\/issues/, { timeout: ROUTE_CHANGE_TIMEOUT });
    await waitForPageText(page, "Issues");
    await expect(page).toHaveTitle("Issues | Multica");
  });

  test("settings page loads via sidebar", async ({ page }) => {
    await page.getByRole("link", { name: "Settings", exact: true }).click();
    await expect(page).toHaveURL(/\/settings/, { timeout: ROUTE_CHANGE_TIMEOUT });
    await waitForPageText(page, "Settings");

    // Settings navigation renders as links inside a labelled <nav> (AppLink ->
    // <a href>), not as a tablist. Assert the real role, the destination each
    // link points at, and that following one actually renders that page.
    const settingsNav = page.getByRole("navigation", { name: "Settings" });
    const generalLink = settingsNav.getByRole("link", { name: "General" });
    const membersLink = settingsNav.getByRole("link", { name: "Members" });

    await expect(generalLink).toBeVisible();
    await expect(membersLink).toBeVisible();
    await expect(generalLink).toHaveAttribute("href", /tab=workspace/);
    await expect(membersLink).toHaveAttribute("href", /tab=members/);

    await membersLink.click();
    await expect(page).toHaveURL(/tab=members/, {
      timeout: ROUTE_CHANGE_TIMEOUT,
    });
    await expect(membersLink).toHaveAttribute("aria-current", "page");
    await waitForPageText(page, "Members");
  });

  test("agents page shows agent list", async ({ page }) => {
    await page.getByRole("link", { name: "Agents" }).click();
    await expect(page).toHaveURL(/\/agents/, { timeout: ROUTE_CHANGE_TIMEOUT });
    await waitForPageText(page, "Agents");

    // Should show "Agents" heading
    await expect(page.locator("text=Agents").first()).toBeVisible();
  });
});

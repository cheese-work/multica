import { test, expect } from "@playwright/test";
import { loginAsDefault, waitForHydrated, waitForPageText } from "./helpers";

const ROUTE_CHANGE_TIMEOUT = 30000;

test.describe("Navigation", () => {
  test.beforeEach(async ({ page }) => {
    await loginAsDefault(page);
    // "networkidle" never arrives: the app keeps polling and holding realtime
    // connections. Wait for the sidebar the tests click instead.
    await waitForHydrated(page.getByRole("link", { name: "Inbox" }));
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

    await expect(page.getByRole("link", { name: "General" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Members" })).toBeVisible();
  });

  test("agents page shows agent list", async ({ page }) => {
    await page.getByRole("link", { name: "Agents" }).click();
    await expect(page).toHaveURL(/\/agents/, { timeout: ROUTE_CHANGE_TIMEOUT });
    await waitForPageText(page, "Agents");

    // Should show "Agents" heading
    await expect(page.locator("text=Agents").first()).toBeVisible();
  });
});

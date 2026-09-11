import { test, expect } from "@playwright/test";
import { createTestApi, loginAsDefault, waitForPageText } from "./helpers";

// A settings tab compiles and mounts on its first visit in dev mode, which can
// exceed the default 5s expect timeout.
const TAB_MOUNT_TIMEOUT = 30000;

async function enableComposioForTest(page: import("@playwright/test").Page) {
  await page.route("**/api/config", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        cdn_domain: "",
        cdn_signed: false,
        allow_signup: true,
        google_client_id: "",
        daemon_server_url: "",
        daemon_app_url: "",
        workspace_creation_disabled: false,
        vcs_integration_available: false,
        local_worktree_supported: false,
        agent_conversation_starters_supported: false,
        feature_flags: { composio_mcp_apps: true },
      }),
    }),
  );
}

test.describe("Settings", () => {
  // The workspace name field autosaves (useAutoSave, 650ms debounce, flushed on
  // blur) — there is no Save button. Assert the persisted write, not a control:
  // arm the PATCH response first, edit, blur to flush, then verify the response,
  // the sidebar without a refresh, and the value surviving a reload.
  test("updating workspace name autosaves and reflects in sidebar immediately", async ({
    page,
  }) => {
    const api = await createTestApi();
    const workspaceSlug = await loginAsDefault(page);

    // The authoritative original name comes from the fixture record, not from
    // ambiguous sidebar text, so restoration cannot drift.
    const workspaces = await api.getWorkspaces();
    const workspace = workspaces.find((item) => item.slug === workspaceSlug);
    if (!workspace) {
      throw new Error(`E2E workspace ${workspaceSlug} not found via the test API`);
    }
    const originalName = workspace.name;
    const newName = `Renamed WS ${Date.now()}`;

    await page.getByRole("link", { name: "Settings", exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/${workspaceSlug}/settings`));
    // The sidebar lands on the personal Profile tab; the workspace name lives
    // under Workspace > General.
    await page
      .getByRole("navigation", { name: "Settings" })
      .getByRole("link", { name: "General" })
      .click();
    await expect(page).toHaveURL(/tab=workspace/);

    const nameInput = page.locator('input[name="workspace-name"]');
    await expect(nameInput).toHaveValue(originalName);

    try {
      // Arm the assertion before the edit so the autosave PATCH cannot be missed.
      const saved = page.waitForResponse(
        (response) =>
          response.url().includes(`/api/workspaces/${workspace.id}`) &&
          response.request().method() === "PATCH" &&
          response.ok(),
      );

      await nameInput.fill(newName);
      await nameInput.blur(); // flushes the debounce; no fixed sleep
      const response = await saved;
      expect(((await response.json()) as { name: string }).name).toBe(newName);

      // Sidebar reflects the new name WITHOUT a page refresh.
      await expect(
        page.getByRole("button", { name: new RegExp(newName) }).first(),
      ).toBeVisible();

      // The change is persisted server-side, not just in the client cache:
      // reload straight into the workspace tab and re-read the field.
      await page.goto(`/${workspaceSlug}/settings?tab=workspace`, {
        waitUntil: "domcontentloaded",
      });
      const reloadedInput = page.locator('input[name="workspace-name"]');
      // Wait for the field itself before asserting its value.
      await expect(reloadedInput).toBeVisible({ timeout: TAB_MOUNT_TIMEOUT });
      await expect(reloadedInput).toHaveValue(newName);
    } finally {
      // Restore through the same proven autosave path so other tests are
      // unaffected even if an assertion above failed, then confirm server-side
      // that the fixture name is actually back.
      const current = await api.getWorkspaces();
      if (current.find((item) => item.id === workspace.id)?.name !== originalName) {
        await page.goto(`/${workspaceSlug}/settings?tab=workspace`, {
          waitUntil: "domcontentloaded",
        });
        const restoreInput = page.locator('input[name="workspace-name"]');
        const restored = page.waitForResponse(
          (response) =>
            response.url().includes(`/api/workspaces/${workspace.id}`) &&
            response.request().method() === "PATCH" &&
            response.ok(),
        );
        await restoreInput.fill(originalName);
        await restoreInput.blur();
        await restored;
      }
      const afterRestore = await api.getWorkspaces();
      expect(afterRestore.find((item) => item.id === workspace.id)?.name).toBe(
        originalName,
      );
    }
  });

  // Composio connect flow, fully mocked at the network boundary so it runs
  // without a configured COMPOSIO_API_KEY or a live Composio project. The
  // backend redirect is simulated by pointing the init endpoint's redirect_url
  // straight back at the settings page with ?connected=<slug> — exercising the
  // frontend's callback toast + connections refresh (MUL-3718) end to end.
  test("connecting a Composio toolkit shows a toast and refreshes the list", async ({
    page,
  }) => {
    // Config loads during the first authenticated page. Route it before login
    // so the default-off Composio feature flag is available at initialization.
    await enableComposioForTest(page);
    const workspaceSlug = await loginAsDefault(page);
    const settingsUrl = `/${workspaceSlug}/settings?tab=integrations&integration=composio`;

    // Stateful: connections is empty until the (mocked) connect flow lands.
    let connected = false;

    await page.route("**/api/integrations/composio/toolkits", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify([
          { slug: "notion", name: "Notion", connectable: true },
        ]),
      }),
    );

    await page.route("**/api/integrations/composio/connections", (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(
          connected
            ? [
                {
                  id: "conn-notion-1",
                  toolkit_slug: "notion",
                  status: "active",
                  connected_at: new Date().toISOString(),
                  last_used_at: null,
                },
              ]
            : [],
        ),
      });
    });

    await page.route("**/api/integrations/composio/connect/init", (route) => {
      // Composio would 302 through its hosted consent and back to our callback,
      // which emits CallbackRedirect's slug-less shape:
      // `/settings?tab=integrations&connected=<slug>`. The web proxy's
      // legacy-route redirect then prepends the last workspace slug, landing on
      // the real settings route. Mock that exact backend shape (NOT the final
      // slugged URL) so the test exercises the same redirect path real users hit.
      connected = true;
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          redirect_url: `/settings?tab=integrations&connected=notion`,
        }),
      });
    });

    await page.goto(settingsUrl, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, "Composio");

    // Notion starts disconnected → click Connect.
    await page.getByRole("button", { name: /^Connect$/ }).first().click();

    // Success toast from the simulated callback redirect.
    await expect(page.getByText("Connected").first()).toBeVisible({ timeout: 10000 });

    // List refreshed without a manual reload: the Notion card now offers
    // Disconnect, and the one-shot ?connected param has been stripped.
    await expect(
      page.getByRole("button", { name: /Disconnect/ }).first(),
    ).toBeVisible({ timeout: 10000 });
    await expect(page).not.toHaveURL(/connected=notion/);
  });
});

import "./env";

import { devices, expect, test, type Page, type Route } from "@playwright/test";
import { createTestApi } from "./helpers";
import type { TestApiClient } from "./fixtures";
import { HTTPS_ORIGIN, addVirtualAuthenticator, readOutcome, removeSeed, seedPasskey, serveOverHttps } from "./dm-budget-proposal-helpers";
import {
  ADDED_LINE,
  AFTER_CONTEXT,
  BEFORE_CONTEXT,
  REMOVED_LINE,
  UNCHANGED_LINE,
  codePoints,
  proposeContext,
  readChangeActivity,
  readContext,
  removeContextSeed,
  seedDmContext,
  sha256,
  type DmContextSeed,
} from "./dm-context-proposal-helpers";

// E8: an agent's chat task proposes `workspace.context.set` in a DM; the owner approves it with
// a passkey on a phone-sized viewport. Same harness as D8a/D8b. The server state is the verdict:
// the stored context and the activity row are read from the API and the database, never from the card.
test.use({ ...devices["Pixel 7"] });

const approveUrl = /\/api\/decision-actions\/[0-9a-f-]+\/approve$/;

interface CapturedApprove {
  url: string;
  headers: Record<string, string>;
  body: string | null;
}

let api: TestApiClient;
let seed: DmContextSeed;
let approveRequests: number;
let firstApprove: CapturedApprove | undefined;

async function openDm(page: Page) {
  const passkey = await seedPasskey(seed.userId);
  await addVirtualAuthenticator(page, passkey);
  await page.addInitScript((token) => {
    localStorage.setItem("multica_token", token);
    localStorage.setItem("multica:chat:isOpen", "false");
  }, api.getToken()!);
  await serveOverHttps(page.context(), async (route: Route) => {
    const request = route.request();
    if (request.method() === "POST" && approveUrl.test(new URL(request.url()).pathname)) {
      approveRequests++;
      firstApprove ??= { url: request.url(), headers: await request.allHeaders(), body: request.postData() };
    }
    return false;
  });
  await page.goto(`${HTTPS_ORIGIN}/${seed.workspaceSlug}/chat?session=${seed.chatSessionId}`, { waitUntil: "domcontentloaded" });
}

test.beforeEach(async ({}, testInfo) => {
  api = await createTestApi();
  seed = await seedDmContext(api, `${testInfo.workerIndex}-${Date.now().toString(36)}`);
  approveRequests = 0;
  firstApprove = undefined;
});

test.afterEach(async () => {
  await removeContextSeed(seed);
  await api.cleanup();
});

test.describe("E8: DM workspace context proposal on a phone viewport", () => {
  test("the card shows only the changed line, a passkey approves it, the stored context is the proposed text, a second approve is refused", async ({ page }) => {
    const actionId = await proposeContext(seed);
    expect(await readContext(api, seed)).toBe(BEFORE_CONTEXT);

    await openDm(page);
    // The counts sit above the tabs, from what the server stored: code points before and after, and the cap.
    const counts = page.getByTestId("config-change-counts");
    await expect(counts).toBeVisible({ timeout: 30_000 });
    await expect(counts).toHaveText(`Characters: ${codePoints(BEFORE_CONTEXT)} → ${codePoints(AFTER_CONTEXT)} of 10,000 allowed`);

    // The viewer is collapsed until a tab opens; the tab label counts one line added, one removed.
    const changes = page.getByTestId("config-change-tab-changes");
    await expect(changes).toContainText("+1 −1");
    await changes.click();
    const diff = page.getByTestId("config-change-panel");
    await expect(diff).toBeVisible();
    await expect(diff.locator("s", { hasText: "+" })).toHaveCount(1);
    await expect(diff.locator("s", { hasText: "−" })).toHaveCount(1);
    await expect(diff.locator("code", { hasText: ADDED_LINE })).toHaveCount(1);
    await expect(diff.locator("code", { hasText: REMOVED_LINE })).toHaveCount(1);
    await expect(diff.locator("code", { hasText: UNCHANGED_LINE })).toHaveCount(1);

    await page.getByRole("button", { name: "Approve with passkey" }).click();
    const outcome = page.getByRole("status").filter({ hasText: "Applied" });
    await expect(outcome).toBeVisible({ timeout: 30_000 });
    await expect(outcome).toHaveAttribute("data-state", "executed");

    // Server state: the context is the proposed text, the action is executed, one activity row names the change.
    expect(await readContext(api, seed)).toBe(AFTER_CONTEXT);
    expect((await readOutcome(api, seed, actionId)).state).toBe("executed");
    const activity = await readChangeActivity(seed);
    expect(activity).toHaveLength(1);
    expect(activity[0]).toMatchObject({
      field: "workspace.context",
      target_id: seed.workspaceId,
      before_sha256: sha256(BEFORE_CONTEXT),
      after_sha256: sha256(AFTER_CONTEXT),
      decision_action_id: actionId,
    });
    expect(approveRequests).toBe(1);

    // The card offers no second approve; a replay of the very same approve request is refused and changes nothing.
    await expect(page.getByRole("button", { name: "Approve with passkey" })).toHaveCount(0);
    expect(firstApprove).toBeDefined();
    const { headers, body, url } = firstApprove!;
    const replayHeaders = Object.fromEntries(Object.entries(headers).filter(([name]) => !["content-length", "host"].includes(name.toLowerCase())));
    const replay = await fetch(url, { method: "POST", headers: replayHeaders, body });
    expect(replay.ok).toBe(false);
    expect((await readOutcome(api, seed, actionId)).state).toBe("executed");
    expect(await readContext(api, seed)).toBe(AFTER_CONTEXT);
    expect(await readChangeActivity(seed)).toHaveLength(1);
  });
});

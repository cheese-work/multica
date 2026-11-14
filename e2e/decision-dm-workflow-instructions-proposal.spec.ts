import "./env";

import { devices, expect, test, type Page, type Route } from "@playwright/test";
import { createTestApi } from "./helpers";
import type { TestApiClient } from "./fixtures";
import {
  HTTPS_ORIGIN,
  addVirtualAuthenticator,
  readOutcome,
  removeSeed,
  seedPasskey,
  serveOverHttps,
} from "./dm-budget-proposal-helpers";
import {
  AFTER_TEXT,
  BEFORE_TEXT,
  HUMAN_EDIT_TEXT,
  OTHER_STATUS_KEY,
  OTHER_STEP_TEXT,
  STATUS_KEY,
  humanEditsStep,
  instructionsOf,
  proposeInstructions,
  readWorkflow,
  removeWorkflowSeed,
  seedDmWorkflow,
  type DmWorkflowSeed,
} from "./dm-workflow-proposal-helpers";

// D8b: an agent's chat task proposes `workflow.step.instructions.set` in a DM; the owner
// approves it with a passkey on a phone-sized viewport. Same harness as D8a. The server
// state is the verdict: the stored instructions are read back from the API, never from the card.
test.use({ ...devices["Pixel 7"] });

const approveUrl = /\/api\/decision-actions\/[0-9a-f-]+\/approve$/;

let api: TestApiClient;
let seed: DmWorkflowSeed;
let approveRequests: number;

async function openDm(page: Page) {
  const passkey = await seedPasskey(seed.userId);
  await addVirtualAuthenticator(page, passkey);
  await page.addInitScript((token) => {
    localStorage.setItem("multica_token", token);
    localStorage.setItem("multica:chat:isOpen", "false");
  }, api.getToken()!);
  await serveOverHttps(page.context(), async (route: Route) => {
    const request = route.request();
    if (request.method() === "POST" && approveUrl.test(new URL(request.url()).pathname)) approveRequests++;
    return false;
  });
  await page.goto(`${HTTPS_ORIGIN}/${seed.workspaceSlug}/chat?session=${seed.chatSessionId}`, { waitUntil: "domcontentloaded" });
}

test.beforeEach(async ({}, testInfo) => {
  api = await createTestApi();
  seed = await seedDmWorkflow(api, `${testInfo.workerIndex}-${Date.now().toString(36)}`);
  approveRequests = 0;
});

test.afterEach(async () => {
  await removeWorkflowSeed(seed, removeSeed);
  await api.cleanup();
});

test.describe("D8b: DM workflow instructions proposal on a phone viewport", () => {
  test("the card shows the diff, a passkey approves it, the stored instructions are the proposed text", async ({ page }) => {
    const actionId = await proposeInstructions(api, seed);
    expect(instructionsOf(await readWorkflow(api, seed), STATUS_KEY)).toBe(BEFORE_TEXT);

    await openDm(page);
    // The viewer sits behind Details and shows the changed rows the server stored.
    await page.getByRole("button", { name: /Details/ }).click({ timeout: 30_000 });
    const diff = page.getByRole("list", { name: /^Changes to / });
    await expect(diff).toBeVisible();
    await expect(diff).toContainText("Run the tests.");

    await page.getByRole("button", { name: "Approve with passkey" }).click();
    const outcome = page.getByRole("status").filter({ hasText: "Executed" });
    await expect(outcome).toBeVisible({ timeout: 30_000 });
    await expect(outcome).toHaveAttribute("data-state", "executed");

    // Server state: the step holds the proposed text, the other step is untouched.
    const stored = await readWorkflow(api, seed);
    expect(instructionsOf(stored, STATUS_KEY)).toBe(AFTER_TEXT);
    expect(instructionsOf(stored, OTHER_STATUS_KEY)).toBe(OTHER_STEP_TEXT);
    expect((await readOutcome(api, seed, actionId)).state).toBe("executed");
    expect(approveRequests).toBe(1);
  });

  test("an edit after the proposal makes the approval stale_base; the instructions stay as edited", async ({ page }) => {
    const actionId = await proposeInstructions(api, seed);
    await humanEditsStep(seed);

    await openDm(page);
    await page.getByRole("button", { name: "Approve with passkey" }).click();
    const outcome = page.getByRole("status").filter({ hasText: "The target changed after the request. Ask again." });
    await expect(outcome).toBeVisible({ timeout: 30_000 });
    await expect(outcome).toHaveAttribute("data-state", "failed");

    // Server state: refused as stale_base; neither the proposal nor the old text was written.
    const server = await readOutcome(api, seed, actionId);
    expect(server.state).toBe("failed");
    expect(server.error_code).toBe("stale_base");
    const stored = await readWorkflow(api, seed);
    expect(instructionsOf(stored, STATUS_KEY)).toBe(HUMAN_EDIT_TEXT);
    expect(instructionsOf(stored, OTHER_STATUS_KEY)).toBe(OTHER_STEP_TEXT);
    expect(approveRequests).toBe(1);
  });
});

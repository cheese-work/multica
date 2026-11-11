import "./env";

import { devices, expect, test, type Page, type Route } from "@playwright/test";
import { createTestApi } from "./helpers";
import type { TestApiClient } from "./fixtures";
import {
  BASE_CAP,
  HTTPS_ORIGIN,
  RAISED_CAP,
  addVirtualAuthenticator,
  countRaisedRows,
  proposeRaise,
  readCap,
  readOutcome,
  removeSeed,
  seedDmBudgetIssue,
  seedPasskey,
  serveOverHttps,
  withDb,
  type DmBudgetSeed,
} from "./dm-budget-proposal-helpers";

// D8a: an agent's chat task proposes `issue.run_budget.raise` in a DM; the owner
// approves it with a passkey on a phone-sized viewport. Chromium's virtual
// authenticator is the passkey. The server state is the verdict: the card text
// only shows what the server already decided.
test.use({ ...devices["Pixel 7"] });

const approveUrl = /\/api\/decision-actions\/[0-9a-f-]+\/approve$/;

let api: TestApiClient;
let seed: DmBudgetSeed;
let actionId: string;
let approveRequests: number;
let loseNextApproveResponse: boolean;

async function enrollPasskey(page: Page) {
  const passkey = await seedPasskey(seed.userId);
  await addVirtualAuthenticator(page, passkey);
}

async function openDm(page: Page) {
  await page.addInitScript((token) => {
    localStorage.setItem("multica_token", token);
    localStorage.setItem("multica:chat:isOpen", "false");
  }, api.getToken()!);
  await serveOverHttps(page.context(), async (route: Route) => {
    const request = route.request();
    if (request.method() !== "POST" || !approveUrl.test(new URL(request.url()).pathname)) return false;
    approveRequests++;
    if (!loseNextApproveResponse) return false;
    loseNextApproveResponse = false;
    // The request reaches the server and is decided there; only the answer is lost.
    await route.fetch();
    await route.abort("connectionreset");
    return true;
  });
  await page.goto(`${HTTPS_ORIGIN}/${seed.workspaceSlug}/chat?session=${seed.chatSessionId}`, { waitUntil: "domcontentloaded" });
}

test.beforeEach(async ({}, testInfo) => {
  api = await createTestApi();
  seed = await seedDmBudgetIssue(api, `${testInfo.workerIndex}-${Date.now().toString(36)}`);
  approveRequests = 0;
  loseNextApproveResponse = false;
});

test.afterEach(async () => {
  await removeSeed(seed);
  await api.cleanup();
});

test.describe("D8a: DM budget proposal on a phone viewport", () => {
  test("a proposed raise appears in the DM, a passkey approves it, the cap and the card agree", async ({ page }) => {
    actionId = await proposeRaise(seed);
    expect(await readCap(api, seed)).toEqual({ limit: BASE_CAP, source: "workflow" });

    await enrollPasskey(page);
    await openDm(page);
    const approve = page.getByRole("button", { name: "Approve with passkey" });
    await expect(approve).toBeVisible({ timeout: 30_000 });
    await approve.click();

    const outcome = page.getByRole("status").filter({ hasText: "Cap applied" });
    await expect(outcome).toBeVisible({ timeout: 30_000 });
    await expect(outcome).toHaveAttribute("data-state", "executed");
    await expect(outcome).toContainText(`${BASE_CAP} → ${RAISED_CAP}`);

    // Server state: the cap persisted, the action is executed with the outcome token, one activity row.
    expect(await readCap(api, seed)).toEqual({ limit: RAISED_CAP, source: "issue" });
    const server = await readOutcome(api, seed, actionId);
    expect(server.state).toBe("executed");
    expect(server.result).toMatchObject({ from: BASE_CAP, to: RAISED_CAP, outcome: "cap_applied" });
    expect(await countRaisedRows(seed.issueId)).toEqual({ activity: 1, ledger: 1 });
    expect(approveRequests).toBe(1);
  });

  test("a lost approve response is read back; the raise runs exactly once", async ({ page }) => {
    actionId = await proposeRaise(seed);
    loseNextApproveResponse = true;

    await enrollPasskey(page);
    await openDm(page);
    await page.getByRole("button", { name: "Approve with passkey" }).click();

    // The browser never saw the answer. The card reads the outcome back and offers no second approve.
    const outcome = page.getByRole("status").filter({ hasText: "Cap applied" });
    await expect(outcome).toBeVisible({ timeout: 30_000 });
    await expect(page.getByRole("button", { name: "Approve with passkey" })).toHaveCount(0);

    expect(approveRequests).toBe(1);
    expect(await readCap(api, seed)).toEqual({ limit: RAISED_CAP, source: "issue" });
    expect((await readOutcome(api, seed, actionId)).state).toBe("executed");
    expect(await countRaisedRows(seed.issueId)).toEqual({ activity: 1, ledger: 1 });

    // A reload in the same tab still offers no second approve.
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByRole("status").filter({ hasText: "Cap applied" })).toBeVisible({ timeout: 30_000 });
    expect(approveRequests).toBe(1);
    expect(await countRaisedRows(seed.issueId)).toEqual({ activity: 1, ledger: 1 });
  });

  // The executor only ever writes `cap_applied`. `run_queued` and `still_blocked:<reason>` are
  // tokens the card already renders; their writer is not in the server yet, so the executed
  // result is stored directly and the test checks the API and the card agree on it.
  for (const [token, label] of [
    ["run_queued", "Run queued"],
    ["still_blocked:budget_exhausted", "Still blocked: budget_exhausted"],
  ] as const) {
    test(`outcome ${token} is served by the API and shown on the card`, async ({ page }) => {
      actionId = await proposeRaise(seed);
      await withDb((db) =>
        db.query(
          `UPDATE decision_action SET state = 'executed', executed_at = now(), approved_at = now(), approved_by_user_id = $2,
                  result = jsonb_build_object('from', $3::int, 'to', $4::int, 'outcome', $5::text)
           WHERE id = $1`,
          [actionId, seed.userId, BASE_CAP, RAISED_CAP, token],
        ),
      );
      expect((await readOutcome(api, seed, actionId)).result?.outcome).toBe(token);

      await openDm(page);
      const outcome = page.getByRole("status").filter({ hasText: label });
      await expect(outcome).toBeVisible({ timeout: 30_000 });
      await expect(outcome).toHaveAttribute("data-state", "executed");
      expect(approveRequests).toBe(0);
    });
  }
});

import "../../../../e2e/env";
import { devices, expect, test } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { mkdirSync } from "node:fs";
import { resolve } from "node:path";
import { createTestApi } from "../../../../e2e/helpers";
import type { TestApiClient } from "../../../../e2e/fixtures";
import {
  API_BASE,
  HTTPS_ORIGIN,
  addVirtualAuthenticator,
  removeSeed,
  seedDmBudgetIssue,
  seedPasskey,
  serveOverHttps,
  withDb,
  type DmBudgetSeed,
} from "../../../../e2e/dm-budget-proposal-helpers";

// verify-multica: drive one pending weekly run-guard decision card. CHE-6242 shipped a
// card whose preview the web client dropped ("Details unavailable", Approve disabled).
// Assertion: the card shows "Weekly run limit" and the Approve choice is enabled.
test.use({ ...devices["Pixel 7"] });

let api: TestApiClient;
let seed: DmBudgetSeed;

test.beforeEach(async () => {
  api = await createTestApi();
  seed = await seedDmBudgetIssue(api, `verify-${Date.now().toString(36)}`);
});

test.afterEach(async () => {
  await withDb((db) => db.query(`DELETE FROM weekly_run_guard_refusal WHERE issue_id = $1`, [seed.issueId]));
  await removeSeed(seed);
  await api.cleanup();
});

test("a pending weekly run-guard card shows Weekly run limit and an enabled Approve", async ({ page }) => {
  const evidence = resolve(process.env.EVIDENCE_DIR ?? "evidence");
  mkdirSync(evidence, { recursive: true });
  const step = (name: string) => console.log(`STEP ${new Date().toISOString()} ${name}`);

  step("seed: refusal row for the issue");
  const refusalId = randomUUID();
  await withDb((db) =>
    db.query(`INSERT INTO weekly_run_guard_refusal (id, workspace_id, issue_id, agent_id, runs) VALUES ($1, $2, $3, $4, 30)`,
      [refusalId, seed.workspaceId, seed.issueId, seed.agentId]),
  );
  step("propose: issue.run_guard.allow through the agent's task token");
  const response = await fetch(`${API_BASE}/api/decision-actions/proposals`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${seed.taskToken}`,
      "X-Workspace-ID": seed.workspaceId,
      "X-Agent-ID": seed.agentId,
      "X-Task-ID": seed.chatTaskId,
    },
    body: JSON.stringify({
      action_type: "issue.run_guard.allow", version: 1, issue_id: seed.issueId, reason: "verify-multica: run-guard card check.",
      params: { issue_id: seed.issueId, guard_refusal_id: refusalId, runs: 10 },
    }),
  });
  expect(response.ok, `proposal failed: ${response.status} ${await response.clone().text()}`).toBe(true);

  step("open: the DM with the card, signed in as the workspace owner");
  await addVirtualAuthenticator(page, await seedPasskey(seed.userId));
  await page.addInitScript((token) => {
    localStorage.setItem("multica_token", token);
    localStorage.setItem("multica:chat:isOpen", "false");
  }, api.getToken()!);
  await serveOverHttps(page.context());
  await page.goto(`${HTTPS_ORIGIN}/${seed.workspaceSlug}/chat?session=${seed.chatSessionId}`, { waitUntil: "domcontentloaded" });

  step("observe: card, preview field, Approve");
  await expect(page.getByTestId("decision-request-card")).toBeVisible({ timeout: 120_000 });
  const preview = page.getByTestId("decision-request-action-preview");
  await expect(preview.getByText("Weekly run limit")).toBeVisible();
  await expect(page.getByText("Details unavailable")).toHaveCount(0);
  await expect(page.getByRole("radio", { name: "Approve", exact: true })).toBeEnabled();
  await page.screenshot({ path: `${evidence}/decision-card-run-guard.png`, fullPage: true });
  step("PASS: Weekly run limit shown, Approve enabled");
  // The HTTPS bridge may still be fetching a chunk; let it finish quietly instead of failing the test at teardown.
  await page.context().unrouteAll({ behavior: "ignoreErrors" });
});

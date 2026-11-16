import "./env";

import { devices, expect, test } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { createTestApi } from "./helpers";
import type { TestApiClient } from "./fixtures";
import {
  API_BASE,
  HTTPS_ORIGIN,
  removeSeed,
  seedDmBudgetIssue,
  seedPasskey,
  addVirtualAuthenticator,
  serveOverHttps,
  withDb,
  type DmBudgetSeed,
} from "./dm-budget-proposal-helpers";

// CHE-6247: a pending card of every previewed action type shows its details and an enabled
// Approve button. CHE-6242 shipped a type the server previewed and the web client dropped:
// every card said "Details unavailable" with Approve disabled, and no test opened one.
// Keep the type list equal to PREVIEWED_ACTION_TYPES in packages/core/issues/decision-action-preview.ts
// (the Go contract test in server/internal/handler pins that file to the server). A type is skipped
// only with a named reason in SKIPPED.
test.use({ ...devices["Pixel 7"] });

const SKIPPED: Record<string, string> = {};

type Propose = (seed: DmBudgetSeed, api: TestApiClient) => Promise<Record<string, unknown>>;

// The card's reader must be allowed to use the copy's runtime, so the seeded one is theirs.
const runtimeOf = (seed: DmBudgetSeed) =>
  withDb(async (db) =>
    (await db.query<{ id: string }>(
      `UPDATE agent_runtime SET owner_id = $2 WHERE id = (SELECT runtime_id FROM agent WHERE id = $1) RETURNING id::text`,
      [seed.agentId, seed.userId],
    )).rows[0]!.id,
  );

const TYPES: Record<string, Propose> = {
  "issue.run_budget.raise": async (seed) => ({ issue_id: seed.issueId, new_cap: 25, window_id: seed.windowId }),
  "issue.run_budget.top_up": async (seed) => ({ issue_id: seed.issueId, window_id: seed.windowId }),
  "issue.workflow.set": async (seed) => {
    const wider = await withDb(async (db) =>
      (await db.query<{ id: string }>(
        `INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps, max_runs_per_issue, run_budget_since)
         VALUES ($1, $2, 'todo', '[]'::jsonb, 50, now() - interval '1 hour') RETURNING id::text`,
        [seed.workspaceId, `Wider ${randomUUID()}`],
      )).rows[0]!.id,
    );
    const binding = await withDb(async (db) =>
      (await db.query<{ id: string; start: string }>(
        `SELECT workflow_id::text AS id, to_char(w.run_budget_since AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') AS start
         FROM issue i JOIN issue_workflow w ON w.id = i.workflow_id WHERE i.id = $1`, [seed.issueId])).rows[0]!,
    );
    return {
      issue_id: seed.issueId,
      workflow_id: wider,
      explicit_workflow_id: wider,
      before: { workflow_id: binding.id, source: "issue", budgeted: true, cap: 10, start: binding.start.replace(/0+Z$/, "Z") },
      after: { workflow_id: wider, source: "issue", budgeted: true, cap: 50, start: binding.start.replace(/0+Z$/, "Z") },
    };
  },
  "issue.project.set": async (seed, api) => {
    // The seeded workflow has no steps, so the issue's status would not fit it after a move: follow the project's instead.
    await withDb((db) => db.query(`UPDATE issue SET workflow_id = NULL WHERE id = $1`, [seed.issueId]));
    return { issue_id: seed.issueId, project_id: (await api.createProject("CHE-6247 card")).id };
  },
  "agent.env.patch": async (seed) => ({ agent_id: seed.agentId, set: { CARD_RENDER_CHECK: "1" } }),
  "agent.copy": async (seed) => ({ source_agent_id: seed.agentId, name: "Card copy", runtime_id: await runtimeOf(seed), description: "" }),
  "issue.run_guard.allow": async (seed) => {
    const refusalId = randomUUID();
    await withDb((db) =>
      db.query(`INSERT INTO weekly_run_guard_refusal (id, workspace_id, issue_id, agent_id, runs) VALUES ($1, $2, $3, $4, 30)`,
        [refusalId, seed.workspaceId, seed.issueId, seed.agentId]),
    );
    return { issue_id: seed.issueId, guard_refusal_id: refusalId, runs: 10 };
  },
  "agent.pause": async (seed) => ({ agent_id: seed.agentId }),
  "agent.resume": async (seed) => ({ agent_id: seed.agentId }),
  "doop.deploy_lift": async () => ({ marker_value: "train-card-check", repo: "cheese-work/doop" }),
};

let api: TestApiClient;
let seed: DmBudgetSeed;

test.beforeEach(async ({}, testInfo) => {
  api = await createTestApi();
  seed = await seedDmBudgetIssue(api, `card-${testInfo.workerIndex}-${Date.now().toString(36)}`);
});

test.afterEach(async () => {
  await withDb(async (db) => {
    await db.query(`DELETE FROM weekly_run_guard_refusal WHERE issue_id = $1`, [seed.issueId]);
    await db.query(`DELETE FROM issue_workflow WHERE name LIKE 'Wider %' AND workspace_id = $1`, [seed.workspaceId]);
  });
  await removeSeed(seed);
  await api.cleanup();
});

// A type added to the client list without a case above would silently skip browser coverage.
// This one test seeds nothing, so it needs no database, but the hooks above run for it too.
test("every previewed client type has a browser case", () => {
  const source = readFileSync(resolve(process.cwd(), "packages/core/issues/decision-action-preview.ts"), "utf8");
  const list = /PREVIEWED_ACTION_TYPES = \[(.*?)\] as const/s.exec(source)?.[1] ?? "";
  const previewed = [...list.matchAll(/"([^"]+)"/g)].map((m) => m[1]).sort();
  expect(previewed.length).toBeGreaterThan(0);
  expect(Object.keys(TYPES).sort()).toEqual(previewed);
});

for (const [type, params] of Object.entries(TYPES)) {
  test(`${type}: the card shows its details and an enabled Approve`, async ({ page }) => {
    test.skip(type in SKIPPED, SKIPPED[type]);
    const response = await fetch(`${API_BASE}/api/decision-actions/proposals`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${seed.taskToken}`,
        "X-Workspace-ID": seed.workspaceId,
        "X-Agent-ID": seed.agentId,
        "X-Task-ID": seed.chatTaskId,
      },
      body: JSON.stringify({ action_type: type, version: 1, issue_id: seed.issueId, reason: "Card render check.", params: await params(seed, api) }),
    });
    expect(response.ok, `proposal failed: ${response.status} ${await response.clone().text()}`).toBe(true);

    await addVirtualAuthenticator(page, await seedPasskey(seed.userId));
    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken()!);
    await serveOverHttps(page.context());
    await page.goto(`${HTTPS_ORIGIN}/${seed.workspaceSlug}/chat?session=${seed.chatSessionId}`, { waitUntil: "domcontentloaded" });

    await expect(page.getByTestId("decision-request-card")).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId("decision-request-action-preview").locator("dt").first()).toBeVisible();
    await expect(page.getByText("Details unavailable")).toHaveCount(0);
    // The card's own Approve choice; it is disabled while the details are unavailable.
    await expect(page.getByRole("radio", { name: "Approve", exact: true })).toBeEnabled();
  });
}

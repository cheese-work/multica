/**
 * Helpers for the D8b journey: an agent's chat task proposes new instructions
 * for one workflow step in a DM, the signed-in owner approves with a passkey.
 * The agent, DM, task token and issue come from the D8a seed; this file adds
 * the workflow, the delegated-write setting and the readbacks.
 */
import { createHash } from "node:crypto";
import type { TestApiClient } from "./fixtures";
import { API_BASE, removeSeed, seedDmBudgetIssue, withDb, type DmBudgetSeed } from "./dm-budget-proposal-helpers";

export const STATUS_KEY = "todo";
export const OTHER_STATUS_KEY = "in_review";
export const BEFORE_TEXT = "Read the diff.\nPost the verdict.";
export const AFTER_TEXT = "Read the diff.\nRun the tests.\nPost the verdict.";
export const OTHER_STEP_TEXT = "Hand back to the author.";
/** What a person saves between the proposal and the approval. */
export const HUMAN_EDIT_TEXT = "Read the diff.\nAsk the author.\nPost the verdict.";

/** The value the journey installs for `delegated_governed_writes`, for the agent given. */
function delegationFor(agentId: string) {
  return { allowed_agents: [agentId], allowed_fields: ["workflow.instructions"], requires_human_decision: false };
}

export interface DmWorkflowSeed extends DmBudgetSeed {
  workflowId: string;
  /** The `delegated_governed_writes` setting before the journey (JSON text, or null when unset); restored by `removeWorkflowSeed`. */
  originalDelegation: string | null;
}

/** The agent, its DM and chat task, an anchor issue, a two-step workflow, and the setting that lets the agent propose step instructions. */
export async function seedDmWorkflow(api: TestApiClient, label: string): Promise<DmWorkflowSeed> {
  const seed = await seedDmBudgetIssue(api, `wf-${label}`);
  const steps = [
    { status_key: STATUS_KEY, handler: { type: "member", id: seed.userId }, instructions: BEFORE_TEXT, next_status_key: OTHER_STATUS_KEY },
    { status_key: OTHER_STATUS_KEY, handler: { type: "member", id: seed.userId }, instructions: OTHER_STEP_TEXT },
  ];
  const delegated = delegationFor(seed.agentId);
  try {
    const { workflowId, originalDelegation } = await withDb(async (db) => {
      // The D8a seed already linked the anchor issue to a workflow with no steps; give it the two steps.
      const id = (
        await db.query<{ id: string }>(
          `UPDATE issue_workflow SET steps = $2::jsonb WHERE id = (SELECT workflow_id FROM issue WHERE id = $1) RETURNING id::text`,
          [seed.issueId, JSON.stringify(steps)],
        )
      ).rows[0]!.id;
      // Last write, and one statement: the previous value of this one key is read and replaced atomically, so a failure above leaves the setting untouched.
      const original = (
        await db.query<{ original: string | null }>(
          `WITH old AS (SELECT (settings -> 'delegated_governed_writes')::text AS v FROM workspace WHERE id = $1 FOR UPDATE),
                set_ AS (UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || jsonb_build_object('delegated_governed_writes', $2::jsonb) WHERE id = $1)
           SELECT v AS original FROM old`,
          [seed.workspaceId, JSON.stringify(delegated)],
        )
      ).rows[0]!.original;
      return { workflowId: id, originalDelegation: original };
    });
    return { ...seed, workflowId, originalDelegation };
  } catch (error) {
    await removeSeed(seed);
    throw error;
  }
}

interface WorkflowStep {
  status_key: string;
  instructions: string;
  [field: string]: unknown;
}

/** Keys sorted at every depth, as the server's canonical form sorts them. */
function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value && typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    return `{${entries.map(([key, item]) => `${JSON.stringify(key)}:${canonical(item)}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

/** The digest an agent computes for the workflow it read: sha256 of {steps, workflow_id}. */
export function stepsDigest(workflowId: string, steps: WorkflowStep[]): string {
  return createHash("sha256").update(canonical({ steps, workflow_id: workflowId })).digest("hex");
}

/** The stored workflow as the API serves it. */
export async function readWorkflow(api: TestApiClient, seed: DmWorkflowSeed) {
  const response = await fetch(`${API_BASE}/api/issue-workflows/${seed.workflowId}`, {
    headers: { Authorization: `Bearer ${api.getToken()}`, "X-Workspace-ID": seed.workspaceId },
  });
  if (!response.ok) throw new Error(`GET issue-workflows failed: ${response.status} ${await response.text()}`);
  return (await response.json()) as { steps: WorkflowStep[] };
}

export function instructionsOf(workflow: { steps: WorkflowStep[] }, statusKey: string): string | undefined {
  return workflow.steps.find((step) => step.status_key === statusKey)?.instructions;
}

/** The agent's `workflow.step.instructions.set` proposal, made with its task token: it lands as a card in the DM. */
export async function proposeInstructions(api: TestApiClient, seed: DmWorkflowSeed, instructions = AFTER_TEXT): Promise<string> {
  const workflow = await readWorkflow(api, seed);
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
      action_type: "workflow.step.instructions.set",
      version: 1,
      issue_id: seed.issueId,
      reason: "The review step never says to run the tests.",
      params: { workflow_id: seed.workflowId, status_key: STATUS_KEY, base_sha256: stepsDigest(seed.workflowId, workflow.steps), instructions },
    }),
  });
  if (!response.ok) throw new Error(`proposal failed: ${response.status} ${await response.text()}`);
  return withDb(async (db) =>
    (await db.query<{ id: string }>(`SELECT id::text FROM decision_action WHERE requested_by_task_id = $1`, [seed.chatTaskId])).rows[0]!.id,
  );
}

/** A person's save of the step between the proposal and the approval (the stored form, as a workflow save writes it). */
export async function humanEditsStep(seed: DmWorkflowSeed, instructions = HUMAN_EDIT_TEXT) {
  await withDb((db) =>
    db.query(
      `UPDATE issue_workflow
          SET steps = (SELECT jsonb_agg(CASE WHEN step->>'status_key' = $2 THEN jsonb_set(step, '{instructions}', to_jsonb($3::text)) ELSE step END ORDER BY ord)
                         FROM jsonb_array_elements(steps) WITH ORDINALITY AS t(step, ord))
        WHERE id = $1`,
      [seed.workflowId, STATUS_KEY, instructions],
    ),
  );
}

export async function readActionState(seed: DmWorkflowSeed, actionId: string) {
  return withDb(async (db) => {
    const row = (await db.query<{ state: string; error_code: string | null }>(`SELECT state, error_code FROM decision_action WHERE id = $1`, [actionId])).rows[0]!;
    return { state: row.state, errorCode: row.error_code };
  });
}

/** Test-created rows, removed in dependency order; the issue goes with `api.cleanup()`. */
export async function removeWorkflowSeed(seed: DmWorkflowSeed, removeBase: (seed: DmBudgetSeed) => Promise<void>) {
  try {
    await removeBase(seed);
    await withDb(async (db) => {
      await db.query(`UPDATE issue SET workflow_id = NULL WHERE id = $1`, [seed.issueId]);
      await db.query(`DELETE FROM issue_workflow WHERE id = $1`, [seed.workflowId]);
    });
  } finally {
    // Runs after any earlier cleanup statement fails, so the delegated setting is never left installed.
    // Put back only the key this journey set, and only while it still holds the value this journey installed;
    // a concurrent change to that key, or to any other setting, is left as it is.
    await withDb((db) =>
      db.query(
        `UPDATE workspace SET settings = CASE WHEN $2::jsonb IS NULL THEN COALESCE(settings, '{}'::jsonb) - 'delegated_governed_writes'
                                              ELSE COALESCE(settings, '{}'::jsonb) || jsonb_build_object('delegated_governed_writes', $2::jsonb) END
          WHERE id = $1 AND settings -> 'delegated_governed_writes' = $3::jsonb`,
        [seed.workspaceId, seed.originalDelegation, JSON.stringify(delegationFor(seed.agentId))],
      ),
    );
  }
}

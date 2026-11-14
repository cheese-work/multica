/**
 * Helpers for the E8 journey: an agent's chat task proposes a new workspace context
 * in a DM, the signed-in owner approves with a passkey. The agent, DM, task token and
 * anchor issue come from the D8a seed; this file adds the stored context, the
 * delegated-write setting for `workspace.context` and the readbacks.
 */
import { createHash } from "node:crypto";
import { API_BASE, removeSeed, seedDmBudgetIssue, withDb, type DmBudgetSeed } from "./dm-budget-proposal-helpers";
import type { TestApiClient } from "./fixtures";

const FIELD = "workspace.context";

/** Eight lines, so the diff has unchanged context around the one line the proposal replaces. */
const CONTEXT_LINES = [
  "Work in ICT.",
  "Reply in short sentences.",
  "Keep PRs small.",
  "Use ICT for every time a person reads.",
  "Prefer merge commits.",
  "Post one comment per run.",
  "Record red and green output.",
  "Never poll CI.",
];
const CHANGED_LINE_INDEX = 3;
export const REMOVED_LINE = CONTEXT_LINES[CHANGED_LINE_INDEX]!;
export const ADDED_LINE = "Write every time a person reads in ICT, with the ICT suffix.";
export const BEFORE_CONTEXT = CONTEXT_LINES.join("\n");
export const AFTER_CONTEXT = CONTEXT_LINES.map((line, index) => (index === CHANGED_LINE_INDEX ? ADDED_LINE : line)).join("\n");
/** The unchanged line just above the change: the diff shows it as context, without a marker. */
export const UNCHANGED_LINE = CONTEXT_LINES[CHANGED_LINE_INDEX - 1]!;

export const sha256 = (text: string) => createHash("sha256").update(text).digest("hex");
/** The server counts code points; so does the test. */
export const codePoints = (text: string) => [...text].length;

/** The value the journey installs for `delegated_governed_writes`, for the agent given. */
function delegationFor(agentId: string) {
  return { allowed_agents: [agentId], allowed_fields: [FIELD], requires_human_decision: false };
}

export interface DmContextSeed extends DmBudgetSeed {
  /** The workspace context before the journey; restored by `removeContextSeed`. */
  originalContext: string | null;
  /** The `delegated_governed_writes` setting before the journey (JSON text, or null when unset). */
  originalDelegation: string | null;
}

/** The agent, its DM and chat task, an anchor issue, the stored context and the setting that lets the agent propose it. */
export async function seedDmContext(api: TestApiClient, label: string): Promise<DmContextSeed> {
  const seed = await seedDmBudgetIssue(api, `ctx-${label}`);
  const delegated = delegationFor(seed.agentId);
  try {
    const originals = await withDb(async (db) => {
      const context = (
        await db.query<{ context: string | null }>(
          `WITH old AS (SELECT context FROM workspace WHERE id = $1 FOR UPDATE),
                set_ AS (UPDATE workspace SET context = $2 WHERE id = $1)
           SELECT context FROM old`,
          [seed.workspaceId, BEFORE_CONTEXT],
        )
      ).rows[0]!.context;
      // Last write, and one statement: the previous value of this one key is read and replaced atomically, so a failure above leaves the setting untouched.
      const delegation = (
        await db.query<{ original: string | null }>(
          `WITH old AS (SELECT (settings -> 'delegated_governed_writes')::text AS v FROM workspace WHERE id = $1 FOR UPDATE),
                set_ AS (UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || jsonb_build_object('delegated_governed_writes', $2::jsonb) WHERE id = $1)
           SELECT v AS original FROM old`,
          [seed.workspaceId, JSON.stringify(delegated)],
        )
      ).rows[0]!.original;
      return { originalContext: context, originalDelegation: delegation };
    });
    return { ...seed, ...originals };
  } catch (error) {
    await removeSeed(seed);
    throw error;
  }
}

async function apiGet(api: TestApiClient, seed: DmContextSeed, path: string) {
  const response = await fetch(`${API_BASE}${path}`, {
    headers: { Authorization: `Bearer ${api.getToken()}`, "X-Workspace-ID": seed.workspaceId },
  });
  if (!response.ok) throw new Error(`GET ${path} failed: ${response.status} ${await response.text()}`);
  return response.json();
}

/** The workspace context as the API serves it. */
export async function readContext(api: TestApiClient, seed: DmContextSeed): Promise<string> {
  const workspace = (await apiGet(api, seed, `/api/workspaces/${seed.workspaceId}`)) as { context?: string | null };
  return workspace.context ?? "";
}

/** The agent's `workspace.context.set` proposal, made with its task token: it lands as a card in the DM. */
export async function proposeContext(seed: DmContextSeed, context = AFTER_CONTEXT): Promise<string> {
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
      action_type: "workspace.context.set",
      version: 1,
      issue_id: seed.issueId,
      reason: "The context never says how to write times.",
      params: { workspace_id: seed.workspaceId, context, base_sha256: sha256(BEFORE_CONTEXT), after_sha256: sha256(context) },
    }),
  });
  if (!response.ok) throw new Error(`proposal failed: ${response.status} ${await response.text()}`);
  return withDb(async (db) =>
    (await db.query<{ id: string }>(`SELECT id::text FROM decision_action WHERE requested_by_task_id = $1`, [seed.chatTaskId])).rows[0]!.id,
  );
}

/** The activity rows an approved context change leaves on the anchor issue (`config_change_applied`, one per approval). */
export async function readChangeActivity(seed: DmContextSeed) {
  return withDb(async (db) =>
    (
      await db.query<{ details: { field: string; target_id: string; before_sha256: string; after_sha256: string; decision_action_id: string } }>(
        `SELECT details FROM activity_log WHERE issue_id = $1 AND action = 'config_change_applied'`,
        [seed.issueId],
      )
    ).rows.map((row) => row.details),
  );
}

/** Test-created rows, removed in dependency order; the issue goes with `api.cleanup()`. */
export async function removeContextSeed(seed: DmContextSeed) {
  try {
    await removeSeed(seed);
  } finally {
    // Runs after a failed earlier cleanup statement, so the delegated setting and the context are never left installed.
    // Put back only the keys this journey set, and only while they still hold what this journey wrote;
    // a concurrent change to either, or to any other setting, is left as it is.
    await withDb(async (db) => {
      await db.query(
        `UPDATE workspace SET settings = CASE WHEN $2::jsonb IS NULL THEN COALESCE(settings, '{}'::jsonb) - 'delegated_governed_writes'
                                              ELSE COALESCE(settings, '{}'::jsonb) || jsonb_build_object('delegated_governed_writes', $2::jsonb) END
          WHERE id = $1 AND settings -> 'delegated_governed_writes' = $3::jsonb`,
        [seed.workspaceId, seed.originalDelegation, JSON.stringify(delegationFor(seed.agentId))],
      );
      await db.query(`UPDATE workspace SET context = $2 WHERE id = $1 AND context = ANY($3::text[])`, [
        seed.workspaceId,
        seed.originalContext,
        [BEFORE_CONTEXT, AFTER_CONTEXT],
      ]);
    });
  }
}

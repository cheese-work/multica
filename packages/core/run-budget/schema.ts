import { z } from "zod";
import { parseWithFallback } from "../api/schema";

// Mirrors service.RunBudgetSnapshot (server/internal/service/run_budget_snapshot.go).
export const RUN_BUDGET_STATES = ["ok", "near", "at_limit", "over"] as const;
export type RunBudgetState = (typeof RUN_BUDGET_STATES)[number];

const schema = z
  .object({
    budgeted: z.boolean(),
    // Negative when named runs went past the balance, so no nonnegative() here.
    runs_used: z.number().int(),
    runs_left: z.number().int(),
    runs_limit: z.number().int(),
    // Omitted by the server for an unbudgeted issue.
    state: z.enum(RUN_BUDGET_STATES).optional(),
    // Opaque string (design r6 N3): compare for equality only, never parse it.
    // Absent until the server ships it (A1b2).
    window_id: z.string().optional(),
    window_started_at: z.string(),
    blocking: z.array(z.string()),
    can_raise: z.boolean(),
  })
  .transform((v) => ({
    budgeted: v.budgeted,
    runsUsed: v.runs_used,
    runsLeft: v.runs_left,
    runsLimit: v.runs_limit,
    state: v.state ?? null,
    windowId: v.window_id ?? null,
    windowStartedAt: v.window_started_at,
    blocking: v.blocking,
    canRaise: v.can_raise,
  }));
export type RunBudget = z.output<typeof schema>;

// Shown for an unbudgeted issue and for any malformed response: the meter
// hides rather than render invented numbers.
export const UNBUDGETED: RunBudget = {
  budgeted: false,
  runsUsed: 0,
  runsLeft: 0,
  runsLimit: 0,
  state: null,
  windowId: null,
  windowStartedAt: "",
  blocking: [],
  canRaise: false,
};

export function parseRunBudget(data: unknown): RunBudget {
  const parsed = parseWithFallback(data, schema, UNBUDGETED, { endpoint: "GET /api/issues/{id}/run-budget" });
  return parsed.budgeted ? parsed : UNBUDGETED;
}

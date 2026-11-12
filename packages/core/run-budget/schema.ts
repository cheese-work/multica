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
    // The suggested raise. A malformed preview drops to null instead of
    // failing the meter: the raise button hides, the numbers stay.
    raise_preview: z.object({ add_runs: z.number().int(), limit_after: z.number().int() }).nullish().catch(null),
  })
  // The server sets state for every budgeted snapshot; without it the meter
  // would lose its exhaustion state, so treat the response as malformed.
  .refine((v) => !v.budgeted || v.state !== undefined, { message: "budgeted snapshot without state" })
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
    raisePreview: v.raise_preview ? { addRuns: v.raise_preview.add_runs, limitAfter: v.raise_preview.limit_after } : null,
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
  raisePreview: null,
};

export function parseRunBudget(data: unknown): RunBudget {
  const parsed = parseWithFallback(data, schema, UNBUDGETED, { endpoint: "GET /api/issues/{id}/run-budget" });
  return parsed.budgeted ? parsed : UNBUDGETED;
}

// Mirrors handler.RunBudgetActionResponse: the first link at top level, every link in `requests`.
const linkSchema = z.object({ request_id: z.string(), action_id: z.string(), approve_url: z.string().optional() });
const actionResponseSchema = z.object({ requests: z.array(linkSchema).min(1) });
export type RunBudgetActionResponse = z.output<typeof actionResponseSchema>;

/** Throws on a malformed answer: the request may have been created, so the caller must not read it as success. */
export function parseRunBudgetActionResponse(data: unknown): RunBudgetActionResponse {
  return actionResponseSchema.parse(data);
}

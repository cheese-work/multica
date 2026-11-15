import type { RunBudgetState } from "./schema";

export type RunBudgetTone = "ok" | "warn" | "danger";

const TONE: Record<RunBudgetState, RunBudgetTone> = { ok: "ok", near: "warn", at_limit: "danger", over: "danger" };

/** One colour decision for the meter and the chip. A missing state reads as ok. */
export function runBudgetTone(state: RunBudgetState | null): RunBudgetTone {
  return state ? TONE[state] : "ok";
}

/** The server sets run_budget_blocked while its budget marker is live; the marker only exists while the issue is blocked. */
export function isStoppedByBudget(issue: { status: string; run_budget_blocked?: boolean }): boolean {
  return issue.status === "blocked" && issue.run_budget_blocked === true;
}

import type { IssueWorkflowEdge, IssueWorkflowStep } from "../types";
import { workflowNextStatus } from "./queries";

/**
 * Editing and validating a workflow's edges client-side (CHE-2150). The rules
 * mirror `validateEdges` and `ParseGuard` in `server/internal/issueworkflow/edges.go`;
 * the server stays the authority, this only stops a save it would reject.
 */
export const MAX_EDGES_PER_STEP = 8;

/** The guards the server validates and a user can pick without naming an issue. */
export const EDITABLE_GUARDS = [
  "agent_done",
  "agent_verdict(PASS)",
  "agent_verdict(CHANGES)",
  "pr.checks_green",
  "pr.checks_red",
  "pr.ready",
  "pr.merged",
  "pr.closed_unmerged",
  "pr.review_changes_requested",
  "pr.review_approved",
  "children_done",
] as const;

const GUARD_ARITY: Record<string, number> = {
  agent_done: 0,
  agent_verdict: 1,
  "pr.checks_green": 0,
  "pr.checks_red": 0,
  "pr.ready": 0,
  "pr.merged": 0,
  "pr.closed_unmerged": 0,
  "pr.review_changes_requested": 0,
  "pr.review_approved": 0,
  children_done: 0,
  issue_status: 2,
};

const DEFAULT_TIMEOUT = "72h";
const MIN_TIMEOUT_MS = 60_000;
const MAX_TIMEOUT_MS = 30 * 24 * 3_600_000;
const UNIT_MS: Record<string, number> = {
  ns: 1e-6,
  us: 1e-3,
  "µs": 1e-3,
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
};
/** Statuses CI green may not move an issue out of (`CanAdvanceOnCIGreen`). */
const NO_CI_ADVANCE = new Set(["backlog", "blocked", "done", "cancelled"]);

/** Whether a `pr.checks_green` exit is allowed from this status. */
export function canAdvanceOnCIGreen(statusKey: string): boolean {
  return !NO_CI_ADVANCE.has(statusKey);
}

export type GraphIssueCode =
  | "too_many_edges"
  | "missing_target"
  | "bad_guard"
  | "bad_timeout"
  | "bad_on_timeout"
  | "ci_green_source";

export interface GraphIssue {
  code: GraphIssueCode;
  from: string;
  /** Position in the step's exits; absent for a step-level issue. */
  index?: number;
}

function isValidGuard(guard: string): boolean {
  const s = guard.trim();
  const open = s.indexOf("(");
  const kind = open < 0 ? s : s.slice(0, open);
  const arity = GUARD_ARITY[kind];
  if (arity === undefined) return false;
  let args: string[] = [];
  if (open >= 0) {
    if (!s.endsWith(")")) return false;
    args = s.slice(open + 1, -1).split(",").map((a) => a.trim());
  }
  if (args.length !== arity || args.join("").includes(")")) return false;
  if (kind === "agent_verdict" && args[0] !== "PASS" && args[0] !== "CHANGES") {
    return false;
  }
  return args.every((a) => a !== "");
}

/** A guard on a PR fact waits on the outside world, so it needs a timeout. */
export function guardNeedsTimeout(guard: string): boolean {
  return guard.trim().startsWith("pr.");
}

/** Milliseconds of a Go duration such as "72h" or "1h30m"; null when malformed. */
function durationMs(s: string): number | null {
  const t = s.trim();
  if (!/^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/.test(t)) return null;
  let total = 0;
  for (const [, n, unit] of t.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)) {
    total += Number(n) * UNIT_MS[unit!]!;
  }
  return total;
}

/** A step's exits: its own edges, or the ones its legacy fields stand for (`EdgesOf`). */
export function stepEdges(
  steps: IssueWorkflowStep[],
  from: string,
): IssueWorkflowEdge[] {
  const step = steps.find((s) => s.status_key === from);
  if (!step) return [];
  if (step.edges?.length) return step.edges;
  const next = workflowNextStatus({ steps }, from);
  const ci = {
    guard: "pr.checks_green",
    timeout: DEFAULT_TIMEOUT,
    ...(steps.some((s) => s.status_key === "blocked")
      ? { on_timeout: "blocked" }
      : {}),
  };
  return [
    ...(next && step.next_status_key
      ? [{ to: next, guard: "agent_done" }]
      : []),
    ...(step.back_status_key
      ? [{ to: step.back_status_key, guard: "agent_verdict(CHANGES)" }]
      : []),
    ...(next && step.advance_on_ci_green
      ? [{ to: next, ...ci }]
      : []),
  ];
}

export function validateWorkflowGraph(
  steps: IssueWorkflowStep[],
): GraphIssue[] {
  const listed = new Set(steps.map((s) => s.status_key));
  const issues: GraphIssue[] = [];
  for (const step of steps) {
    const from = step.status_key;
    const edges = stepEdges(steps, from);
    // Legacy flags carry no timeout and the server does not ask for one; only
    // explicit edges are held to the timeout rules.
    const explicit = !!step.edges?.length;
    if (edges.length > MAX_EDGES_PER_STEP) {
      issues.push({ code: "too_many_edges", from });
    }
    edges.forEach((e, index) => {
      const flag = (code: GraphIssueCode) => issues.push({ code, from, index });
      if (e.to === from || !listed.has(e.to)) return void flag("missing_target");
      if (!isValidGuard(e.guard)) return void flag("bad_guard");
      if (e.guard.trim() === "pr.checks_green" && !canAdvanceOnCIGreen(from)) {
        return void flag("ci_green_source");
      }
      if (!explicit || !guardNeedsTimeout(e.guard)) return;
      const ms = durationMs(e.timeout ?? "");
      if (ms === null || ms < MIN_TIMEOUT_MS || ms > MAX_TIMEOUT_MS) {
        return void flag("bad_timeout");
      }
      const onTimeout = e.on_timeout ?? "";
      if (
        onTimeout
          ? onTimeout === from || !listed.has(onTimeout)
          : !listed.has("blocked")
      ) {
        flag("bad_on_timeout");
      }
    });
  }
  return issues;
}

/** Writes a step's exits as explicit edges, retiring the legacy fields they came from. */
function withEdges(
  steps: IssueWorkflowStep[],
  from: string,
  edges: IssueWorkflowEdge[],
): IssueWorkflowStep[] {
  return steps.map((s) =>
    s.status_key === from
      ? {
          ...s,
          edges,
          next_status_key: undefined,
          back_status_key: undefined,
          advance_on_ci_green: undefined,
        }
      : s,
  );
}

export function addStepEdge(
  steps: IssueWorkflowStep[],
  from: string,
  to: string,
): IssueWorkflowStep[] {
  const edges = stepEdges(steps, from);
  if (
    to === from ||
    !steps.some((s) => s.status_key === to) ||
    edges.length >= MAX_EDGES_PER_STEP
  ) {
    return steps;
  }
  return withEdges(steps, from, [...edges, { to, guard: "agent_done" }]);
}

export function updateStepEdge(
  steps: IssueWorkflowStep[],
  from: string,
  index: number,
  patch: Partial<IssueWorkflowEdge>,
): IssueWorkflowStep[] {
  const edges = stepEdges(steps, from);
  const old = edges[index];
  if (!old) return steps;
  const next: IssueWorkflowEdge = { ...old, ...patch };
  if (patch.guard !== undefined) {
    if (!guardNeedsTimeout(next.guard)) {
      delete next.timeout;
      delete next.on_timeout;
    } else {
      next.timeout ||= DEFAULT_TIMEOUT;
      if (!next.on_timeout && steps.some((s) => s.status_key === "blocked")) {
        next.on_timeout = "blocked";
      }
    }
  }
  return withEdges(
    steps,
    from,
    edges.map((e, i) => (i === index ? next : e)),
  );
}

export function removeStepEdge(
  steps: IssueWorkflowStep[],
  from: string,
  index: number,
): IssueWorkflowStep[] {
  const edges = stepEdges(steps, from);
  if (!edges[index]) return steps;
  return withEdges(
    steps,
    from,
    edges.filter((_, i) => i !== index),
  );
}

import type { IssueWorkflowLintFinding } from "../types";

export type LintLevel = IssueWorkflowLintFinding["level"];
export type LintFilter = "all" | LintLevel;
export interface LintCounts {
  errors: number;
  warnings: number;
  infos: number;
}

/** Counts of findings that still count: a waived finding is shown but not tallied. */
export function lintCounts(findings: IssueWorkflowLintFinding[]): LintCounts {
  const counts: LintCounts = { errors: 0, warnings: 0, infos: 0 };
  for (const f of findings) {
    if (f.waived) continue;
    if (f.level === "error") counts.errors++;
    else if (f.level === "warning") counts.warnings++;
    else counts.infos++;
  }
  return counts;
}

export function filterLint(findings: IssueWorkflowLintFinding[], filter: LintFilter): IssueWorkflowLintFinding[] {
  return filter === "all" ? findings : findings.filter((f) => f.level === filter);
}

/** Findings grouped by step, in the order the steps first appear. Workflow-level findings have step "". */
export function groupLintByStep(findings: IssueWorkflowLintFinding[]): { step: string; findings: IssueWorkflowLintFinding[] }[] {
  const groups = new Map<string, IssueWorkflowLintFinding[]>();
  for (const f of findings) {
    const step = f.step ?? "";
    groups.set(step, [...(groups.get(step) ?? []), f]);
  }
  return [...groups].map(([step, list]) => ({ step, findings: list }));
}

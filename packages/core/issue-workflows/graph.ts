import type { IssueWorkflow, IssueWorkflowEdge } from "../types";
import { workflowNextStatus } from "./queries";

export interface WorkflowGraphEdge extends IssueWorkflowEdge {
  id: string;
  from: string;
}

/**
 * Every exit of every step, for drawing (CHE-2150). A step without `edges`
 * gets the ones its legacy fields stand for, as the server derives them in
 * `EdgesOf` (CHE-2030); edges to a status the workflow does not list are dropped.
 */
export function workflowEdges(
  workflow: Pick<IssueWorkflow, "steps">,
): WorkflowGraphEdge[] {
  const listed = new Set(workflow.steps.map((s) => s.status_key));
  return workflow.steps
    .flatMap((step): WorkflowGraphEdge[] => {
      const from = step.status_key;
      const next = workflowNextStatus(workflow, from);
      const own: IssueWorkflowEdge[] = step.edges?.length
        ? step.edges
        : [
            ...(next && step.next_status_key
              ? [{ to: next, guard: "agent_done" }]
              : []),
            ...(step.back_status_key
              ? [{ to: step.back_status_key, guard: "agent_verdict(CHANGES)" }]
              : []),
            ...(next && step.advance_on_ci_green
              ? [{ to: next, guard: "pr.checks_green" }]
              : []),
          ];
      return own.map((e) => ({ ...e, id: `${from}>${e.to}:${e.guard}`, from }));
    })
    .filter((e) => listed.has(e.to));
}

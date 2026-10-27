import type { IssueWorkflow, IssueWorkflowEdge } from "../types";
import { stepEdges } from "./graph-edit";

export interface WorkflowGraphEdge extends IssueWorkflowEdge {
  id: string;
  from: string;
  /** Position among the step's exits; what the editor addresses an edge by. */
  index: number;
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
    .flatMap((step) =>
      stepEdges(workflow.steps, step.status_key).map((e, index) => ({
        ...e,
        id: `${step.status_key}#${index}`,
        from: step.status_key,
        index,
      })),
    )
    .filter((e) => listed.has(e.to));
}

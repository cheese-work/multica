import type { IssueWorkflow, IssueWorkflowEdge } from "../types";
import { PR_MERGE_STATUS_NONE } from "../github/settings";
import { stepEdges } from "./graph-edit";

export interface WorkflowGraphEdge extends IssueWorkflowEdge {
  id: string;
  from: string;
  /** Position among the step's exits; what the editor addresses an edge by. */
  index: number;
  /** A derived edge no step holds: it is drawn, never selected, edited or saved. */
  readOnly?: boolean;
}

/** `from` of the virtual PR merge edge: the rule applies from any step. */
export const ANY_STEP = "*";

/**
 * Every exit of every step, for drawing (CHE-2150). A step without `edges`
 * gets the ones its legacy fields stand for, as the server derives them in
 * `EdgesOf` (CHE-2030); edges to a status the workflow does not list are dropped.
 */
export function workflowEdges(
  workflow: Pick<IssueWorkflow, "steps">,
  /** The workspace `pr_merge_status`; adds the read-only PR merge edge. */
  prMergeStatus?: string,
): WorkflowGraphEdge[] {
  const listed = new Set(workflow.steps.map((s) => s.status_key));
  const own = workflow.steps
    .flatMap((step) =>
      stepEdges(workflow.steps, step.status_key).map((e, index) => ({
        ...e,
        id: `${step.status_key}#${index}`,
        from: step.status_key,
        index,
      })),
    )
    .filter((e) => listed.has(e.to));
  if (
    !prMergeStatus ||
    prMergeStatus === PR_MERGE_STATUS_NONE ||
    !listed.has(prMergeStatus)
  ) {
    return own;
  }
  return [
    ...own,
    {
      id: `${ANY_STEP}#pr_merge_status`,
      index: -1,
      from: ANY_STEP,
      to: prMergeStatus,
      guard: "pr_merge_status",
      readOnly: true,
    },
  ];
}

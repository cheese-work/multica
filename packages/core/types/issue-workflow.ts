/**
 * Workspace workflows (MUL-7420).
 *
 * A workflow is a whole pipeline: an ordered pick from the shared status
 * catalog plus who each column hands an issue to. Every workflow lists todo,
 * done and cancelled; backlog and blocked are optional, and the five keep fixed
 * places at the ends. Entering a column through a status change assigns the
 * issue to the column's handler and, for an agent, starts its run with the
 * column's instructions. A project only picks a workflow; projects that name
 * none use the workspace's Default workflow.
 */

export type IssueWorkflowHandlerType =
  | "none"
  | "agent"
  | "squad"
  | "member"
  | "project_lead"
  | "creator"
  | "human_by_role";

export interface IssueWorkflowHandler {
  type: IssueWorkflowHandlerType;
  /** Set only for agent, squad and member handlers. */
  id?: string;
}

/** A column with its handoff; handler "none" keeps the assignee. */
export interface IssueWorkflowStep {
  status_key: string;
  handler: IssueWorkflowHandler;
  instructions: string;
  /** Suggested status once the step is done; named in the handler's brief. */
  next_status_key?: string;
  /** Status to send the issue back to when it needs changes. */
  back_status_key?: string;
  /** Move an issue on this step to its next status when a linked PR's checks pass. */
  advance_on_ci_green?: boolean;
  /** Guarded exits; the first whose guard holds is taken. */
  edges?: IssueWorkflowEdge[];
}

/** One exit of a step, e.g. `{ to: "done", guard: "pr.merged", timeout: "72h" }`. */
export interface IssueWorkflowEdge {
  to: string;
  guard: string;
  timeout?: string;
  on_timeout?: string;
}

export interface IssueWorkflow {
  id: string;
  workspace_id: string;
  name: string;
  description: string;
  /** The workspace Default: used by every project that names no workflow. */
  is_default: boolean;
  /** The member who owns the workflow and may edit it; null for Default. */
  owner_id: string | null;
  steps: IssueWorkflowStep[];
  /**
   * Most runs one issue may start before the workflow blocks it. Null when the
   * workflow keeps the platform default (15).
   */
  max_runs_per_issue?: number | null;
  /** Whether a child issue's PRs count for the parent's PR edges. Default true. */
  include_child_prs?: boolean;
  /** Projects currently using this workflow. */
  project_ids: string[];
  created_at: string;
  updated_at: string;
  /** Full-precision updated_at; empty from a server that predates it. */
  version?: string;
}

export interface ListIssueWorkflowsResponse {
  workflows: IssueWorkflow[];
  total: number;
}

/** A status for a new column, created with the workflow write. */
export interface NewWorkflowStatus {
  name: string;
  category?: string;
  color?: string;
}

/** A column of a workflow write: an existing status or a new one, with its handoff. */
export type IssueWorkflowStepInput = ({ status_key: string } | { new_status: NewWorkflowStatus }) &
  Partial<SetWorkflowHandoffRequest>;

export interface IssueWorkflowWriteRequest {
  name?: string;
  description?: string;
  /**
   * Every column with its handoff. The server adds todo, done and cancelled
   * when missing and places the fixed columns; backlog and blocked are listed
   * only when included.
   */
  steps?: IssueWorkflowStepInput[];
  /** 1 to 1000 sets the run budget, 0 restores the default; omit to keep it. */
  max_runs_per_issue?: number;
  include_child_prs?: boolean;
  /** Destination for issues on statuses an edit removes, keyed by removed status. */
  status_mapping?: Record<string, string>;
  dry_run?: boolean;
  /**
   * The `version` of the workflow the edit started from. The server refuses the
   * save with 409 `workflow_changed_elsewhere` when the workflow has moved on.
   */
  expected_version?: string;
}

/** A status some issues are on that the target workflow does not list. */
export interface IssueWorkflowMappingRequirement {
  status_key: string;
  issue_count: number;
  suggested_status_key: string;
}

/** A status some issues are on that both workflows list, so they keep it. */
export interface IssueWorkflowStatusCount {
  status_key: string;
  issue_count: number;
}

export interface IssueWorkflowMappingPlan {
  required: IssueWorkflowMappingRequirement[];
  unchanged: IssueWorkflowStatusCount[];
  total_issues: number;
  affected_issues: number;
}

export interface SetProjectWorkflowRequest {
  /** null switches the project back to the Default workflow. */
  workflow_id: string | null;
  status_mapping?: Record<string, string>;
  dry_run?: boolean;
}

export interface IssueWorkflowDryRunResponse {
  dry_run: true;
  plan: IssueWorkflowMappingPlan;
}

/** An active run of the agent a handoff takes the issue from. */
export interface WorkflowHandoffRun {
  task_id: string;
  agent_id: string;
  status: string;
  started_at: string | null;
}

/**
 * What moving an issue to a status would do under its project's workflow
 * (MUL-7420). `handoff` is false for the Default workflow and for steps that
 * keep the assignee.
 */
export interface WorkflowHandoffPreview {
  handoff: boolean;
  workflow_name: string;
  from_status: string;
  to_status: string;
  handler_type: "agent" | "squad" | "member" | null;
  handler_id: string | null;
  previous_assignee_type: string | null;
  previous_assignee_id: string | null;
  previous_runs: WorkflowHandoffRun[];
  /** The brief the handler's run receives; empty for a member handler. */
  brief: string;
}

/** A workflow's handoff for one column; handler "none" clears it. */
export interface SetWorkflowHandoffRequest {
  handler: IssueWorkflowHandler;
  instructions?: string;
  next_status_key?: string;
  back_status_key?: string;
  advance_on_ci_green?: boolean;
  edges?: IssueWorkflowEdge[];
}

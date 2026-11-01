/**
 * The workflow an issue follows, and the statuses it offers (CHE-2822).
 *
 * Mirrors `resolveIssueWorkflow` and `useIssueStatusOptions` in packages/core
 * and packages/views, restated here because those modules reach web's API
 * client (not on mobile's import whitelist). Order: the issue's own override,
 * else its project's named workflow, else the workspace Default.
 */
import type { IssueWorkflow, Project } from "@multica/core/types";
import type { IssueStatusCatalog, StatusOption } from "./issue-status";

/**
 * Null while the workflow is unknown: the workflow list or the project list is
 * not loaded (pending or failed), or the named override or project is missing.
 * Unknown never falls back to Default.
 */
export function resolveIssueWorkflow(
  workflows: readonly IssueWorkflow[] | undefined,
  projects: readonly Project[] | undefined,
  projectId: string | null,
  workflowId?: string | null,
): IssueWorkflow | null {
  if (!workflows) return null;
  let named = workflowId;
  if (!named && projectId) {
    const project = projects?.find((p) => p.id === projectId);
    if (!project) return null;
    named = project.workflow_id;
  }
  const found = named ? workflows.find((w) => w.id === named) : undefined;
  // A named workflow the list lacks is unknown, never a fallback to Default.
  return found ?? (workflowId ? null : workflows.find((w) => w.is_default) ?? null);
}

/**
 * `options` narrowed to the issue's workflow steps, in workflow order. A step
 * `options` lacks (catalog still loading) keeps a fallback row from `catalog`,
 * as web does. The current status stays listed so it can still be seen and
 * moved off. Whole catalog while the workflow is unknown; the server still
 * refuses a status the workflow omits.
 */
export function issueWorkflowStatusOptions(
  options: StatusOption[],
  catalog: IssueStatusCatalog,
  workflows: readonly IssueWorkflow[] | undefined,
  projects: readonly Project[] | undefined,
  issue: { project_id: string | null; workflow_id?: string | null; status: string },
): StatusOption[] {
  const workflow = resolveIssueWorkflow(workflows, projects, issue.project_id, issue.workflow_id);
  if (!workflow) return options;
  const byKey = new Map(options.map((o) => [o.key, o]));
  const optionOf = (key: string): StatusOption =>
    byKey.get(key) ?? {
      key,
      category: catalog.categoryOf(key),
      label: catalog.labelOf(key),
      color: catalog.colorOf(key),
      icon: catalog.iconOf(key),
    };
  const ordered = workflow.steps.map((s) => optionOf(s.status_key));
  const current = byKey.get(issue.status);
  return current && !workflow.steps.some((s) => s.status_key === issue.status) ? [...ordered, current] : ordered;
}

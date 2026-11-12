import type { DecisionActionPreview, DecisionConfigChange, DecisionRequestAction } from "../types/decision";

// The action types the server renders a preview for. A type outside this list
// is newer than this client: it cannot be vouched for, so it is not approvable here.
const PREVIEWED_ACTION_TYPES = [
  "issue.run_budget.raise",
  "issue.run_budget.top_up",
  "agent.env.patch",
  "agent.copy",
] as const;

export const SHA256_HEX = /^[0-9a-f]{64}$/;

export function isPreviewedActionType(type: string): boolean {
  return (PREVIEWED_ACTION_TYPES as readonly string[]).includes(type);
}

/** The preview to show and approve against, or undefined to fail closed ("details unavailable"). */
export function approvablePreview(action: Pick<DecisionRequestAction, "action_type" | "preview">): DecisionActionPreview | undefined {
  const { preview } = action;
  if (!preview || !isPreviewedActionType(action.action_type) || !SHA256_HEX.test(preview.sha256)) return undefined;
  const wellFormed = preview.fields.length > 0
    && preview.fields.every((f) => typeof f.label === "string" && typeof f.current === "string" && typeof f.proposed === "string");
  return wellFormed ? preview : undefined;
}

/** Scope A: the only fields this client shows and approves. Anything else is newer than this client or denied. */
export const CONFIG_CHANGE_FIELDS = [
  "agent.instructions", "squad.instructions", "workspace.context",
  "workflow.steps", "workflow.instructions", "workspace.settings", "workflow.budget",
] as const;

const CONFIG_CHANGE_ACTION_TYPES = [
  "agent.instructions.set", "squad.instructions.set", "workspace.context.set",
  "workflow.steps.set", "workflow.step.instructions.set", "workspace.settings.patch", "workflow.budget.set",
] as const;

export function isConfigChangeActionType(type: string): boolean {
  return (CONFIG_CHANGE_ACTION_TYPES as readonly string[]).includes(type);
}

/** The server preview to show and approve against, or undefined to fail closed ("details unavailable"). */
export function approvableConfigChange(action: Pick<DecisionRequestAction, "action_type" | "config_change">): DecisionConfigChange | undefined {
  const change = action.config_change;
  if (!change || !isConfigChangeActionType(action.action_type) || !SHA256_HEX.test(change.sha256)) return undefined;
  if (!(CONFIG_CHANGE_FIELDS as readonly string[]).includes(change.field)) return undefined;
  if ((change.field === "workspace.settings") !== (change.keys.length > 0)) return undefined;
  if (!["json", "text", "lines"].includes(change.diff_kind)) return undefined;
  if (change.field === "workflow.instructions" && !(workflowStepTarget(change))) return undefined;
  const wellFormed = change.diff.every((hunk) => hunk.lines.every((line) => line.length > 0 && " +-".includes(line[0]!)));
  return wellFormed ? change : undefined;
}

/** The server-resolved workflow name and status key of a workflow step change, or undefined when either is missing. */
export function workflowStepTarget({ meta }: Pick<DecisionConfigChange, "meta">): { workflowName: string; statusKey: string } | undefined {
  const workflowName = meta?.workflow_name;
  const statusKey = meta?.status_key;
  return typeof workflowName === "string" && workflowName !== "" && typeof statusKey === "string" && statusKey !== ""
    ? { workflowName, statusKey }
    : undefined;
}

import type { DecisionActionDiff, DecisionActionPreview, DecisionRequestAction } from "../types/decision";

// The action types the server renders a preview for. A type outside this list
// is newer than this client: it cannot be vouched for, so it is not approvable here.
const PREVIEWED_ACTION_TYPES = [
  "issue.run_budget.raise",
  "issue.run_budget.top_up",
  "agent.env.patch",
  "agent.copy",
] as const;

// Config-change actions whose card shows the stored text diff instead of preview fields.
const DIFF_ACTION_TYPES = ["workflow.step.instructions.set"] as const;

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

export function isDiffActionType(type: string): boolean {
  return (DIFF_ACTION_TYPES as readonly string[]).includes(type);
}

/** The diff to show and approve against, or undefined to fail closed ("details unavailable"). */
export function approvableDiff(action: Pick<DecisionRequestAction, "action_type" | "diff">): DecisionActionDiff | undefined {
  const { diff } = action;
  if (!diff || !isDiffActionType(action.action_type) || diff.kind !== "text" || !SHA256_HEX.test(diff.sha256)) return undefined;
  const wellFormed = Array.isArray(diff.hunks) && diff.hunks.every((h) =>
    Number.isInteger(h.old_start) && Number.isInteger(h.new_start) && Array.isArray(h.lines)
    && h.lines.every((line) => typeof line === "string" && /^[ +-]/.test(line)));
  // The diff does not say which step it edits: without a named workflow and status key there is nothing to approve.
  const named = !!diff.target?.workflow_name && !!diff.target.status_key;
  return wellFormed && named ? diff : undefined;
}

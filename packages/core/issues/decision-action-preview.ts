import type { DecisionActionPreview, DecisionRequestAction } from "../types/decision";

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

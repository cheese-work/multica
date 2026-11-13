import type { IssueWakeupInput, WakeupCondition, WakeupDefinitionConfig } from "@multica/core/types";
import { WAKEUP_MAX_FIRES, WAKEUP_WAIT_DAYS, type WakeupDraft } from "./wakeup-draft";

export const WAKEUP_DEFINITION_NAME_MAX = 120;

export type WakeupDefinitionConfigError = "missing_name" | "unsupported_filter" | "unsupported_schedule";

/**
 * The workspace rule a draft describes. A definition stores a trigger, a target
 * and limits, not who spoke or which agent ran, and has no timeout wake, so
 * those draft choices are refused instead of dropped. The server re-validates.
 */
export function definitionConfigFromInput(
  input: IssueWakeupInput,
  name: string,
): { config: WakeupDefinitionConfig } | { error: WakeupDefinitionConfigError } {
  const trimmed = name.trim();
  if (!trimmed || [...trimmed].length > WAKEUP_DEFINITION_NAME_MAX) return { error: "missing_name" };
  if (input.kind !== "event") return { error: "unsupported_schedule" };
  if (input.filter_actor_type || input.filter_actor_id || input.filter_agent_id) return { error: "unsupported_filter" };
  const config: WakeupDefinitionConfig = {
    v: 1,
    name: trimmed,
    trigger: input.condition
      ? { kind: "condition", condition: input.condition }
      : { kind: "event", events: input.event_types ?? [] },
    target: { type: "agent", id: input.agent_id },
    instruction: input.instruction,
    mode: input.mode,
  };
  if (input.max_fires) config.max_fires = input.max_fires;
  if (input.expires_in_seconds) config.expiry = { after_seconds: input.expires_in_seconds };
  return { config };
}

/** The draft fields a platform condition sets, or null for one the editor cannot express. */
function draftFromCondition(c: WakeupCondition): Partial<WakeupDraft> | null {
  switch (c.type) {
    case "issue_field":
      if (c.field === "status") return { condition: "field", field: "status", fieldTarget: c.value };
      if (c.field === "label") return { condition: "field", field: "label", fieldTarget: c.label_id };
      if (c.field === "assignee") {
        return { condition: "field", field: "assignee", assignee: { type: c.assignee_type, id: c.assignee_id } };
      }
      return { condition: "field", field: "property", fieldTarget: c.property_id, fieldValue: String(c.value) };
    case "children_done":
      return { condition: "children", stage: c.stage ?? null };
    case "pull_request":
      return { condition: "pull_request", prEvent: c.event };
    default:
      return null;
  }
}

/**
 * The editor draft a saved rule opens with: the inverse of
 * `definitionConfigFromInput`. A rule the editor could not write back as it is
 * (another issue as the trigger, a fixed expiry, no expiry, a wait outside the
 * offered choices) returns null, so it is never rewritten into a different rule.
 */
export function draftFromDefinitionConfig(
  config: WakeupDefinitionConfig,
): { name: string; draft: Partial<WakeupDraft> } | null {
  const { trigger, target, expiry } = config;
  if (!config.name || !trigger || target?.type !== "agent" || !target.id || !config.instruction) return null;
  if (config.mode !== "once" && config.mode !== "continuous") return null;
  const days = expiry?.at === undefined && expiry?.after_seconds ? expiry.after_seconds / 86400 : 0;
  if (!(WAKEUP_WAIT_DAYS as readonly number[]).includes(days)) return null;
  if (config.mode === "continuous" && !(WAKEUP_MAX_FIRES as readonly number[]).includes(config.max_fires ?? 0)) return null;
  let when: Partial<WakeupDraft> | null = null;
  if (trigger.kind === "event" && trigger.events?.length) when = { condition: "custom", events: trigger.events };
  else if (trigger.kind === "condition") when = draftFromCondition(trigger.condition as WakeupCondition);
  if (!when) return null;
  return {
    name: config.name,
    draft: {
      ...when,
      agentId: target.id,
      instruction: config.instruction,
      mode: config.mode,
      waitDays: days,
      ...(config.max_fires && config.mode === "continuous" ? { maxFires: config.max_fires } : {}),
    },
  };
}

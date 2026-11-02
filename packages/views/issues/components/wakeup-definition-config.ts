import type { IssueWakeupInput, WakeupDefinitionConfig } from "@multica/core/types";

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

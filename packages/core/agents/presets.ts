import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const agentPresetKeys = {
  all: (wsId: string, agentId: string) =>
    ["workspaces", wsId, "agents", agentId, "presets"] as const,
  list: (wsId: string, agentId: string) =>
    [...agentPresetKeys.all(wsId, agentId), "list"] as const,
  completions: (wsId: string, agentId: string) =>
    [...agentPresetKeys.all(wsId, agentId), "completions"] as const,
};

export const agentPresetsOptions = (wsId: string, agentId: string) =>
  queryOptions({
    queryKey: agentPresetKeys.list(wsId, agentId),
    queryFn: () => api.listAgentPresets(agentId),
  });

export const agentPresetCompletionsOptions = (wsId: string, agentId: string) =>
  queryOptions({
    queryKey: agentPresetKeys.completions(wsId, agentId),
    queryFn: () => api.getAgentPresetCompletions(agentId),
  });

/** Names that start with the typed text come first, then names that contain it; server order kept within each group. */
export function filterPresetCompletions(names: string[], typed: string): string[] {
  const q = typed.toLowerCase();
  if (!q) return names;
  const lower = (n: string) => n.toLowerCase();
  return [
    ...names.filter((n) => lower(n).startsWith(q)),
    ...names.filter((n) => !lower(n).startsWith(q) && lower(n).includes(q)),
  ];
}

/** "model · thinking · tier · context args", unset parts left out; "" when the preset sets nothing. */
export function presetSettingsSummary(p: {
  model: string | null;
  thinking_level: string | null;
  service_tier: string | null;
  context_args: string[] | null;
}): string {
  return [[p.model, p.thinking_level, p.service_tier].filter(Boolean).join(" · "), p.context_args?.join(" ")]
    .filter(Boolean)
    .join(" · ");
}

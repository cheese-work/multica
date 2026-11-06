import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { WakeupDefinitionConfig, WakeupDefinitionScope } from "../types";

/** The scopes an owner or admin edits in settings; an issue edits its own rules inline. */
export type EditableWakeupScope = Exclude<WakeupDefinitionScope, { kind: "issue" }>;

export const WORKSPACE_WAKEUP_SCOPE: EditableWakeupScope = { kind: "workspace" };

function listKey(workspaceId: string, scope: WakeupDefinitionScope) {
  return scope.kind === "workspace"
    ? ["workspace-wakeup-definitions", workspaceId]
    : ["wakeup-definitions", workspaceId, scope.kind, scope.id];
}

/** Every rule resolved at one scope; a write there refreshes them all. */
function effectiveKey(workspaceId: string, scope: WakeupDefinitionScope, ruleKey?: string) {
  const base = ["wakeup-effective", workspaceId, scope.kind, scope.kind === "workspace" ? "" : scope.id];
  return ruleKey ? [...base, ruleKey] : base;
}

/**
 * A scope's wakeup definitions and whether the server accepts writes.
 * An older server has no endpoint: the caller reads a failure as "closed".
 */
export function wakeupDefinitionsOptions(workspaceId: string, scope: WakeupDefinitionScope) {
  return queryOptions({
    queryKey: listKey(workspaceId, scope),
    queryFn: () => api.listWakeupDefinitions(scope),
    enabled: !!workspaceId,
    retry: false,
  });
}

/** One rule as it resolves at a scope: where each field came from, and an issue instance's runtime state. */
export function effectiveWakeupRuleOptions(workspaceId: string, scope: WakeupDefinitionScope, ruleKey: string) {
  return queryOptions({
    queryKey: effectiveKey(workspaceId, scope, ruleKey),
    queryFn: () => api.getEffectiveWakeupRule(scope, ruleKey),
    enabled: !!workspaceId,
    retry: false,
  });
}

/** What a draft rule would resolve to and meet at once; nothing is saved. */
export function previewWakeupDefinition(scope: EditableWakeupScope, ruleKey: string, config: WakeupDefinitionConfig) {
  return api.previewWakeupDefinition(scope, { rule_key: ruleKey, config });
}

export function useCreateWakeupDefinition(workspaceId: string, scope: EditableWakeupScope) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (config: WakeupDefinitionConfig) => api.createWakeupDefinition(scope, { config }),
    onSettled: () => client.invalidateQueries({ queryKey: listKey(workspaceId, scope) }),
  });
}

function useRefreshScope(workspaceId: string, scope: WakeupDefinitionScope) {
  const client = useQueryClient();
  return () =>
    Promise.all([
      client.invalidateQueries({ queryKey: listKey(workspaceId, scope) }),
      client.invalidateQueries({ queryKey: effectiveKey(workspaceId, scope) }),
    ]);
}

export function useDeleteWakeupDefinition(workspaceId: string, scope: WakeupDefinitionScope) {
  const refresh = useRefreshScope(workspaceId, scope);
  return useMutation({
    mutationFn: ({ ruleKey, revision }: { ruleKey: string; revision: string }) =>
      api.deleteWakeupDefinition(scope, ruleKey, revision),
    onSettled: refresh,
  });
}

export function useSaveWakeupDefinition(workspaceId: string, scope: WakeupDefinitionScope) {
  const refresh = useRefreshScope(workspaceId, scope);
  return useMutation({
    mutationFn: ({ ruleKey, revision, config }: { ruleKey: string; revision: string; config: WakeupDefinitionConfig }) =>
      api.saveWakeupDefinition(scope, ruleKey, { revision, config }),
    onSettled: refresh,
  });
}

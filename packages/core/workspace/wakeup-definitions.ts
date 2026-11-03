import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { WakeupDefinitionConfig, WakeupDefinitionScope } from "../types";

/** The scopes an owner or admin edits in settings; an issue edits its own rules inline. */
export type EditableWakeupScope = Exclude<WakeupDefinitionScope, { kind: "issue" }>;

export const WORKSPACE_WAKEUP_SCOPE: EditableWakeupScope = { kind: "workspace" };

function listKey(workspaceId: string, scope: EditableWakeupScope) {
  return scope.kind === "workspace"
    ? ["workspace-wakeup-definitions", workspaceId]
    : ["wakeup-definitions", workspaceId, scope.kind, scope.id];
}

/**
 * A scope's wakeup definitions and whether the server accepts writes.
 * An older server has no endpoint: the caller reads a failure as "closed".
 */
export function wakeupDefinitionsOptions(workspaceId: string, scope: EditableWakeupScope) {
  return queryOptions({
    queryKey: listKey(workspaceId, scope),
    queryFn: () => api.listWakeupDefinitions(scope),
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

export function useDeleteWakeupDefinition(workspaceId: string, scope: EditableWakeupScope) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ ruleKey, revision }: { ruleKey: string; revision: string }) =>
      api.deleteWakeupDefinition(scope, ruleKey, revision),
    onSettled: () => client.invalidateQueries({ queryKey: listKey(workspaceId, scope) }),
  });
}

export function useSaveWakeupDefinition(workspaceId: string, scope: EditableWakeupScope) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ ruleKey, revision, config }: { ruleKey: string; revision: string; config: WakeupDefinitionConfig }) =>
      api.saveWakeupDefinition(scope, ruleKey, { revision, config }),
    onSettled: () => client.invalidateQueries({ queryKey: listKey(workspaceId, scope) }),
  });
}

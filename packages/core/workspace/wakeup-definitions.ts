import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { WakeupDefinitionConfig } from "../types";

const WORKSPACE = { kind: "workspace" } as const;

/**
 * The workspace's wakeup definitions and whether the server accepts writes.
 * An older server has no endpoint: the caller reads a failure as "closed".
 */
export function workspaceWakeupDefinitionsOptions(workspaceId: string) {
  return queryOptions({
    queryKey: ["workspace-wakeup-definitions", workspaceId],
    queryFn: () => api.listWakeupDefinitions(WORKSPACE),
    enabled: !!workspaceId,
    retry: false,
  });
}

/** What a draft rule would resolve to and meet at once; nothing is saved. */
export function previewWorkspaceWakeupDefinition(ruleKey: string, config: WakeupDefinitionConfig) {
  return api.previewWakeupDefinition(WORKSPACE, { rule_key: ruleKey, config });
}

export function useCreateWorkspaceWakeupDefinition(workspaceId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (config: WakeupDefinitionConfig) => api.createWakeupDefinition(WORKSPACE, { config }),
    onSettled: () => client.invalidateQueries({ queryKey: workspaceWakeupDefinitionsOptions(workspaceId).queryKey }),
  });
}

export function useDeleteWorkspaceWakeupDefinition(workspaceId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ ruleKey, revision }: { ruleKey: string; revision: string }) =>
      api.deleteWakeupDefinition(WORKSPACE, ruleKey, revision),
    onSettled: () => client.invalidateQueries({ queryKey: workspaceWakeupDefinitionsOptions(workspaceId).queryKey }),
  });
}

export function useSaveWorkspaceWakeupDefinition(workspaceId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ ruleKey, revision, config }: { ruleKey: string; revision: string; config: WakeupDefinitionConfig }) =>
      api.saveWakeupDefinition(WORKSPACE, ruleKey, { revision, config }),
    onSettled: () => client.invalidateQueries({ queryKey: workspaceWakeupDefinitionsOptions(workspaceId).queryKey }),
  });
}

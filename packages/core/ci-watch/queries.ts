import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api, ApiError, errorCode } from "../api";
import { CI_WATCHER_DISABLED_CODE, type CIWatchConfig } from "./schema";

export const ciWatchKeys = {
  all: (wsId: string, repo: string) => ["ci-watch", wsId, repo] as const,
  config: (wsId: string, repo: string) => [...ciWatchKeys.all(wsId, repo), "config"] as const,
  events: (wsId: string, repo: string) => [...ciWatchKeys.all(wsId, repo), "events"] as const,
};

/** True for the 403 a server answers while the CI watcher feature flag is off. */
export function isCIWatcherDisabled(err: unknown): boolean {
  return err instanceof ApiError && err.status === 403 && errorCode(err) === CI_WATCHER_DISABLED_CODE;
}

// `retry: false`: a 403 (non-admin, flag off) will not change on retry.
export const ciWatchConfigOptions = (wsId: string, repo: string, enabled = true) =>
  queryOptions({
    queryKey: ciWatchKeys.config(wsId, repo),
    queryFn: () => api.getCIWatchConfig(wsId, repo),
    enabled: enabled && !!wsId && !!repo,
    retry: false,
  });

export const ciWatchEventsOptions = (wsId: string, repo: string, enabled = true) =>
  queryOptions({
    queryKey: ciWatchKeys.events(wsId, repo),
    queryFn: () => api.listCIWatchEvents(wsId, repo, 50),
    enabled: enabled && !!wsId && !!repo,
    retry: false,
  });

export function useUpdateCIWatchConfig(wsId: string, repo: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: Record<string, unknown>) => api.updateCIWatchConfig(wsId, repo, patch),
    onSuccess: (config: CIWatchConfig) => {
      qc.setQueryData(ciWatchKeys.config(wsId, repo), config);
      // A mode change cancels pending actions and re-labels would-do rows.
      void qc.invalidateQueries({ queryKey: ciWatchKeys.events(wsId, repo) });
    },
  });
}

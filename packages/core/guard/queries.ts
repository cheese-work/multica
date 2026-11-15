import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type { GuardDecisionFilters } from "../types/guard";

export const GUARD_WINDOW_DAYS = 7;

export const guardKeys = {
  all: (wsId: string) => ["guard", wsId] as const,
  stats: (wsId: string, days: number) => ["guard", wsId, "stats", days] as const,
  policy: (wsId: string) => ["guard", wsId, "policy"] as const,
  decisions: (wsId: string, filters: GuardDecisionFilters) => ["guard", wsId, "decisions", filters] as const,
};

/** Hits per plugin over the last `days` days; the same numbers as `multica guard stats`. */
export function guardStatsOptions(wsId: string, days = GUARD_WINDOW_DAYS) {
  return queryOptions({
    queryKey: guardKeys.stats(wsId, days),
    queryFn: () => api.getGuardStats(days),
    enabled: !!wsId,
  });
}

/** Newest decisions first; the same rows as `multica guard log`. */
export function guardDecisionsOptions(wsId: string, filters: GuardDecisionFilters) {
  return queryOptions({
    queryKey: guardKeys.decisions(wsId, filters),
    queryFn: () => api.listGuardDecisions(filters),
    enabled: !!wsId,
  });
}

/** The stored policy and its digest: the base a passkey edit signs over. */
export function guardPolicyOptions(wsId: string) {
  return queryOptions({
    queryKey: guardKeys.policy(wsId),
    queryFn: () => api.getGuardPolicy(wsId),
    enabled: !!wsId,
    staleTime: 0,
  });
}

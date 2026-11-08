import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";

export const providerBreakerKeys = {
  all: (wsId: string) => ["provider-breakers", wsId] as const,
};

/** Every provider breaker of the workspace, for Settings. */
export function providerBreakersOptions(wsId: string) {
  return queryOptions({
    queryKey: providerBreakerKeys.all(wsId),
    queryFn: () => api.listProviderBreakers(wsId),
    enabled: !!wsId,
    // Breakers move on their own (half-open, auto-close); keep the panel honest.
    refetchInterval: 30_000,
  });
}

/** Owner/admin only on the server; the reset is audited there. */
export function useResetProviderBreaker(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (provider: string) => api.resetProviderBreaker(wsId, provider),
    onSettled: () => qc.invalidateQueries({ queryKey: providerBreakerKeys.all(wsId) }),
  });
}

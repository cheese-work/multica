import {
  QueryClient,
  type InvalidateOptions,
  type InvalidateQueryFilters,
  type Query,
} from "@tanstack/react-query";
import { inboxQueryRetry, inboxQueryRetryDelay } from "./inbox/retry";
import { setWorkspaceSlugLookup } from "./paths/workspace-slugs";
import { workspaceKeys } from "./workspace/queries";
import type { Workspace } from "./types";

const isHidden = () =>
  typeof document !== "undefined" && document.visibilityState === "hidden";

/**
 * Realtime events invalidate queries in every open tab, and `invalidateQueries`
 * refetches active observers even when the tab is hidden — N tabs means N
 * refetches per event (CHE-3117). While hidden we only mark queries stale
 * (`refetchType: "none"`); the tab refetches the invalidated ones once it
 * becomes visible again. Patches the instance (not a subclass) so modules that
 * mock `@tanstack/react-query` without `QueryClient` still import cleanly.
 */
function deferInvalidationWhenHidden(qc: QueryClient): QueryClient {
  if (typeof document === "undefined") return qc;
  const invalidate = qc.invalidateQueries.bind(qc);
  // `setQueryData` clears `isInvalidated`, but a partial patch (e.g. a rename)
  // does not satisfy a deferred refresh (a missing new row), so remember the
  // obligation separately until the visible catch-up refetch takes it.
  const deferred = new WeakSet<Query>();
  qc.invalidateQueries = (filters?: InvalidateQueryFilters, options?: InvalidateOptions) => {
    if (!isHidden() || filters?.refetchType !== undefined) return invalidate(filters, options);
    qc.getQueryCache().findAll(filters).forEach((q) => deferred.add(q));
    return invalidate({ ...filters, refetchType: "none" }, options);
  };
  // ponytail: listener lives as long as the client (one per app; StrictMode's
  // discarded extra client leaks one listener in dev only).
  document.addEventListener("visibilitychange", () => {
    if (isHidden()) return;
    void qc.refetchQueries({
      type: "active",
      predicate: (q) => deferred.delete(q) || q.state.isInvalidated,
    });
  });
  return qc;
}

export function createQueryClient(): QueryClient {
  const client = new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: Infinity,
        gcTime: 10 * 60 * 1000, // 10 minutes
        refetchOnWindowFocus: false,
        refetchOnReconnect: true,
        retry: 1,
      },
      mutations: {
        retry: false,
      },
    },
  });
  // Keep the policy on the app client so custom clients and explicit query
  // overrides retain their retry settings. A busy server (503 + Retry-After)
  // is tried again a few times with backoff instead of leaving the Inbox
  // looking empty (MUL-9635); see ./inbox/retry.
  client.setQueryDefaults(["inbox"], {
    retry: inboxQueryRetry,
    retryDelay: (failureCount, error) => inboxQueryRetryDelay(failureCount, error),
  });
  return deferInvalidationWhenHidden(client);
}

/**
 * Point path resolution at `client`'s workspace list. Reading the cache on
 * every lookup covers fetches, hydration, mutations and logout/clear. Owned by
 * the mounted provider rather than the constructor: StrictMode builds and
 * discards an extra client, which must never become the bound one.
 */
export function bindWorkspaceSlugLookup(client: QueryClient): () => void {
  return setWorkspaceSlugLookup(
    (slug) =>
      client
        .getQueryData<Workspace[]>(workspaceKeys.list())
        ?.some((workspace) => workspace.slug === slug) ?? false,
  );
}

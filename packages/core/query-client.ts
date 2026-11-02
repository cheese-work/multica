import {
  QueryClient,
  type InvalidateOptions,
  type InvalidateQueryFilters,
} from "@tanstack/react-query";
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
 * becomes visible again.
 */
class TabAwareQueryClient extends QueryClient {
  constructor(config: ConstructorParameters<typeof QueryClient>[0]) {
    super(config);
    if (typeof document === "undefined") return;
    // ponytail: listener lives as long as the client (one per app; StrictMode's
    // discarded extra client leaks one listener in dev only).
    document.addEventListener("visibilitychange", () => {
      if (isHidden()) return;
      void this.refetchQueries({
        type: "active",
        predicate: (q) => q.state.isInvalidated,
      });
    });
  }

  override invalidateQueries(
    filters?: InvalidateQueryFilters,
    options?: InvalidateOptions,
  ): Promise<void> {
    if (isHidden() && filters?.refetchType === undefined) {
      filters = { ...filters, refetchType: "none" };
    }
    return super.invalidateQueries(filters, options);
  }
}

export function createQueryClient(): QueryClient {
  return new TabAwareQueryClient({
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

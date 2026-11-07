import { queryOptions } from "@tanstack/react-query";
import { api } from "@/data/api";

// The cache entry keeps the `validated` flag; plain consumers read the list
// through `select`, so they see `Workspace[]` as before.
export const workspaceListCheckedOptions = () =>
  queryOptions({
    queryKey: ["workspaces"] as const,
    queryFn: ({ signal }) => api.listWorkspacesChecked({ signal }),
  });

export const workspaceListOptions = () =>
  queryOptions({
    ...workspaceListCheckedOptions(),
    select: (result) => result.workspaces,
  });

interface CheckedListQuery {
  isSuccess: boolean;
  isFetching: boolean;
  data: { workspaces: { id: string }[]; validated: boolean } | undefined;
}

/** The workspace list when it may be trusted as the user's real workspaces: a
 *  settled fetch whose response matched the schema. Null otherwise (failed or
 *  pending fetch, or the schema fallback), so nothing is deleted from it. */
export function validatedWorkspaces(
  query: CheckedListQuery,
): { id: string }[] | null {
  if (!query.isSuccess || query.isFetching || !query.data?.validated) return null;
  return query.data.workspaces;
}

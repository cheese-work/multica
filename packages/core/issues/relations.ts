import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import { issueKeys } from "./queries";

export function issueRelationsOptions(workspaceId: string, issueId: string, depth: 1 | 2) {
  return queryOptions({
    queryKey: [...issueKeys.all(workspaceId), "relations", issueId, depth],
    queryFn: ({ signal }) => api.getIssueRelations(workspaceId, issueId, depth, signal),
    staleTime: 0,
  });
}

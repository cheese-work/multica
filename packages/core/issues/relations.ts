import { queryOptions } from "@tanstack/react-query";
import type { IssueRelations } from "../types";
import { api } from "../api";
import { issueKeys } from "./queries";

export function issueRelationsOptions(workspaceId: string, issueId: string, depth: 1 | 2, includeMentions = false) {
  return queryOptions({
    queryKey: [...issueKeys.all(workspaceId), "relations", issueId, depth, includeMentions],
    queryFn: ({ signal }) => api.getIssueRelations(workspaceId, issueId, depth, signal, includeMentions),
    staleTime: 30_000,
  });
}

export type RelationFilter = "parent" | "wakeup" | "mention";
export const RELATION_FILTERS: RelationFilter[] = ["parent", "wakeup", "mention"];

// Edge types without a filter (duplicate, shared PR, unknown) always show.
const EDGE_FILTER: Partial<Record<IssueRelations["edges"][number]["type"], RelationFilter>> = {
  parent: "parent", wakeup: "wakeup", children_done: "wakeup", mention: "mention",
};

export function relationFilterOf(type: IssueRelations["edges"][number]["type"]): RelationFilter | undefined {
  return EDGE_FILTER[type];
}

/** Drops hidden edge types, then every node no longer reachable from the current issue. */
export function filterRelations(graph: IssueRelations, currentIssueId: string, hidden: readonly RelationFilter[]): IssueRelations {
  if (hidden.length === 0) return graph;
  const edges = graph.edges.filter((edge) => {
    const filter = relationFilterOf(edge.type);
    return !filter || !hidden.includes(filter);
  });
  const reachable = new Set([currentIssueId]);
  for (let grew = true; grew;) {
    grew = false;
    for (const { source, target } of edges) {
      if (reachable.has(source) !== reachable.has(target)) {
        reachable.add(source).add(target);
        grew = true;
      }
    }
  }
  return { ...graph, nodes: graph.nodes.filter((node) => reachable.has(node.id)), edges: edges.filter((edge) => reachable.has(edge.source) && reachable.has(edge.target)) };
}

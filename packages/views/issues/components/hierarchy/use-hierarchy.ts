"use client";

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { childIssuesOptions, issueDetailOptions } from "@multica/core/issues/queries";
import { issueRelationsOptions } from "@multica/core/issues/relations";
import type { Issue } from "@multica/core/types";
import { summarizeGroup, type HierarchyIssue } from "./hierarchy-model";
import type { Hierarchy } from "./hierarchy-layout";

const toNode = ({ id, identifier, title, status }: Issue): HierarchyIssue => ({ id, identifier, title, status });
const byNumber = (a: Issue, b: Issue) => a.number - b.number;

/** Wake / mention neighbours of `issueId` from a relations graph. */
function neighbours(graph: { nodes: HierarchyIssue[]; edges: { source: string; target: string; type: string }[] } | undefined, issueId: string, types: string[]) {
  if (!graph) return [];
  const ids = new Set(graph.edges.filter((e) => types.includes(e.type) && (e.source === issueId || e.target === issueId)).flatMap((e) => [e.source, e.target]));
  ids.delete(issueId);
  return graph.nodes.filter((node) => ids.has(node.id));
}

export function useHierarchy(issue: Pick<Issue, "id" | "parent_issue_id" | "identifier" | "title" | "status">, { wakes, mentions }: { wakes: boolean; mentions: boolean }) {
  const workspaceId = useWorkspaceId();
  const catalog = useIssueStatuses(workspaceId);
  const parentId = issue.parent_issue_id ?? "";
  const parent = useQuery({ ...issueDetailOptions(workspaceId, parentId), enabled: !!parentId });
  const siblings = useQuery({ ...childIssuesOptions(workspaceId, parentId), enabled: !!parentId });
  const children = useQuery({ ...childIssuesOptions(workspaceId, issue.id) });
  const relations = useQuery(issueRelationsOptions(workspaceId, issue.id, 1));
  // Mentions are costly server-side: read them from cache, fetch only once the dialog asks.
  const mentionGraph = useQuery({ ...issueRelationsOptions(workspaceId, issue.id, 1, true), enabled: mentions });
  const cachedMentions = useQuery({ ...issueRelationsOptions(workspaceId, issue.id, 1, true), enabled: false });

  const loading = (!!parentId && (parent.isPending || siblings.isPending)) || children.isPending;
  const error = parent.isError || siblings.isError || children.isError;
  const mentionData = mentionGraph.data ?? cachedMentions.data;

  const hierarchy = useMemo<Hierarchy | null>(() => {
    if (loading || error) return null;
    const siblingIssues = (siblings.data ?? []).filter((s) => s.id !== issue.id).sort(byNumber).map(toNode);
    const childIssues = (children.data ?? []).slice().sort(byNumber).map(toNode);
    const related = [
      ...(wakes ? neighbours(relations.data, issue.id, ["wakeup", "children_done"]) : []),
      ...(mentions ? neighbours(mentionData, issue.id, ["mention"]) : []),
    ];
    const taken = new Set([parentId, ...siblingIssues.map((s) => s.id), ...childIssues.map((c) => c.id)]);
    return {
      parent: parent.data ? toNode(parent.data) : null,
      current: toNode(issue as Issue),
      siblings: summarizeGroup(siblingIssues, catalog.categoryOf),
      children: summarizeGroup(childIssues, catalog.categoryOf),
      siblingIssues,
      childIssues,
      relatedIssues: [...new Map(related.filter((r) => !taken.has(r.id)).map((r) => [r.id, r])).values()],
    };
  }, [loading, error, siblings.data, children.data, parent.data, relations.data, mentionData, wakes, mentions, issue, parentId, catalog.categoryOf]);

  const count = (graph: typeof mentionData, types: string[]) => neighbours(graph, issue.id, types).length;
  return {
    hierarchy,
    loading,
    error,
    catalog,
    // Counts for the footer and the dialog chips; mentions stay null until loaded.
    wakeCount: count(relations.data, ["wakeup", "children_done"]),
    mentionCount: mentionData ? count(mentionData, ["mention"]) : null,
    mentionsLoading: mentions && mentionGraph.isFetching,
    refetch: () => { void parent.refetch(); void siblings.refetch(); void children.refetch(); },
  };
}

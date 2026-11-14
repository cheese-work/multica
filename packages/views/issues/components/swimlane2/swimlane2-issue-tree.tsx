"use client";

// CHE-4358: Swimlane 2.0 as a sidebar/dialog mode: the focus issue plus its direct relations, lanes by assignee.
// ponytail: one hop (parent, children, wakeups, dependencies, mentions) in one batch call; level-by-level loading when a deeper tree is asked for.
import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@multica/ui/components/ui/button";
import { useWorkspaceId } from "@multica/core/hooks";
import { issueRelationsOptions, issueRowsByIdsOptions } from "@multica/core/issues/graph-queries";
import type { GraphEdgeType, TimelineEdge } from "@multica/core/issues/graph-core";
import type { Issue } from "@multica/core/types";
import { useActorName } from "@multica/core/workspace/hooks";
import { useT } from "../../../i18n";
import { Swimlane2Graph, type Swimlane2Node } from "./swimlane2-graph";

const RELATION_OPTS = { types: ["parent", "wakeup", "dependency", "mention"], includeMentions: true } as const satisfies Parameters<typeof issueRelationsOptions>[2];

export function Swimlane2IssueTree({ issue }: { issue: Issue }) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const { getActorName } = useActorName();
  const ids = useMemo(() => [issue.id], [issue.id]);
  const relations = useQuery(issueRelationsOptions(wsId, ids, RELATION_OPTS));
  const nodeIds = useMemo(() => [...new Set([issue.id, ...(relations.data?.nodes ?? []).map((n) => n.id)])], [issue.id, relations.data]);
  const rows = useQuery(issueRowsByIdsOptions(wsId, nodeIds));

  const { nodes, edges } = useMemo(() => {
    const byId = new Map((rows.data ?? []).map((r) => [r.id, r]));
    const source = new Map((relations.data?.nodes ?? []).map((n) => [n.id, n]));
    source.set(issue.id, { ...(source.get(issue.id) ?? {}), id: issue.id, identifier: issue.identifier, title: issue.title, status: issue.status, parent_issue_id: issue.parent_issue_id } as never);
    const nodes: Swimlane2Node[] = [...source.values()].map((n) => {
      const row = byId.get(n.id);
      return { id: n.id, identifier: n.identifier, title: n.title, status: n.status, parentId: n.parent_issue_id, at: row?.created_at ?? "", lane: row?.assignee_type && row.assignee_id ? `${row.assignee_type}:${row.assignee_id}` : null };
    });
    const edges: TimelineEdge[] = (relations.data?.edges ?? []).map((e) => ({ type: e.type as GraphEdgeType, source: e.source, target: e.target }));
    return { nodes, edges };
  }, [issue, relations.data, rows.data]);

  const laneLabel = (key: string | null) => {
    if (key === null) return t(($) => $.swimlane2.no_lane);
    const [type, id] = key.split(":");
    return getActorName(type!, id!);
  };

  if (relations.isError) {
    return (
      <div role="alert" className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/10 px-3 py-1.5 text-caption">
        <span>{t(($) => $.swimlane2.timeline_failed)}</span>
        <Button type="button" variant="outline" size="xs" onClick={() => void relations.refetch()} className="ml-auto">{t(($) => $.swimlane2.retry)}</Button>
      </div>
    );
  }
  if (relations.isPending) return <div role="status" className="p-2 text-caption text-muted-foreground">{t(($) => $.swimlane2.loading)}</div>;
  return <Swimlane2Graph nodes={nodes} edges={edges} notLoaded={new Set(relations.data?.notLoaded ?? [])} onRetry={() => void relations.refetch()} laneLabel={laneLabel} />;
}

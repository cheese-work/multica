"use client";

// Swimlane 2.0 container (CHE-4312 W3): loads the timeline pages for the surface's scope and filters, the issue
// rows (lane keys) and the relations (edges), and hands them to the presentational Swimlane2Graph.
import { useMemo } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Button } from "@multica/ui/components/ui/button";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  issueActualTimelineOptions,
  issueRelationsOptions,
  issueRowsByIdsOptions,
  mergeTimelinePages,
} from "@multica/core/issues/graph-queries";
import type { GraphEdgeType, TimelineEdge } from "@multica/core/issues/graph-core";
import type { SwimlaneGrouping } from "@multica/core/issues/stores/view-store";
import { projectListOptions } from "@multica/core/projects/queries";
import type { Issue, IssueTableQuerySpec } from "@multica/core/types";
import { useActorName } from "@multica/core/workspace/hooks";
import { useT } from "../../../i18n";
import { Swimlane2Graph, type Swimlane2Node } from "./swimlane2-graph";

const RELATION_OPTS = { types: ["parent", "wakeup", "dependency", "mention"], includeMentions: true } as const satisfies Parameters<typeof issueRelationsOptions>[2];

function laneKey(issue: Issue, grouping: SwimlaneGrouping): string | null {
  if (grouping === "project") return issue.project_id;
  if (grouping === "parent") return issue.parent_issue_id;
  return issue.assignee_type && issue.assignee_id ? `${issue.assignee_type}:${issue.assignee_id}` : null;
}

export function Swimlane2View({ query, grouping }: { query: IssueTableQuerySpec; grouping: SwimlaneGrouping }) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const { getActorName } = useActorName();
  const request = useMemo(() => ({ scope: query.scope, filters: query.filters }), [query.scope, query.filters]);

  const timeline = useInfiniteQuery(issueActualTimelineOptions(wsId, request));
  const merged = useMemo(() => (timeline.data?.pages.length ? mergeTimelinePages(timeline.data.pages) : null), [timeline.data]);
  const ids = useMemo(() => merged?.issues.map((i) => i.issue_id) ?? [], [merged]);

  const rows = useQuery(issueRowsByIdsOptions(wsId, ids));
  const relations = useQuery(issueRelationsOptions(wsId, ids, RELATION_OPTS));
  const { data: projects } = useQuery(projectListOptions(wsId));

  const { nodes, edges, notLoaded } = useMemo(() => {
    const byId = new Map((rows.data ?? []).map((r) => [r.id, r]));
    const nodes: Swimlane2Node[] = [];
    for (const item of merged?.issues ?? []) {
      const row = byId.get(item.issue_id);
      if (!row) continue;
      nodes.push({
        id: row.id,
        identifier: row.identifier,
        title: row.title,
        status: item.status,
        parentId: row.parent_issue_id,
        at: item.actual_start ?? item.presence_from ?? "",
        lane: laneKey(row, grouping),
      });
    }
    const inResponse = new Set(nodes.map((n) => n.id));
    for (const n of relations.data?.nodes ?? []) {
      if (inResponse.has(n.id)) continue;
      nodes.push({ id: n.id, identifier: n.identifier, title: n.title, status: n.status, parentId: n.parent_issue_id, context: true, at: "", lane: null });
    }
    const edges: TimelineEdge[] = (relations.data?.edges ?? []).map((e) => ({ type: e.type as GraphEdgeType, source: e.source, target: e.target }));
    return { nodes, edges, notLoaded: new Set(relations.data?.notLoaded ?? []) };
  }, [merged, rows.data, relations.data, grouping]);

  const laneLabel = useMemo(() => {
    const projectName = new Map((projects ?? []).map((p) => [p.id, p.title]));
    const byId = new Map(nodes.map((n) => [n.id, n]));
    return (key: string | null) => {
      if (key === null) return t(($) => $.swimlane2.no_lane);
      if (grouping === "project") return projectName.get(key) ?? key;
      if (grouping === "parent") {
        const parent = byId.get(key);
        return parent ? `${parent.identifier} ${parent.title}` : key;
      }
      const [type, id] = key.split(":");
      return getActorName(type!, id!);
    };
  }, [grouping, nodes, projects, getActorName, t]);

  if (timeline.isError) {
    return (
      <div role="alert" className="m-4 flex items-center gap-2 rounded-md border border-warning/40 bg-warning/10 px-3 py-1.5 text-caption">
        <span>{t(($) => $.swimlane2.timeline_failed)}</span>
        <Button type="button" variant="outline" size="xs" onClick={() => void timeline.refetch()} className="ml-auto">
          {t(($) => $.swimlane2.retry)}
        </Button>
      </div>
    );
  }
  if (!merged || rows.isPending && ids.length > 0) {
    return <div role="status" className="p-4 text-caption text-muted-foreground">{t(($) => $.swimlane2.loading)}</div>;
  }

  return (
    <Swimlane2Graph
      nodes={nodes}
      edges={edges}
      notLoaded={notLoaded}
      hasMoreIssues={!!timeline.hasNextPage}
      onLoadMore={() => void timeline.fetchNextPage()}
      onRetry={() => void relations.refetch()}
      laneLabel={laneLabel}
    />
  );
}

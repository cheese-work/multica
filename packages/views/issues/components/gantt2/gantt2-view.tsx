"use client";

// Gantt v2 container (CHE-4312 G5): loads the timeline pages for the surface's scope and filters and the issue rows
// (identifier, title, parent), then hands them to the presentational Gantt2Bars. Registered in the `gantt` mode on
// Project, Issues and My Issues; the upstream gantt-view.tsx stays in the tree unchanged.
import { useMemo } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Button } from "@multica/ui/components/ui/button";
import { useWorkspaceId } from "@multica/core/hooks";
import { issueActualTimelineOptions, issueRowsByIdsOptions, mergeTimelinePages } from "@multica/core/issues/graph-queries";
import type { IssueTableQuerySpec } from "@multica/core/types";
import { useT } from "../../../i18n";
import { Gantt2Bars, type Gantt2Row } from "./gantt2-bars";
import { useGantt2Collapse } from "./use-gantt2-collapse";

export function Gantt2View({ query }: { query: IssueTableQuerySpec }) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const { collapsed, onToggleCollapse } = useGantt2Collapse();
  const request = useMemo(() => ({ scope: query.scope, filters: query.filters }), [query.scope, query.filters]);

  const timeline = useInfiniteQuery(issueActualTimelineOptions(wsId, request));
  const merged = useMemo(() => (timeline.data?.pages.length ? mergeTimelinePages(timeline.data.pages) : null), [timeline.data]);
  const ids = useMemo(() => merged?.issues.map((i) => i.issue_id) ?? [], [merged]);
  const issueRows = useQuery(issueRowsByIdsOptions(wsId, ids));

  const rows = useMemo<Gantt2Row[]>(() => {
    const byId = new Map((issueRows.data ?? []).map((r) => [r.id, r]));
    return (merged?.issues ?? []).flatMap((timelineIssue) => {
      const row = byId.get(timelineIssue.issue_id);
      return row ? [{ identifier: row.identifier, title: row.title, timeline: timelineIssue, parentId: row.parent_issue_id }] : [];
    });
  }, [merged, issueRows.data]);

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
  if (!merged || (issueRows.isPending && ids.length > 0)) {
    return <div role="status" className="p-4 text-caption text-muted-foreground">{t(($) => $.swimlane2.loading)}</div>;
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-auto">
      <Gantt2Bars
        rows={rows}
        window={merged.window}
        asOf={merged.as_of}
        collapsed={collapsed}
        onToggleCollapse={onToggleCollapse}
        wakeupEvents={merged.wakeup_events}
      />
      {timeline.hasNextPage && (
        <Button type="button" variant="outline" size="xs" className="m-2 self-start" disabled={timeline.isFetchingNextPage} onClick={() => void timeline.fetchNextPage()}>
          {t(($) => $.swimlane2.load_more)}
        </Button>
      )}
    </div>
  );
}

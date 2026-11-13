"use client";

// Gantt v2 actual bars (CHE-4312 G1). Design lock: design/timeline-views/approved (Direction B).
// Presentational: one bar per issue, from the server's actual start to its actual end (or now), split into
// status segments. Every segment carries its status icon and a text label, so colour is never the only signal.
// Parent/child rows (CHE-4355 G3): row order and collapse come from K1 `buildGraph`; the collapse set is owned by the caller
// (view store `collapsedIssues.gantt`). Markers, partial-history rows and wakeups are later slices (G2, G4). Not registered in the view-mode popup yet (G5).
import { useMemo } from "react";
import { buildGraph } from "@multica/core/issues/graph-core";
import { Button } from "@multica/ui/components/ui/button";
import { ChevronRight } from "@multica/ui/icons";
import type { IssueTimelineResponse } from "@multica/core/api/issue-graph-schema";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { useT } from "../../../i18n";
import { useStatusLabel } from "../../utils/status-label";
import { StatusIcon } from "../status-icon";

type TimelineIssue = IssueTimelineResponse["issues"][number];
type Segment = TimelineIssue["segments"][number];

export interface Gantt2Row {
  identifier: string;
  title: string;
  timeline: TimelineIssue;
  /** Parent issue id; a parent outside `rows` makes this row a root. */
  parentId?: string | null;
}
export interface Gantt2BarsProps {
  rows: Gantt2Row[];
  /** The timeline window; the x axis spans it. */
  window: { from: string; to: string };
  /** Server "now" of the page chain; an open bar ends here. */
  asOf: string;
  /** Issue ids whose subtree is collapsed. */
  collapsed?: ReadonlySet<string>;
  /** Without it rows show no toggle. */
  onToggleCollapse?: (issueId: string) => void;
}

const LABEL_W = 220;
const ROW_H = 32;
/** A segment narrower than this keeps its icon and moves its text label to assistive tech only. */
const LABEL_MIN_PX = 72;
const TRACK_W = 900;

const pct = (n: number) => `${+(n * 100).toFixed(4)}%`;

const NO_COLLAPSE: ReadonlySet<string> = new Set();
const INDENT_PX = 12;

export function Gantt2Bars({ rows, window, asOf, collapsed = NO_COLLAPSE, onToggleCollapse }: Gantt2BarsProps) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const statusLabel = useStatusLabel(wsId);
  const { categoryOf, colorOf, iconOf } = useIssueStatuses(wsId);

  const axis = useMemo(() => {
    const from = Date.parse(window.from);
    const to = Date.parse(window.to);
    const span = Math.max(1, to - from);
    const now = Date.parse(asOf);
    /** Position of a time on the axis as a 0..1 fraction, clamped to the window. */
    const at = (iso: string | null, fallback: number) => Math.min(1, Math.max(0, ((iso ? Date.parse(iso) : fallback) - from) / span));
    return { from, to, now, at };
  }, [window.from, window.to, asOf]);

  const tree = useMemo(() => {
    const byId = new Map(rows.map((r) => [r.timeline.issue_id, r]));
    const view = buildGraph({
      nodes: rows.map((r) => ({ id: r.timeline.issue_id, identifier: r.identifier, title: r.title, status: r.timeline.status, parentId: r.parentId ?? null })),
      edges: [],
      collapsed,
    });
    const depthOf = (id: string) => {
      let depth = 0;
      for (let p = byId.get(id)?.parentId; p && byId.has(p) && depth < byId.size; p = byId.get(p)?.parentId) depth++;
      return depth;
    };
    return { view, ordered: view.visible.map((id) => ({ row: byId.get(id)!, depth: depthOf(id) })) };
  }, [rows, collapsed]);

  const icon = (status: string, category: Segment["category"]) => (
    <StatusIcon
      status={status}
      category={category === "unknown" ? categoryOf(status) : category}
      color={colorOf(status)}
      icon={iconOf(status)}
      className="h-3 w-3 shrink-0"
    />
  );

  return (
    <div className="min-h-0 flex-1 overflow-auto">
      <div className="relative" style={{ width: LABEL_W + TRACK_W }}>
        {tree.ordered.map(({ row: { identifier, title, timeline }, depth }) => {
          const open = timeline.actual_end === null;
          // The server closes the last segment at `asOf`, so a done issue's last segment runs to now. The bar ends at
          // `actual_end` instead: each segment end is clamped to it and a segment that becomes empty is dropped.
          const end = open ? 1 : axis.at(timeline.actual_end, axis.now);
          const segments = timeline.segments.filter((s) => axis.at(s.from, axis.from) < end);
          const last = segments[segments.length - 1];
          const first = segments[0];
          // The bar spans the segments; an unknown `from` (history before the records) starts at the window edge.
          const left = first ? axis.at(first.from, axis.from) : 0;
          const right = last ? Math.min(end, axis.at(last.to, axis.now)) : 0;
          const clippedStart = !!first?.clipped_start;
          return (
            <div key={timeline.issue_id + identifier} data-testid="gantt-row" data-issue-id={timeline.issue_id} data-depth={depth} className="flex items-center border-b" style={{ height: ROW_H }}>
              <div className="flex shrink-0 items-center gap-1 truncate px-2 text-caption" style={{ width: LABEL_W, paddingLeft: 8 + depth * INDENT_PX }} title={title}>
                {onToggleCollapse && !tree.view.isLeaf(timeline.issue_id) && (
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon-xs"
                    aria-expanded={!collapsed.has(timeline.issue_id)}
                    aria-label={collapsed.has(timeline.issue_id) ? t(($) => $.gantt2.expand) : t(($) => $.gantt2.collapse)}
                    onClick={() => onToggleCollapse(timeline.issue_id)}
                    className="shrink-0"
                  >
                    <ChevronRight className={`!size-3 stroke-[2.5] text-muted-foreground transition-transform motion-reduce:transition-none ${collapsed.has(timeline.issue_id) ? "" : "rotate-90"}`} />
                  </Button>
                )}
                <span className="font-mono text-muted-foreground">{identifier}</span> <span className="truncate">{title}</span>
              </div>
              <div className="relative h-full" style={{ width: TRACK_W }}>
                {last && (
                  <div
                    data-gantt-bar=""
                    data-open={open}
                    data-clipped-start={clippedStart}
                    className="absolute top-1.5 flex h-5 overflow-hidden rounded-sm border"
                    style={{ left: pct(left), width: pct(right - left) }}
                  >
                    {clippedStart && (
                      <span aria-label={t(($) => $.gantt2.clipped_start)} title={t(($) => $.gantt2.clipped_start)} className="absolute left-0 top-0 z-10 px-0.5 text-caption leading-5">
                        ‹
                      </span>
                    )}
                    {segments.map((s, i) => {
                      const a = axis.at(s.from, axis.from);
                      const b = Math.min(end, axis.at(s.to, axis.now));
                      const px = (b - a) * TRACK_W;
                      const label = statusLabel(s.status);
                      return (
                        <span
                          key={s.row_id ?? `${s.status}|${s.from}|${i}`}
                          data-testid="gantt-segment"
                          data-status={s.status}
                          data-category={s.category}
                          className="flex items-center gap-1 overflow-hidden bg-muted px-0.5 text-caption"
                          style={{ width: pct((b - a) / Math.max(right - left, 1e-9)) }}
                          title={label}
                        >
                          {icon(s.status, s.category)}
                          <span className={px >= LABEL_MIN_PX ? "truncate" : "sr-only"}>{label}</span>
                        </span>
                      );
                    })}
                    {!open && last && (
                      <span
                        data-testid="gantt-end-icon"
                        role="img"
                        aria-label={statusLabel(timeline.status)}
                        className="absolute right-0.5 top-1 z-10"
                      >
                        {icon(timeline.status, categoryOf(timeline.status))}
                      </span>
                    )}
                  </div>
                )}
              </div>
            </div>
          );
        })}
        <div
          aria-label={t(($) => $.gantt2.now)}
          className="pointer-events-none absolute top-0 h-full border-l border-destructive"
          style={{ left: LABEL_W + axis.at(asOf, axis.now) * TRACK_W }}
        />
      </div>
    </div>
  );
}

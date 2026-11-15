"use client";

// Gantt v2 actual bars (CHE-4312 G1). Design lock: design/timeline-views/approved (Direction B).
// Presentational: one bar per issue, from the server's actual start to its actual end (or now), split into
// status segments. Every segment carries its status icon and a text label, so colour is never the only signal.
// Parent/child rows (CHE-4355 G3): row order and collapse come from K1 `buildGraph`; the collapse set is owned by the caller
// (view store `collapsedIssues.gantt`). G2 adds start_date/due_date markers and the incomplete-history rows (`history: none`
// and `partial`). Wakeup arrows (G4): observed firings only, `cross` and `aggregate_children`; the other classes draw nothing here.
// Retarget to the visible parent is K1 `buildGraph` (no Gantt-private code); each firing keeps its own arrow via the edge `id`. Not registered in the view-mode popup yet (G5).
import { useId, useMemo } from "react";
import { buildGraph, type TimelineEdge } from "@multica/core/issues/graph-core";
import { Button } from "@multica/ui/components/ui/button";
import { ChevronRight } from "@multica/ui/icons";
import type { IssueTimelineResponse, TimelineWakeupEvent } from "@multica/core/api/issue-graph-schema";
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
  /** Observed wakeup firings (server `wakeup_events`). Only `cross` and `aggregate_children` draw arrows. */
  wakeupEvents?: readonly TimelineWakeupEvent[];
}

const LABEL_W = 220;
const ROW_H = 32;
/** A segment narrower than this keeps its icon and moves its text label to assistive tech only. */
const LABEL_MIN_PX = 72;
const TRACK_W = 900;

/** Diagonal hatch for the part of a bar that lies before C1 recorded history. */
const HATCH = "repeating-linear-gradient(45deg, transparent 0 4px, rgba(127,127,127,.35) 4px 6px)";
const px_ok = (fraction: number) => fraction * TRACK_W >= LABEL_MIN_PX;

const pct = (n: number) => `${+(n * 100).toFixed(4)}%`;

const NO_COLLAPSE: ReadonlySet<string> = new Set();
const INDENT_PX = 12;
const NO_EVENTS: readonly TimelineWakeupEvent[] = [];

export function Gantt2Bars({ rows, window, asOf, collapsed = NO_COLLAPSE, onToggleCollapse, wakeupEvents = NO_EVENTS }: Gantt2BarsProps) {
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
    /** A date marker draws only inside the window; outside it the row shows nothing for that date. */
    const inWindow = (iso: string | null) => !!iso && Date.parse(iso) >= from && Date.parse(iso) < to;
    return { from, to, now, at, inWindow };
  }, [window.from, window.to, asOf]);

  const tree = useMemo(() => {
    const byId = new Map(rows.map((r) => [r.timeline.issue_id, r]));
    const eventOf = new Map(wakeupEvents.map((e) => [e.id, e]));
    const lastChild = new Map<string, string>();
    for (const r of rows) if (r.parentId && byId.has(r.parentId)) lastChild.set(r.parentId, r.timeline.issue_id);
    // `aggregate_children` records counts, not child ids: the arrow starts at the target's child group (its last child row), never one per child.
    const edges: TimelineEdge[] = wakeupEvents.flatMap((e) => {
      const source = e.class === "cross" ? e.source_issue_id : e.class === "aggregate_children" ? lastChild.get(e.target_issue_id) : null;
      return source && byId.has(source) && byId.has(e.target_issue_id) && axis.inWindow(e.at)
        ? [{ type: "wakeup" as const, source, target: e.target_issue_id, id: e.id }]
        : [];
    });
    const view = buildGraph({
      nodes: rows.map((r) => ({ id: r.timeline.issue_id, identifier: r.identifier, title: r.title, status: r.timeline.status, parentId: r.parentId ?? null })),
      edges,
      collapsed,
    });
    const depthOf = (id: string) => {
      let depth = 0;
      for (let p = byId.get(id)?.parentId; p && byId.has(p) && depth < byId.size; p = byId.get(p)?.parentId) depth++;
      return depth;
    };
    const rowIndex = new Map(view.visible.map((id, i) => [id, i]));
    const arrows = view.edges.flatMap((edge) => {
      const event = eventOf.get(edge.id ?? "");
      return event ? [{ edge, event, from: rowIndex.get(edge.source)!, to: rowIndex.get(edge.target)! }] : [];
    });
    return { view, arrows, ordered: view.visible.map((id) => ({ row: byId.get(id)!, depth: depthOf(id) })) };
  }, [rows, collapsed, wakeupEvents, axis]);
  const arrowhead = useId();

  const icon = (status: string, category: Segment["category"]) => (
    <StatusIcon
      status={status}
      category={category === "unknown" ? categoryOf(status) : category}
      color={colorOf(status)}
      icon={iconOf(status)}
      className="h-3 w-3 shrink-0"
    />
  );

  const marker = (kind: "start" | "due", iso: string, glyph: string) => {
    const name = kind === "start" ? t(($) => $.gantt2.start_marker) : t(($) => $.gantt2.due_marker);
    return (
      <span
        data-testid={`gantt-marker-${kind}`}
        role="img"
        aria-label={name}
        title={`${name}: ${iso.slice(0, 10)}`}
        className="absolute top-1.5 z-10 -translate-x-1/2 text-caption leading-5"
        style={{ left: pct(axis.at(iso, axis.from)) }}
      >
        {glyph}
      </span>
    );
  };

  return (
    <div className="min-h-0 flex-1 overflow-auto">
      <div className="relative" style={{ width: LABEL_W + TRACK_W }}>
        {tree.ordered.map(({ row: { identifier, title, timeline }, depth }) => {
          const open = timeline.actual_end === null;
          // The server closes the last segment at `asOf`, so a done issue's last segment runs to now. The bar ends at
          // `actual_end` instead: each segment end is clamped to it and a segment that becomes empty is dropped.
          const end = open ? 1 : axis.at(timeline.actual_end, axis.now);
          // An unknown `from` (partial history, "before records") starts at max(presence_from, window from); `at` clamps to the window.
          const startOf = (s: Segment) => axis.at(s.from ?? timeline.presence_from, axis.from);
          const segments = timeline.segments.filter((s) => startOf(s) < end);
          const last = segments[segments.length - 1];
          const first = segments[0];
          const left = first ? startOf(first) : 0;
          const right = last ? Math.min(end, axis.at(last.to, axis.now)) : 0;
          const clippedStart = !!first?.clipped_start;
          const startMarker = axis.inWindow(timeline.start_date) ? timeline.start_date : null;
          const dueMarker = axis.inWindow(timeline.due_date) ? timeline.due_date : null;
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
                {!last && timeline.history === "none" && (
                  <span className="absolute left-1 top-1.5 italic text-muted-foreground text-caption">{t(($) => $.gantt2.no_history)}</span>
                )}
                {!last && startMarker && dueMarker && (
                  <span
                    data-testid="gantt-marker-span"
                    className="absolute top-4 border-t border-dashed border-muted-foreground"
                    style={{ left: pct(axis.at(startMarker, axis.from)), width: pct(axis.at(dueMarker, axis.from) - axis.at(startMarker, axis.from)) }}
                  />
                )}
                {startMarker && marker("start", startMarker, "▲")}
                {dueMarker && marker("due", dueMarker, "◆")}
                {last && (
                  <div
                    data-gantt-bar=""
                    data-open={open}
                    data-start-known={timeline.start_known}
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
                      const a = startOf(s);
                      const beforeRecords = s.from === null;
                      const b = Math.min(end, axis.at(s.to, axis.now));
                      const labelClass = px_ok(b - a) ? "truncate" : "sr-only";
                      const label = statusLabel(s.status);
                      return (
                        <span
                          key={s.row_id ?? `${s.status}|${s.from}|${i}`}
                          data-testid="gantt-segment"
                          data-status={s.status}
                          data-category={s.category}
                          data-before-records={beforeRecords ? "true" : undefined}
                          className="flex items-center gap-1 overflow-hidden bg-muted px-0.5 text-caption"
                          style={{
                            width: pct((b - a) / Math.max(right - left, 1e-9)),
                            ...(beforeRecords ? { backgroundImage: HATCH } : null),
                          }}
                          title={label}
                        >
                          {icon(s.status, s.category)}
                          <span className={labelClass}>{label}</span>
                          {beforeRecords && <span className={`${labelClass} italic`}>{t(($) => $.gantt2.before_records)}</span>}
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
        {tree.arrows.length > 0 && (
          <>
            <svg className="pointer-events-none absolute left-0 top-0" width={LABEL_W + TRACK_W} height={tree.ordered.length * ROW_H}>
              <defs>
                <marker id={arrowhead} markerWidth="6" markerHeight="6" refX="5" refY="3" orient="auto">
                  <path d="M0,0 L6,3 L0,6 Z" className="fill-foreground" />
                </marker>
              </defs>
              {tree.arrows.map(({ edge, event, from, to }) => {
                const x = LABEL_W + axis.at(event.at, axis.now) * TRACK_W;
                const name = event.class === "aggregate_children" ? t(($) => $.gantt2.wakeup_children_done) : t(($) => $.gantt2.wakeup_cross);
                const label = `${name}: ${tree.ordered[from]!.row.identifier} → ${tree.ordered[to]!.row.identifier}`;
                return (
                  <line
                    key={event.id}
                    data-testid="gantt-arrow"
                    data-class={event.class}
                    data-source={edge.source}
                    data-target={edge.target}
                    role="img"
                    aria-label={label}
                    x1={x}
                    x2={x}
                    y1={from * ROW_H + ROW_H / 2}
                    y2={to * ROW_H + ROW_H / 2 + (to > from ? -6 : 6)}
                    className="stroke-foreground"
                    strokeWidth={1.5}
                    strokeDasharray={event.class === "aggregate_children" ? "4 2" : undefined}
                    markerEnd={`url(#${arrowhead})`}
                  >
                    <title>{label}</title>
                  </line>
                );
              })}
            </svg>
            {tree.arrows.flatMap(({ edge, event, from, to }) =>
              (["source", "target"] as const)
                .filter((end) => (end === "source" ? edge.sourceInside : edge.targetInside))
                .map((end) => (
                  <span
                    key={`${event.id}-${end}`}
                    data-testid="gantt-target-inside"
                    role="img"
                    aria-label={t(($) => $.gantt2.target_inside)}
                    title={t(($) => $.gantt2.target_inside)}
                    className="pointer-events-none absolute z-10 -translate-x-1/2 -translate-y-1/2 rounded-full border bg-background px-1 text-caption leading-4"
                    style={{ left: LABEL_W + axis.at(event.at, axis.now) * TRACK_W, top: (end === "source" ? from : to) * ROW_H + ROW_H / 2 }}
                  >
                    ⊕
                  </span>
                )),
            )}
          </>
        )}
        <div
          aria-label={t(($) => $.gantt2.now)}
          className="pointer-events-none absolute top-0 h-full border-l border-destructive"
          style={{ left: LABEL_W + axis.at(asOf, axis.now) * TRACK_W }}
        />
      </div>
    </div>
  );
}

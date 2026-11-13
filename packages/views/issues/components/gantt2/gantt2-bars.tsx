"use client";

// Gantt v2 actual bars (CHE-4312 G1). Design lock: design/timeline-views/approved (Direction B).
// Presentational: one bar per issue, from the server's actual start to its actual end (or now), split into
// status segments. Every segment carries its status icon and a text label, so colour is never the only signal.
// G2 adds start_date/due_date markers and the incomplete-history rows (`history: none` and `partial`). Tree rows and wakeups
// are later slices (G3, G4). Not registered in the view-mode popup yet (G5).
import { useMemo } from "react";
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
}
export interface Gantt2BarsProps {
  rows: Gantt2Row[];
  /** The timeline window; the x axis spans it. */
  window: { from: string; to: string };
  /** Server "now" of the page chain; an open bar ends here. */
  asOf: string;
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

export function Gantt2Bars({ rows, window, asOf }: Gantt2BarsProps) {
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
        {rows.map(({ identifier, title, timeline }) => {
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
            <div key={timeline.issue_id + identifier} className="flex items-center border-b" style={{ height: ROW_H }}>
              <div className="shrink-0 truncate px-2 text-caption" style={{ width: LABEL_W }} title={title}>
                <span className="font-mono text-muted-foreground">{identifier}</span> {title}
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
        <div
          aria-label={t(($) => $.gantt2.now)}
          className="pointer-events-none absolute top-0 h-full border-l border-destructive"
          style={{ left: LABEL_W + axis.at(asOf, axis.now) * TRACK_W }}
        />
      </div>
    </div>
  );
}

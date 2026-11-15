"use client";

import { useMemo, useState } from "react";
import { ArrowRight } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Slider } from "@multica/ui/components/ui/slider";
import type { IssueWorkflow, Project, TimelineEntry } from "@multica/core/types";
import { useLocale, useT } from "../../i18n";
import { WorkflowGraph } from "../../workflows/workflow-graph";
import { buildStatusHistory, type StatusHistoryItem } from "./status-history";

export interface ResolvedStatus {
  label: string;
  /** True when the workspace catalog no longer knows the status. */
  removed: boolean;
}

const KNOWN_CAUSES = [
  "ui_or_api",
  "workflow_edge",
  "workflow_remap",
  "pr_merged",
  "ci_advance",
  "run_budget",
  "decision",
  "watchdog",
  "task_reset",
  "qoder",
  "duplicate",
  "system",
] as const;
type KnownCause = (typeof KNOWN_CAUSES)[number];

function isKnownCause(kind: string): kind is KnownCause {
  return (KNOWN_CAUSES as readonly string[]).includes(kind);
}

/**
 * Every status change on the issue, oldest first, with what triggered it. The
 * scrubber walks the list one change at a time; `entries` is the issue
 * timeline and `truncated` is the server's `X-Timeline-Truncated` signal for
 * activity rows.
 */
export function StatusHistoryDialog({
  open,
  onOpenChange,
  entries,
  truncated,
  resolveStatus,
  workflow = null,
  project = null,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  entries: readonly TimelineEntry[];
  truncated: boolean;
  resolveStatus: (statusKey: string) => ResolvedStatus;
  /** When given, the graph above the list shows the selected change. */
  workflow?: Pick<IssueWorkflow, "steps"> | null;
  project?: Pick<Project, "lead_type" | "lead_id"> | null;
}) {
  const { t } = useT("issues");
  const locale = useLocale();
  const items = useMemo(() => buildStatusHistory(entries), [entries]);
  const dateFormat = useMemo(
    () => new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeStyle: "short" }),
    [locale],
  );
  // null follows the latest change, so a refetch that appends a row moves the
  // selection with it until the person scrubs away.
  const [picked, setPicked] = useState<number | null>(null);
  const last = items.length - 1;
  const selected = picked === null ? last : Math.min(picked, last);

  const current = items[selected];
  // A legacy row says nothing about how it moved, so it never claims an edge;
  // the graph only marks its target node. A recorded move highlights its edge
  // when the workflow still has one between the two statuses.
  const highlightEdge = current?.recorded ? { from: current.from, to: current.to } : null;
  const move = (delta: number) => setPicked(Math.max(0, Math.min(last, selected + delta)));
  // The scrubber handles its own arrows; handling them here too would move twice.
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.defaultPrevented || e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
    if ((e.target as HTMLElement).closest('[role="slider"]')) return;
    if (e.key === "ArrowLeft") move(-1);
    else if (e.key === "ArrowRight") move(1);
    else return;
    e.preventDefault();
  };

  const statusText = (key: string): string => {
    const { label, removed } = resolveStatus(key);
    return removed ? t(($) => $.status_history.removed, { status: label }) : label;
  };
  const causeText = (item: StatusHistoryItem): string => {
    if (!item.recorded || item.causeKind === null) {
      return t(($) => $.status_history.trigger_not_recorded);
    }
    return isKnownCause(item.causeKind)
      ? t(($) => $.status_history.cause[item.causeKind as KnownCause])
      : item.causeKind;
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className={cn("max-w-lg", workflow && "sm:max-w-2xl")} onKeyDown={onKeyDown}>
        <DialogHeader>
          <DialogTitle>{t(($) => $.status_history.title)}</DialogTitle>
          <DialogDescription>{t(($) => $.status_history.description)}</DialogDescription>
        </DialogHeader>
        {truncated && (
          <p role="status" className="rounded-md bg-muted px-3 py-2 text-xs text-muted-foreground">
            {t(($) => $.status_history.truncated)}
          </p>
        )}
        {items.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t(($) => $.status_history.empty)}</p>
        ) : (
          <>
            {items.length > 1 && (
              <div className="flex items-center gap-3">
                <Slider
                  aria-label={t(($) => $.status_history.scrubber_label)}
                  min={0}
                  max={last}
                  step={1}
                  value={[selected]}
                  onValueChange={(v) => setPicked(Array.isArray(v) ? (v[0] ?? 0) : v)}
                />
                <span className="shrink-0 text-xs tabular-nums text-muted-foreground">
                  {t(($) => $.status_history.position, { current: selected + 1, total: items.length })}
                </span>
              </div>
            )}
            {workflow && current && (
              <WorkflowGraph
                workflow={workflow}
                currentStatus={current.to}
                highlightEdge={highlightEdge}
                project={project}
              />
            )}
            <ol className="max-h-80 space-y-1 overflow-y-auto">
              {items.map((item, i) => (
                <li
                  key={item.id}
                  aria-current={i === selected ? "step" : undefined}
                  className={cn("rounded-md px-3 py-2 text-sm", i === selected && "bg-accent")}
                  ref={(el) => {
                    if (el && i === selected) el.scrollIntoView?.({ block: "nearest" });
                  }}
                >
                  <div className="flex items-center gap-1.5">
                    <span>{statusText(item.from)}</span>
                    <ArrowRight className="size-3 text-muted-foreground" aria-hidden />
                    <span>{statusText(item.to)}</span>
                  </div>
                  <div className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground">
                    <span>{causeText(item)}</span>
                    <span>{dateFormat.format(new Date(item.createdAt))}</span>
                    {item.handoffCommentId && (
                      <a
                        href={`#comment-${item.handoffCommentId}`}
                        className="underline underline-offset-2"
                        onClick={() => onOpenChange(false)}
                      >
                        {t(($) => $.status_history.handoff_comment)}
                      </a>
                    )}
                    {item.refInvalid && <span>{t(($) => $.status_history.handoff_invalid)}</span>}
                  </div>
                </li>
              ))}
            </ol>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

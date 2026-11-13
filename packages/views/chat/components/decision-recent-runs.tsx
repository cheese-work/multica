"use client";

import { useQuery } from "@tanstack/react-query";
import { issueTasksOptions } from "@multica/core/issues/queries";
import { useT } from "../../i18n";

const RECENT = 5;

const time = new Intl.DateTimeFormat("en-GB", {
  timeZone: "Asia/Bangkok",
  day: "2-digit",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});

/**
 * The issue's last 5 runs, newest first, on a run-budget card: what the
 * budget was spent on. Context only; the approval never depends on it.
 * Hidden while loading, on error, and for an issue with no runs.
 */
export function DecisionRecentRuns({ issueId }: { issueId: string }) {
  const { t } = useT("chat");
  const { data } = useQuery(issueTasksOptions(issueId));
  const runs = [...(data ?? [])]
    .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at))
    .slice(0, RECENT);
  if (runs.length === 0) return null;
  const status = (s: string) => {
    switch (s) {
      case "completed": return t(($) => $.decision.recent_runs_status.completed);
      case "failed": return t(($) => $.decision.recent_runs_status.failed);
      case "cancelled": return t(($) => $.decision.recent_runs_status.cancelled);
      case "running": return t(($) => $.decision.recent_runs_status.running);
      default: return t(($) => $.decision.recent_runs_status.other);
    }
  };
  return (
    <section data-testid="decision-recent-runs" aria-label={t(($) => $.decision.recent_runs)} className="space-y-1 rounded-lg border border-surface-border px-2 py-1.5 text-label">
      <h4 className="font-medium">{t(($) => $.decision.recent_runs)}</h4>
      <ul className="space-y-0.5">
        {runs.map((run) => (
          <li key={run.id} className="flex flex-wrap items-baseline gap-x-2 text-muted-foreground">
            <span className={run.status === "failed" ? "text-destructive" : "text-foreground"}>{status(run.status)}</span>
            <time dateTime={run.created_at}>{time.format(new Date(run.created_at))} ICT</time>
          </li>
        ))}
      </ul>
    </section>
  );
}

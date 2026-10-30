"use client";

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { cn } from "@multica/ui/lib/utils";
import { useWorkspaceId } from "@multica/core/hooks";
import { chatSessionUsageOptions } from "@multica/core/chat/queries";
import { summarizeChatSessionUsage } from "@multica/core/chat/usage";
import type { ChatSession, ChatUsageTokens } from "@multica/core/types";
import { useT } from "../../i18n";
import { formatDuration } from "../../dashboard/utils";
import { formatTokens, formatUsd } from "../../runtimes/utils";

/**
 * Statistics for one DM (session only; the whole-conversation total is a
 * separate layer). The numbers come from the usage endpoint; this dialog only
 * lays them out. A value that was never recorded shows a dash, never zero.
 */
export function ChatSessionStatsDialog({
  sessionId,
  open,
  onOpenChange,
  sessions = [],
  onOpenSession,
}: {
  sessionId: string | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Resolves the titles of the related DMs. */
  sessions?: ChatSession[];
  onOpenSession?: (session: ChatSession) => void;
}) {
  const { t } = useT("chat");
  const wsId = useWorkspaceId();
  const usage = useQuery(chatSessionUsageOptions(wsId, open ? sessionId : null));

  // A response the schema could not read falls back to an empty payload with
  // no session id. That is a failure to load, not a session with no usage.
  const data = usage.data && usage.data.session_id === sessionId ? usage.data : null;
  const summary = useMemo(() => (data ? summarizeChatSessionUsage(data) : null), [data]);

  const failed = usage.isError || (usage.isSuccess && !data);
  const refreshFailed = failed && !!summary;
  const loadFailed = failed && !summary;

  const titleOf = (id: string | null) => {
    if (!id) return null;
    const session = sessions.find((s) => s.id === id);
    return {
      session,
      title: session?.title?.trim() || t(($) => $.window.untitled),
    };
  };
  const related = [
    { label: t(($) => $.stats.continued_from), target: titleOf(data?.continued_from_session_id ?? null) },
    { label: t(($) => $.stats.continued_into), target: titleOf(data?.continued_into_session_id ?? null) },
    { label: t(($) => $.stats.forked_from), target: titleOf(data?.forked_from_session_id ?? null) },
  ].filter((r) => r.target);

  const tokenKinds = (tokens: ChatUsageTokens) => [
    { key: "input", label: t(($) => $.stats.col_input), value: tokens.input },
    { key: "output", label: t(($) => $.stats.col_output), value: tokens.output },
    { key: "cache_read", label: t(($) => $.stats.col_cache_read), value: tokens.cache_read },
    { key: "cache_write", label: t(($) => $.stats.col_cache_write), value: tokens.cache_write },
  ];

  const retry = (
    <Button variant="outline" size="sm" onClick={() => void usage.refetch()}>
      {t(($) => $.stats.retry)}
    </Button>
  );

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-testid="chat-stats-dialog" showCloseButton={false} className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t(($) => $.stats.title)}</DialogTitle>
          <p className="text-caption text-muted-foreground">{t(($) => $.stats.scope)}</p>
        </DialogHeader>

        {!usage.isFetched && !summary ? (
          <div data-testid="chat-stats-state-loading" className="grid gap-3">
            <Skeleton className="h-8 w-28" />
            <Skeleton className="h-4 w-56" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-full" />
          </div>
        ) : loadFailed ? (
          <div
            data-testid="chat-stats-state-error"
            role="alert"
            className="flex flex-col items-start gap-3 text-body"
          >
            <p>{t(($) => $.stats.error)}</p>
            {retry}
          </div>
        ) : summary ? (
          <div
            data-testid="chat-stats-body"
            data-stale={refreshFailed ? "true" : undefined}
            className="grid gap-4"
          >
            {refreshFailed ? (
              <div
                data-testid="chat-stats-state-offline"
                role="status"
                className="flex items-start justify-between gap-3 rounded-md bg-muted px-3 py-2 text-caption"
              >
                <p>
                  {t(($) => $.stats.offline, {
                    time: new Date(usage.dataUpdatedAt).toLocaleTimeString([], {
                      hour: "2-digit",
                      minute: "2-digit",
                    }),
                  })}
                </p>
                {retry}
              </div>
            ) : null}

            <div className={cn("grid gap-4", refreshFailed && "opacity-60")}>
              <div className="grid gap-1">
                <div className="flex items-baseline gap-2">
                  <span data-testid="chat-stats-cost" className="text-title font-semibold">
                    {summary.costUsd === null ? t(($) => $.stats.no_value) : formatUsd(summary.costUsd)}
                  </span>
                  <span className="text-caption text-muted-foreground">
                    {summary.costUsd === null
                      ? t(($) => $.stats.no_cost)
                      : summary.partial
                        ? `${t(($) => $.stats.estimated)} · ${t(($) => $.stats.partial)}`
                        : t(($) => $.stats.estimated)}
                  </span>
                </div>
                {summary.hasUsage ? (
                  <p className="text-caption text-muted-foreground">
                    {t(($) => $.stats.tokens, { tokens: formatTokens(summary.totalTokens) })}
                    {summary.unreportedRuns > 0
                      ? ` · ${t(($) => $.stats.usage_missing, { count: summary.unreportedRuns })}`
                      : ""}
                  </p>
                ) : null}
                {summary.hasUsage ? (
                  <dl
                    data-testid="chat-stats-token-breakdown"
                    className="flex flex-wrap gap-x-4 gap-y-0.5 text-caption text-muted-foreground"
                  >
                    {tokenKinds(summary.tokenKinds).map(({ key, label, value }) => (
                      <div key={key} className="flex gap-1">
                        <dt>{label}</dt>
                        <dd className="tabular-nums">{formatTokens(value)}</dd>
                      </div>
                    ))}
                  </dl>
                ) : null}
                {summary.runs.inProgress > 0 ? (
                  <p className="text-caption text-muted-foreground">
                    {t(($) => $.stats.in_progress, { count: summary.runs.inProgress })}
                  </p>
                ) : null}
              </div>

              {summary.runs.completed + summary.runs.failed + summary.runs.cancelled + summary.runs.inProgress === 0 ? (
                <p data-testid="chat-stats-state-empty" className="text-body text-muted-foreground">
                  {t(($) => $.stats.no_runs)}
                </p>
              ) : (
                <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1 text-body">
                  <dt className="text-muted-foreground">{t(($) => $.stats.runs)}</dt>
                  <dd>
                    {t(($) => $.stats.runs_value, {
                      completed: summary.runs.completed,
                      failed: summary.runs.failed,
                    })}
                    {summary.runs.cancelled > 0
                      ? t(($) => $.stats.runs_cancelled, { cancelled: summary.runs.cancelled })
                      : ""}
                  </dd>
                  <dt className="text-muted-foreground">{t(($) => $.stats.run_time)}</dt>
                  <dd>
                    {formatDuration(summary.totalRunMs / 1000, t(($) => $.stats.less_than_a_minute))}
                  </dd>
                </dl>
              )}

              <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1 text-body">
                <dt className="text-muted-foreground">{t(($) => $.stats.messages)}</dt>
                <dd>
                  {t(($) => $.stats.messages_value, {
                    user: summary.messages.user,
                    agent: summary.messages.assistant,
                  })}
                </dd>
              </dl>

              {summary.models.length > 0 ? (
                <table className="w-full text-body">
                  <caption className="pb-1 text-left text-caption text-muted-foreground">
                    {t(($) => $.stats.by_model)}
                  </caption>
                  <thead className="text-caption text-muted-foreground">
                    <tr>
                      <th scope="col" className="py-1 text-left font-normal">
                        {t(($) => $.stats.col_model)}
                      </th>
                      {tokenKinds(summary.tokenKinds).map(({ key, label }) => (
                        <th key={key} scope="col" className="py-1 text-right font-normal">
                          {label}
                        </th>
                      ))}
                      <th scope="col" className="py-1 text-right font-normal">
                        {t(($) => $.stats.col_cost)}
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {summary.models.map((row) => (
                      <tr key={row.model}>
                        <th scope="row" className="py-1 text-left font-normal break-all">
                          {row.model}
                        </th>
                        {tokenKinds(row.tokenKinds).map(({ key, value }) => (
                          <td key={key} className="py-1 text-right tabular-nums">
                            {formatTokens(value)}
                          </td>
                        ))}
                        <td className="py-1 text-right tabular-nums">
                          {row.costUsd === null ? t(($) => $.stats.no_value) : formatUsd(row.costUsd)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              ) : null}

              {related.length > 0 ? (
                <section className="grid gap-1">
                  <h3 className="text-caption text-muted-foreground">{t(($) => $.stats.related)}</h3>
                  <ul className="grid gap-1 text-body">
                    {related.map(({ label, target }) => (
                      <li key={label}>
                        {label}{" "}
                        {target?.session && onOpenSession ? (
                          <Button
                            variant="link"
                            className="h-auto p-0"
                            onClick={() => {
                              onOpenChange(false);
                              onOpenSession(target.session as ChatSession);
                            }}
                          >
                            {target.title}
                          </Button>
                        ) : (
                          <span>{target?.title}</span>
                        )}
                      </li>
                    ))}
                  </ul>
                </section>
              ) : null}
            </div>
          </div>
        ) : null}

        <DialogFooter>
          <Button
            variant="outline"
            data-testid="chat-stats-close"
            onClick={() => onOpenChange(false)}
          >
            {t(($) => $.stats.close)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

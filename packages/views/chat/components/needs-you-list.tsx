"use client";

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { openDecisionRequestsOptions } from "@multica/core/chat/queries";
import { AppLink } from "../../navigation";
import { useLocale, useT } from "../../i18n";
import { formatMessageDateTime } from "./message-time";

function requestAge(createdAt: string, now: number, locale: string): string {
  const elapsed = now - Date.parse(createdAt);
  if (!Number.isFinite(elapsed)) return createdAt;
  const minutes = Math.max(0, Math.floor(elapsed / 60_000));
  const formatter = new Intl.RelativeTimeFormat(locale, { numeric: "always" });
  if (minutes < 60) return formatter.format(-minutes, "minute");
  if (minutes < 1440) return formatter.format(-Math.floor(minutes / 60), "hour");
  return formatter.format(-Math.floor(minutes / 1440), "day");
}

export function NeedsYouList() {
  const wsId = useWorkspaceId();
  const slug = useWorkspaceSlug();
  const locale = useLocale();
  const { t } = useT("chat");
  const query = useQuery(openDecisionRequestsOptions(wsId));
  const requests = query.data ?? [];
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(timer);
  }, []);

  if (!slug) return null;

  return (
    <section aria-label={t(($) => $.needs_you.title)} className="sticky top-0 z-10 border-b border-surface-border bg-background pb-2">
      <h3 className="flex items-center justify-between px-2 py-2 text-caption font-semibold">
        {t(($) => $.needs_you.title)}
        {" "}
        <span className="tabular-nums text-muted-foreground">{query.data ? requests.length : "—"}</span>
      </h3>
      {query.isPending && <p role="status" className="px-2 text-caption text-muted-foreground">{t(($) => $.needs_you.loading)}</p>}
      {query.isError && (
        <div role="alert" className="px-2 text-caption text-destructive">
          {t(($) => $.needs_you.error)}
          <button type="button" onClick={() => void query.refetch()} className="ml-2 min-h-9 underline focus-visible:outline-2 focus-visible:outline-ring">
            {t(($) => $.needs_you.retry)}
          </button>
        </div>
      )}
      {!query.isPending && !query.isError && requests.length === 0 && (
        <p className="px-2 text-caption text-muted-foreground">{t(($) => $.needs_you.empty)}</p>
      )}
      <ul className="max-h-64 overflow-y-auto">
        {requests.map((request) => {
          const headline = (request.ask.match(/\*\*Decision\*\*\s*\n([\s\S]*?)(?:\n\s*\n|$)/)?.[1] ?? request.ask)
            .replace(/[#*`>~]/g, "").replace(/\s+/g, " ").trim();
          const params = new URLSearchParams({ session: request.chat_session_id });
          if (request.message_id) params.set("message", request.message_id);
          return (
            <li key={request.id}>
              <AppLink href={`${paths.workspace(slug).chat()}?${params}`} className="flex min-h-11 flex-col gap-1 rounded-md px-2 py-2 text-caption outline-none hover:bg-accent/50 focus-visible:ring-1 focus-visible:ring-ring">
                <span className="flex items-center gap-2 text-muted-foreground">
                  <span className="font-medium text-foreground">{request.issue_identifier}</span>
                  <span className="min-w-0 flex-1 truncate">{request.asking_agent_name || request.asking_agent_id}</span>
                  <time dateTime={request.created_at} title={formatMessageDateTime(request.created_at, locale)} className="shrink-0">
                    {requestAge(request.created_at, now, locale)}
                  </time>
                </span>
                <span className="line-clamp-2 font-medium">{headline}</span>
                <span className="text-muted-foreground">{request.escalation_class.replaceAll("_", " ")}</span>
              </AppLink>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

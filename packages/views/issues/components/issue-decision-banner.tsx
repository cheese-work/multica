"use client";

import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { issueDecisionCardsOptions, openDecisionRequestsOptions } from "@multica/core/chat/queries";
import { issueTasksOptions } from "@multica/core/issues/queries";
import { agentListOptions } from "@multica/core/workspace/queries";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { DecisionRequestCard } from "../../chat/components/decision-request-card";

const ACTIVE_TASK = new Set(["queued", "dispatched", "waiting_local_directory", "deferred", "running"]);

const ict = (iso: string) => {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return `${new Intl.DateTimeFormat("en-GB", {
    timeZone: "Asia/Bangkok", day: "2-digit", month: "short", year: "numeric",
    hour: "2-digit", minute: "2-digit", hourCycle: "h23",
  }).format(date)} ICT`;
};

/** True while the card still asks the viewer for something (shown expanded). */
const needsAnswer = (card: RequestCard) =>
  card.state === "open" || (card.state === "answered" && card.answer?.state === "delivered");

/**
 * Decision cards of the viewer on this issue: open ones and used ones (the used ones fold),
 * or why `need_decision` has none yet.
 */
export function IssueDecisionBanner({ issueId, status, ownerAgentId }: { issueId: string; status: string; ownerAgentId?: string | null }) {
  const wsId = useWorkspaceId();
  const { t } = useT("chat");
  const { data, isSuccess, isError, isFetching, refetch } = useQuery(openDecisionRequestsOptions(wsId));
  const usedQuery = useQuery(issueDecisionCardsOptions(wsId, issueId));
  const waiting = isSuccess && status === "need_decision" && !data.some((r) => r.issue_id === issueId);
  const tasksQuery = useQuery({ ...issueTasksOptions(issueId), enabled: waiting });
  const { data: agents = [] } = useQuery({ ...agentListOptions(wsId), enabled: waiting });
  const used = usedQuery.data ?? [];
  const usedIds = new Set(used.map((card) => card.id));
  const current: RequestCard[] = [
    ...(data ?? [])
      .filter((request) => request.issue_id === issueId && !usedIds.has(request.id))
      .map((request): RequestCard => ({ ...request, state: "open" })),
    ...used,
  ];
  // One answer refreshes two lists, and they land one after the other. A card that left
  // the open list stays mounted until the used list has it, so it folds in place.
  const busy = isFetching || usedQuery.isFetching;
  const shown = useRef(new Map<string, RequestCard>());
  const cards = busy ? [...current, ...[...shown.current.values()].filter((card) => !current.some((c) => c.id === card.id))] : current;
  useEffect(() => {
    if (!busy) shown.current = new Map(current.map((card) => [card.id, card]));
  });

  if (cards.length > 0) {
    const pending = cards.filter(needsAnswer).length;
    const title = pending > 0 ? t(($) => $.decision_banner.title) : t(($) => $.decision_banner.title_used);
    const answered = cards.filter((card) => card.state === "answered").length;
    return (
      <section
        id="issue-decision-banner"
        aria-label={title}
        className={cn("mt-8 space-y-2 rounded-xl border p-2", pending > 0 ? "border-warning/40 bg-warning/5" : "border-surface-border bg-surface/50")}
      >
        <h2 className="flex items-center gap-2 px-1 text-label font-semibold">
          {title}
          {pending > 1 && <span className="rounded-full bg-warning/10 px-2 text-micro tabular-nums text-warning">{pending}</span>}
          {pending === 0 && answered > 0 && (
            <span className="rounded-full bg-muted px-2 text-micro font-normal tabular-nums text-muted-foreground">{t(($) => $.decision_banner.answered_count, { number: answered })}</span>
          )}
        </h2>
        {cards.map((card) => <DecisionRequestCard key={card.id} card={card} inIssue />)}
      </section>
    );
  }
  if (isError && status === "need_decision") {
    return (
      <section aria-label={t(($) => $.decision_banner.title)} role="alert" className="mt-8 rounded-xl border border-warning/40 bg-warning/5 p-3 text-caption">
        <h2 className="text-label font-semibold">{t(($) => $.decision_banner.title)}</h2>
        {t(($) => $.needs_you.error)}
        <button type="button" onClick={() => void refetch()} className="ml-2 min-h-9 underline focus-visible:outline-2 focus-visible:outline-ring">
          {t(($) => $.needs_you.retry)}
        </button>
      </section>
    );
  }
  if (!waiting) return null;

  // Only the owner's runs: another agent (a reviewer) running now is not the sender of the card.
  const active = (tasksQuery.data ?? []).filter((task) => ACTIVE_TASK.has(task.status) && task.agent_id === ownerAgentId);
  const owner = agents.find((agent) => agent.id === ownerAgentId)?.name;
  return (
    <section aria-label={t(($) => $.decision_banner.title)} role="status" className="mt-8 space-y-1 rounded-xl border border-warning/40 bg-warning/5 p-3 text-caption">
      <h2 className="text-label font-semibold">{t(($) => $.decision_banner.title)}</h2>
      <p>{t(($) => $.decision_banner.waiting)}</p>
      {tasksQuery.isSuccess && active.length === 0 && <p className="text-muted-foreground">{t(($) => $.decision_banner.no_run)}</p>}
      {active.map((task) => (
        <p key={task.id} className="text-muted-foreground">
          {owner && `${owner}: `}
          {task.status === "running"
            ? t(($) => $.decision_banner.running_since, { time: ict(task.started_at ?? task.created_at) })
            : t(($) => $.decision_banner.queued_since, { time: ict(task.created_at) })}
        </p>
      ))}
    </section>
  );
}

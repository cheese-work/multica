"use client";

import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { openDecisionRequestsOptions } from "@multica/core/chat/queries";
import { issueTasksOptions } from "@multica/core/issues/queries";
import { agentListOptions } from "@multica/core/workspace/queries";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
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

/** Open decision requests for the viewer on this issue, or why `need_decision` has none yet. */
export function IssueDecisionBanner({ issueId, status, ownerAgentId }: { issueId: string; status: string; ownerAgentId?: string | null }) {
  const wsId = useWorkspaceId();
  const { t } = useT("chat");
  const { data, isSuccess, isError, refetch } = useQuery(openDecisionRequestsOptions(wsId));
  const waiting = isSuccess && status === "need_decision" && !data.some((r) => r.issue_id === issueId);
  const tasksQuery = useQuery({ ...issueTasksOptions(issueId), enabled: waiting });
  const { data: agents = [] } = useQuery({ ...agentListOptions(wsId), enabled: waiting });
  const cards: RequestCard[] = (data ?? [])
    .filter((request) => request.issue_id === issueId)
    .map((request) => ({ ...request, state: "open" }));

  if (cards.length > 0) {
    return (
      <section id="issue-decision-banner" aria-label={t(($) => $.decision_banner.title)} className="mt-8 space-y-2 rounded-xl border border-warning/40 bg-warning/5 p-2">
        <h2 className="flex items-center gap-2 px-1 text-label font-semibold">
          {t(($) => $.decision_banner.title)}
          {cards.length > 1 && <span className="rounded-full bg-warning/10 px-2 text-micro tabular-nums text-warning">{cards.length}</span>}
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

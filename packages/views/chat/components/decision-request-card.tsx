import { MessageSquareQuote } from "lucide-react";
import { useWorkspacePaths } from "@multica/core/paths";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { RichContent } from "../../rich-content";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";
import { AnnotatableAnswer } from "./annotatable-answer";
import { DECISION_REQUEST_SOURCE_PREFIX } from "./chat-annotation-context";

export function DecisionRequestCard({ card }: { card: RequestCard }) {
  const { t } = useT("chat");
  const paths = useWorkspacePaths();
  const state = t(($) => $.decision.request_state[card.state]);
  const timestamp = card.state === "withdrawn" ? card.withdrawn_at || card.created_at : card.created_at;
  const date = new Date(timestamp);
  const validDate = !Number.isNaN(date.getTime());
  const issueHref = paths.issueDetail(card.issue_id);

  return (
    <section
      aria-label={t(($) => $.decision.request_label, { state })}
      data-testid="decision-request-card"
      data-state={card.state}
      className="min-w-0 rounded-xl border border-surface-border bg-surface text-body"
    >
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-surface-border px-3 py-2 text-label">
        <MessageSquareQuote className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className="font-medium">{t(($) => $.decision.request_title)}</span>
        <span className={cn(
          "rounded-full px-2 py-0.5 text-micro font-medium",
          card.state === "open" && "bg-warning/10 text-warning",
          card.state === "answered" && "bg-success/10 text-success",
          card.state === "withdrawn" && "bg-muted text-muted-foreground",
        )}>
          {state}
        </span>
        <AppLink
          href={issueHref}
          className="inline-flex min-h-9 max-w-full items-center break-all rounded-full border border-surface-border px-2 font-medium hover:underline focus-visible:outline-2 focus-visible:outline-ring"
          data-testid="decision-request-source-issue"
        >
          {card.issue_identifier || card.issue_id}
        </AppLink>
        {card.source_comment_id && (
          <AppLink
            href={`${issueHref}#comment-${card.source_comment_id}`}
            className="inline-flex min-h-9 items-center hover:underline focus-visible:outline-2 focus-visible:outline-ring"
            data-testid="decision-request-source-comment"
          >
            {t(($) => $.decision.source_comment)}
          </AppLink>
        )}
        {card.state === "answered" && card.record_comment_id && (
          <AppLink
            href={`${issueHref}#comment-${card.record_comment_id}`}
            className="inline-flex min-h-9 items-center hover:underline focus-visible:outline-2 focus-visible:outline-ring"
            data-testid="decision-request-record"
          >
            {t(($) => $.decision.recorded_on, { issue: card.issue_identifier || card.issue_id })}
          </AppLink>
        )}
      </div>
      <div className="min-w-0 space-y-3 break-words px-3 py-3">
        {card.state === "withdrawn" && card.reason && (
          <p className="whitespace-pre-wrap text-muted-foreground" data-testid="decision-request-reason">{card.reason}</p>
        )}
        <div data-testid="decision-request-ask">
          <AnnotatableAnswer messageId={`${DECISION_REQUEST_SOURCE_PREFIX}${card.id}`} readOnly={card.state !== "open"}>
            <RichContent content={card.ask} density="compact" phase="settled" />
          </AnnotatableAnswer>
        </div>
        {validDate && (
          <time dateTime={timestamp} className="block text-caption text-muted-foreground">
            {new Intl.DateTimeFormat("en-GB", {
              timeZone: "Asia/Bangkok",
              day: "2-digit",
              month: "short",
              year: "numeric",
              hour: "2-digit",
              minute: "2-digit",
              hourCycle: "h23",
            }).format(date)} ICT
          </time>
        )}
      </div>
    </section>
  );
}

import { useId, useState } from "react";
import { Loader2, MessageSquareQuote } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { useAnswerDecisionRequest } from "@multica/core/issues/decision-answer";
import { useWorkspacePaths } from "@multica/core/paths";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { RichContent } from "../../rich-content";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";
import { AnnotatableAnswer } from "./annotatable-answer";
import { DECISION_REQUEST_SOURCE_PREFIX } from "./chat-annotation-context";

function DecisionAnswerForm({ card }: { card: RequestCard }) {
  const { t } = useT("chat");
  const answer = useAnswerDecisionRequest(card.issue_id);
  const [option, setOption] = useState("");
  const [text, setText] = useState("");
  const group = useId();
  const canSend = !answer.isPending && (option !== "" || text.trim() !== "");
  const send = () => {
    if (canSend) answer.mutate({ requestId: card.id, option: option || undefined, text: text.trim() || undefined });
  };

  return (
    <form
      data-testid="decision-request-answer"
      className="space-y-2"
      onSubmit={(event) => {
        event.preventDefault();
        send();
      }}
    >
      {!!card.options?.length && (
        <fieldset className="space-y-1">
          <legend className="sr-only">{t(($) => $.decision.answer_options)}</legend>
          {card.options.map((item) => (
            <label
              key={item.label}
              className={cn(
                "flex min-h-9 cursor-pointer items-start gap-2 rounded-lg border border-surface-border px-2 py-1.5 has-[:focus-visible]:outline-2 has-[:focus-visible]:outline-ring",
                option === item.label && "border-primary bg-primary/5",
              )}
            >
              <input
                type="radio"
                name={group}
                value={item.label}
                checked={option === item.label}
                onChange={() => setOption(item.label)}
                className="mt-1"
              />
              <span className="min-w-0">
                <span className="font-medium">{item.label}</span>
                {item.what_happens && <span className="block text-caption text-muted-foreground">{item.what_happens}</span>}
              </span>
            </label>
          ))}
        </fieldset>
      )}
      <textarea
        aria-label={t(($) => $.decision.answer_text)}
        placeholder={t(($) => $.decision.answer_text)}
        value={text}
        rows={2}
        onChange={(event) => setText(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
            event.preventDefault();
            send();
          }
        }}
        className="block w-full resize-y rounded-lg border border-surface-border bg-transparent px-2 py-1.5 focus-visible:outline-2 focus-visible:outline-ring"
      />
      <div className="flex items-center gap-2">
        <Button type="submit" size="sm" disabled={!canSend}>
          {answer.isPending && <Loader2 className="size-4 animate-spin" aria-hidden />}
          {t(($) => $.decision.answer_send)}
        </Button>
        {answer.isError && <span role="alert" className="text-caption text-destructive">{t(($) => $.decision.answer_error)}</span>}
      </div>
    </form>
  );
}

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
        {card.state === "open" && <DecisionAnswerForm card={card} />}
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

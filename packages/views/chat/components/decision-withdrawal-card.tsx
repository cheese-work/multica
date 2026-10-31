import { MessageSquareQuote } from "@multica/ui/icons";
import { useWorkspacePaths } from "@multica/core/paths";
import type { DecisionRequestCard } from "@multica/core/types";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";

export function DecisionWithdrawalCard({ card }: { card: DecisionRequestCard }) {
  const { t } = useT("chat");
  const paths = useWorkspacePaths();
  const issueHref = paths.issueDetail(card.issue_id);
  const href = card.source_comment_id ? `${issueHref}#comment-${card.source_comment_id}` : issueHref;
  const label = t(($) => $.decision.withdrawn_in, { issue: card.issue_identifier || card.issue_id });

  return (
    <section
      aria-label={label}
      data-testid="decision-withdrawal-card"
      data-state="withdrawn"
      className="mt-1.5 w-full min-w-0 max-w-[28rem] rounded-xl border border-surface-border bg-surface px-3 py-1 text-label"
    >
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <AppLink
          href={href}
          className="flex min-h-9 min-w-0 max-w-full items-center gap-1.5 break-all hover:underline focus-visible:outline-2 focus-visible:outline-ring"
        >
          <MessageSquareQuote className="size-3.5 shrink-0" aria-hidden />
          <span>{label}</span>
        </AppLink>
        <span className="shrink-0 rounded-full bg-muted px-2 text-micro font-medium text-muted-foreground">
          {t(($) => $.decision.request_state.withdrawn)}
        </span>
      </div>
      {card.reason && <p className="mb-1 truncate text-caption text-muted-foreground" title={card.reason}>{card.reason}</p>}
    </section>
  );
}

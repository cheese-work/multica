import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Loader2, MessageSquareQuote, RefreshCw, Undo2 } from "@multica/ui/icons";
import { Button, buttonVariants } from "@multica/ui/components/ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { cn } from "@multica/ui/lib/utils";
import {
  disclaimConflict,
  useDisclaimDecisionAnswer,
  type DisclaimConflict,
} from "@multica/core/issues/decision-disclaim";
import { chatKeys } from "@multica/core/chat/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import type { DecisionCard } from "@multica/core/types";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";

/**
 * The card under a quoted DM bubble (design `dm-decision-quote-cards` rev-8,
 * direction B). It names the issue the words were quoted into and opens the
 * original issue comment. On a forked DM the card is read-only provenance: it
 * says so, still opens the original comment, and has no Disclaim. Disclaim
 * stays in the original DM and on the issue.
 */
export function DecisionDmCard({ card }: { card: DecisionCard }) {
  const { t } = useT("chat");
  const { t: ti } = useT("issues");
  const paths = useWorkspacePaths();
  const qc = useQueryClient();
  const disclaim = useDisclaimDecisionAnswer(card.issue_id);
  const [confirming, setConfirming] = useState(false);
  const [pending, setPending] = useState(false);
  const [conflict, setConflict] = useState<DisclaimConflict | undefined>();
  const [failed, setFailed] = useState(false);

  const state = conflict === "withdrawn" ? "disclaimed" : card.state;
  const href = `${paths.issueDetail(card.issue_id)}#comment-${card.issue_comment_id}`;
  const canDisclaim = !card.inherited && state !== "disclaimed";
  const delivered = state !== "disclaimed" && !!card.delivered;
  const jumpHref =
    card.request_session_id && card.request_message_id
      ? `${paths.chatSession(card.request_session_id)}&message=${encodeURIComponent(card.request_message_id)}`
      : undefined;

  const refresh = () => {
    qc.invalidateQueries({ queryKey: chatKeys.messagesAll() });
    qc.invalidateQueries({ queryKey: chatKeys.messagesPageAll() });
    setConflict(undefined);
    setFailed(false);
  };

  const confirm = async () => {
    setConfirming(false);
    setPending(true);
    setFailed(false);
    try {
      await disclaim.mutateAsync(card.answer_id);
    } catch (error) {
      const kind = disclaimConflict(error);
      if (kind) setConflict(kind);
      else setFailed(true);
    } finally {
      setPending(false);
    }
  };

  return (
    <div
      data-testid="decision-dm-card"
      data-state={state}
      data-inherited={card.inherited || undefined}
      className={cn(
        "mt-1.5 w-full max-w-[28rem] rounded-xl border border-surface-border bg-surface px-3 py-1 text-label",
        state === "disclaimed" && "bg-muted text-muted-foreground",
      )}
    >
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <AppLink
          href={href}
          data-testid="decision-dm-card-link"
          className="flex min-h-9 items-center gap-1.5 hover:underline"
        >
          <MessageSquareQuote className="size-3.5 shrink-0" aria-hidden />
          <span>
            {t(($) => $.decision.receipt_recorded, { issue: card.issue_identifier })}
            {card.inherited && (
              <span className="text-muted-foreground"> · {t(($) => $.decision.from_original_dm)}</span>
            )}
          </span>
        </AppLink>
        <span
          className={cn(
            "rounded-full px-2 text-micro font-medium",
            state === "disclaimed" ? "bg-muted text-muted-foreground" : delivered ? "bg-success/10 text-success" : "bg-warning/10 text-warning",
          )}
        >
          {state === "disclaimed" ? ti(($) => $.decision.state.disclaimed) : delivered ? ti(($) => $.decision.state.delivered) : ti(($) => $.decision.state.recorded)}
        </span>
        {card.chosen_option_index !== undefined && (
          <span className="rounded-xs border px-1.5 text-micro" title={card.option_label}>
            {ti(($) => $.decision.option_n, { number: card.chosen_option_index + 1 })}
          </span>
        )}
        {jumpHref && state !== "disclaimed" && (
          <AppLink href={jumpHref} data-testid="decision-dm-jump" className="flex min-h-9 items-center text-muted-foreground hover:underline">
            ↑ {t(($) => $.decision.jump_to_card)}
          </AppLink>
        )}
        {card.inherited && (
          <span className="rounded-xs border px-1.5 text-micro text-muted-foreground">
            {t(($) => $.decision.inherited)}
          </span>
        )}
        {canDisclaim && (
          <Button
            size="sm"
            variant="ghost"
            className="ml-auto text-destructive"
            data-testid="decision-dm-disclaim"
            disabled={pending || conflict === "changed"}
            aria-busy={pending || undefined}
            onClick={() => setConfirming(true)}
          >
            {pending ? <Loader2 className="size-3.5 animate-spin" aria-hidden /> : <Undo2 className="size-3.5" aria-hidden />}
            {pending ? ti(($) => $.decision.disclaiming) : ti(($) => $.decision.disclaim_action)}
          </Button>
        )}
      </div>
      {conflict === "changed" && (
        <div
          role="alert"
          data-testid="decision-dm-conflict"
          className="my-1.5 flex flex-wrap items-center gap-2 rounded-md border border-warning/30 bg-warning/10 px-2.5 py-2 text-caption text-foreground"
        >
          <span className="min-w-40 flex-1">
            <b>{ti(($) => $.decision.changed_title)}</b> {ti(($) => $.decision.changed_body)}
          </span>
          <Button size="sm" variant="outline" onClick={refresh}>
            <RefreshCw className="size-3.5" aria-hidden />
            {ti(($) => $.decision.refresh_action)}
          </Button>
        </div>
      )}
      {conflict === "withdrawn" && (
        <div
          role="status"
          data-testid="decision-dm-conflict"
          className="my-1.5 flex flex-wrap items-center gap-2 rounded-md bg-muted px-2.5 py-2 text-caption text-muted-foreground"
        >
          <span className="min-w-40 flex-1">
            <b>{ti(($) => $.decision.withdrawn_title)}</b> {ti(($) => $.decision.withdrawn_body)}
          </span>
          <AppLink href={href} className={buttonVariants({ size: "sm", variant: "outline" })}>
            {ti(($) => $.decision.review_action)}
          </AppLink>
        </div>
      )}
      {failed && (
        <div
          role="alert"
          data-testid="decision-dm-error"
          className="my-1.5 flex flex-wrap items-center gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-2.5 py-2 text-caption text-destructive"
        >
          <span className="min-w-40 flex-1">{ti(($) => $.decision.failed_notice)}</span>
          <Button size="sm" variant="outline" onClick={() => setConfirming(true)}>
            {ti(($) => $.decision.try_again_action)}
          </Button>
        </div>
      )}
      <AlertDialog open={confirming} onOpenChange={setConfirming}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{ti(($) => $.decision.disclaim_title)}</AlertDialogTitle>
            <AlertDialogDescription>{ti(($) => $.decision.disclaim_desc)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{ti(($) => $.decision.keep_action)}</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={() => void confirm()}>
              {ti(($) => $.decision.disclaim_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

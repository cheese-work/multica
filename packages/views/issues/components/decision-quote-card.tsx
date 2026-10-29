import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Loader2, MessageSquareQuote, RefreshCw, Undo2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
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
  splitDecisionQuote,
  useDisclaimDecisionAnswer,
  type DisclaimConflict,
} from "@multica/core/issues/decision-disclaim";
import { issueKeys } from "@multica/core/issues/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import { useActorName } from "@multica/core/workspace/hooks";
import type { DecisionState, TimelineEntry } from "@multica/core/types";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";

const STATE_TONE: Record<DecisionState, string> = {
  pending: "bg-warning/10 text-warning",
  effective: "bg-success/10 text-success",
  disclaimed: "bg-muted text-muted-foreground",
};

/**
 * Receipt card for a decision quote comment (design `dm-decision-quote-cards`
 * rev-8, direction B). The server stores the quote under the recording agent,
 * so the caller shows the human as author. The DM pointers arrive only for the
 * request's human, so Open DM and Disclaim are theirs alone.
 */
export function DecisionQuoteCard({
  entry,
  issueId,
  currentUserId,
  onJumpToComment,
}: {
  entry: TimelineEntry;
  issueId: string;
  currentUserId?: string;
  onJumpToComment?: (commentId: string) => void;
}) {
  const { t } = useT("issues");
  const paths = useWorkspacePaths();
  const qc = useQueryClient();
  const { getActorName } = useActorName();
  const disclaim = useDisclaimDecisionAnswer(issueId);
  const [confirming, setConfirming] = useState(false);
  const [pending, setPending] = useState(false);
  const [conflict, setConflict] = useState<DisclaimConflict | undefined>();
  const [failed, setFailed] = useState(false);

  const answerId = entry.decision_answer_id ?? "";
  const isHuman = !!currentUserId && entry.decision_human_user_id === currentUserId;
  const state: DecisionState = conflict === "withdrawn" ? "disclaimed" : (entry.decision_state ?? "pending");
  const { note, quote } = splitDecisionQuote(entry.content ?? "");
  const recorder = getActorName("agent", entry.decision_recorder_agent_id ?? entry.actor_id);
  const dmLink = (session?: string, message?: string) =>
    session && message ? `${paths.chatSession(session)}&message=${encodeURIComponent(message)}` : undefined;
  const dmHref = dmLink(entry.decision_source_session_id, entry.decision_source_message_id);
  const askedHref = dmLink(entry.decision_request_session_id, entry.decision_request_message_id);
  const klass = entry.decision_class ?? "";

  const refresh = () => {
    qc.invalidateQueries({ queryKey: issueKeys.timeline(issueId) });
    setConflict(undefined);
    setFailed(false);
  };

  const review = () => {
    refresh();
    const timeline = qc.getQueryData<TimelineEntry[]>(issueKeys.timeline(issueId));
    const withdrawal = timeline?.find((e) => e.decision_answer_id === answerId && e.decision_comment_kind === "disclaim");
    if (withdrawal) onJumpToComment?.(withdrawal.id);
  };

  const confirm = async () => {
    setConfirming(false);
    setPending(true);
    setFailed(false);
    try {
      await disclaim.mutateAsync(answerId);
    } catch (error) {
      const kind = disclaimConflict(error);
      if (kind) setConflict(kind);
      else setFailed(true);
    } finally {
      setPending(false);
    }
  };

  const canDisclaim = isHuman && state !== "disclaimed";

  return (
    <div
      data-testid="decision-quote-card"
      data-state={state}
      className={cn(
        "mt-1 overflow-hidden rounded-md border border-surface-border",
        state === "disclaimed" && "bg-muted text-muted-foreground",
      )}
    >
      <div className="flex flex-wrap items-center gap-2 border-b bg-muted px-2.5 py-1.5 text-caption">
        <MessageSquareQuote className="size-3.5 shrink-0" aria-hidden />
        <span className="rounded-xs border px-1.5 text-micro text-muted-foreground">
          {t(($) => $.decision.class[klass as keyof typeof $.decision.class] ?? klass, { defaultValue: klass })}
        </span>
        <span className={cn("rounded-full px-2 text-micro font-medium", STATE_TONE[state])}>
          {t(($) => $.decision.state[state])}
        </span>
        <span className="text-micro text-muted-foreground">{t(($) => $.decision.via_dm)}</span>
      </div>
      <p className="whitespace-pre-wrap break-words px-2.5 py-2 text-body">{quote}</p>
      {note && <p className="px-2.5 pb-2 text-caption text-muted-foreground">{note}</p>}
      <div className="flex flex-wrap items-center gap-1 px-1.5 pb-1.5 pl-2.5">
        <span className="mr-auto text-caption text-muted-foreground">
          {t(($) => $.decision.recorded_by, { name: recorder })}
        </span>
        {isHuman && askedHref && state !== "disclaimed" && (
          <Button
            size="sm"
            variant="ghost"
            className="text-muted-foreground"
            nativeButton={false}
            render={<AppLink href={askedHref} data-testid="decision-quote-asked-dm" />}
          >
            {t(($) => $.decision.asked_in_dm)}
          </Button>
        )}
        {isHuman && dmHref && state !== "disclaimed" ? (
          <Button
            size="sm"
            variant="ghost"
            className="text-muted-foreground"
            nativeButton={false}
            render={<AppLink href={dmHref} data-testid="decision-quote-open-dm" />}
          >
            {t(($) => $.decision.answered_in_dm)}
          </Button>
        ) : !isHuman ? (
          <span className="text-caption text-muted-foreground">{t(($) => $.decision.private_dm)}</span>
        ) : null}
        {canDisclaim && (
          <Button
            size="sm"
            variant="ghost"
            className="text-destructive"
            data-testid="decision-disclaim-button"
            disabled={pending || conflict === "changed"}
            aria-busy={pending || undefined}
            onClick={() => setConfirming(true)}
          >
            {pending ? <Loader2 className="size-3.5 animate-spin" aria-hidden /> : <Undo2 className="size-3.5" aria-hidden />}
            {pending ? t(($) => $.decision.disclaiming) : t(($) => $.decision.disclaim_action)}
          </Button>
        )}
      </div>
      {conflict === "changed" && (
        <div
          role="alert"
          data-testid="decision-conflict-notice"
          className="m-1.5 flex flex-wrap items-center gap-2 rounded-md border border-warning/30 bg-warning/10 px-2.5 py-2 text-caption text-foreground"
        >
          <span className="min-w-48 flex-1">
            <b>{t(($) => $.decision.changed_title)}</b> {t(($) => $.decision.changed_body)}
          </span>
          <Button size="sm" variant="outline" onClick={refresh}>
            <RefreshCw className="size-3.5" aria-hidden />
            {t(($) => $.decision.refresh_action)}
          </Button>
        </div>
      )}
      {conflict === "withdrawn" && (
        <div
          role="status"
          data-testid="decision-conflict-notice"
          className="m-1.5 flex flex-wrap items-center gap-2 rounded-md bg-muted px-2.5 py-2 text-caption text-muted-foreground"
        >
          <span className="min-w-48 flex-1">
            <b>{t(($) => $.decision.withdrawn_title)}</b> {t(($) => $.decision.withdrawn_body)}
          </span>
          <Button size="sm" variant="outline" onClick={review}>
            {t(($) => $.decision.review_action)}
          </Button>
        </div>
      )}
      {failed && (
        <div
          role="alert"
          data-testid="decision-error-notice"
          className="m-1.5 flex flex-wrap items-center gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-2.5 py-2 text-caption text-destructive"
        >
          <span className="min-w-48 flex-1">{t(($) => $.decision.failed_notice)}</span>
          <Button size="sm" variant="outline" onClick={() => setConfirming(true)}>
            {t(($) => $.decision.try_again_action)}
          </Button>
        </div>
      )}
      <AlertDialog open={confirming} onOpenChange={setConfirming}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.decision.disclaim_title)}</AlertDialogTitle>
            <AlertDialogDescription>{t(($) => $.decision.disclaim_desc)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.decision.keep_action)}</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={() => void confirm()}>
              {t(($) => $.decision.disclaim_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

/**
 * The server stores a decision quote under the recording agent so wake-ups and
 * audit keep their meaning. The timeline shows the human whose words it holds.
 */
export function decisionDisplayActor(entry: TimelineEntry): { actorType: string; actorId: string; name?: string; avatarUrl?: string } {
  if (entry.decision_comment_kind === "quote" && entry.decision_human_user_id) {
    return { actorType: "member", actorId: entry.decision_human_user_id };
  }
  return { actorType: entry.actor_type, actorId: entry.actor_id, name: entry.actor_name, avatarUrl: entry.actor_avatar_url };
}

/** The agent's "Decision withdrawn: human." note, as a compact line instead of a comment body. */
export function DecisionWithdrawalLine({ entry }: { entry: TimelineEntry }) {
  return (
    <p data-testid="decision-withdrawal-line" className="text-caption text-muted-foreground">
      {entry.content}
    </p>
  );
}

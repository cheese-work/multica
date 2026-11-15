import { type KeyboardEvent, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Check, KeyRound, Loader2 } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { api } from "@multica/core/api";
import type { PasskeyDecisionOutcome } from "@multica/core/api/passkey-schema";
import { signPasskeyChallenge } from "@multica/core/auth/webauthn";
import { chatKeys } from "@multica/core/chat/queries";
import { useCurrentWorkspace, useWorkspacePaths } from "@multica/core/paths";
import { formatShortcut, isPlainShortcut, shortcutMatchesEvent, useShortcut } from "@multica/core/shortcuts";
import type { DecisionBatchItem } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";

type Pick = "approve" | "reject";
const PICKS: readonly Pick[] = ["approve", "reject"];

// What the batch approve signs: the hash of the approved items' payload hashes, sorted (the server's batchPayloadSHA256).
async function batchPayloadSha256(hashes: string[]) {
  const bytes = new TextEncoder().encode([...hashes].sort().join("\n"));
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

// Direction B v9 batch card: each item has its own Approve / Reject, none chosen on open; one button covers the batch
// and one passkey signs the approved items. Choices live in state only, so a reload never brings back an armed Approve.
export function DecisionBatchForm({ batchId, items }: { batchId: string; items: DecisionBatchItem[] }) {
  const { t } = useT("chat");
  const paths = useWorkspacePaths();
  const workspaceId = useCurrentWorkspace()?.id;
  const queryClient = useQueryClient();
  const sendChord = useShortcut("send");
  const sendKey = formatShortcut(sendChord);
  const [choices, setChoices] = useState<Record<string, Pick>>({});
  const [outcomes, setOutcomes] = useState<Record<string, PasskeyDecisionOutcome>>({});
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  // Closed on the first press: a second press before the next render cannot send a second approval.
  const sending = useRef(false);
  const radios = useRef<Record<string, Partial<Record<Pick, HTMLButtonElement | null>>>>({});

  // The batch is decided as a whole, so a choice is offered only while every item is pending and nothing was sent from here.
  const decided = Object.keys(outcomes).length > 0;
  const choosable = !decided && items.every((item) => item.state === "pending");
  const approved = items.filter((item) => choices[item.action_id] === "approve");
  const left = items.length - Object.keys(choices).length;
  const rejected = items.length - left - approved.length;

  const choose = (id: string, pick: Pick | undefined) => setChoices((current) => {
    const next = { ...current };
    if (pick) next[id] = pick;
    else delete next[id];
    return next;
  });
  const move = (event: KeyboardEvent, id: string, from: Pick) => {
    const step = event.key === "ArrowRight" || event.key === "ArrowDown" ? 1 : event.key === "ArrowLeft" || event.key === "ArrowUp" ? -1 : 0;
    if (!step) return;
    event.preventDefault();
    const next = PICKS[(PICKS.indexOf(from) + step + PICKS.length) % PICKS.length]!;
    choose(id, next);
    radios.current[id]?.[next]?.focus();
  };

  const send = async () => {
    if (left > 0 || sending.current) return;
    sending.current = true;
    setBusy(true);
    setFailed(false);
    try {
      let signed: { payload_sha256: string; assertion?: Awaited<ReturnType<typeof signPasskeyChallenge>> } = { payload_sha256: "" };
      if (approved.length > 0) {
        const views = await Promise.all(approved.map((item) => api.getDecisionAction(item.action_id)));
        const assertion = await signPasskeyChallenge(await api.challengeDecisionBatch(batchId, choices));
        signed = { payload_sha256: await batchPayloadSha256(views.map((view) => view.payload_sha256)), assertion };
      }
      const result = await api.approveDecisionBatch(batchId, { ...signed, choices });
      setOutcomes(Object.fromEntries(result.items.map((outcome) => [outcome.id, outcome])));
    } catch {
      // A refusal applied nothing; a lost response is settled by reading the card again.
      setFailed(true);
      sending.current = false;
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: chatKeys.messagesAll() });
      queryClient.invalidateQueries({ queryKey: chatKeys.messagesPageAll() });
      if (workspaceId) queryClient.invalidateQueries({ queryKey: chatKeys.issueDecisionCardsAll(workspaceId) });
    }
  };

  const effect = (item: DecisionBatchItem) => {
    const outcome = outcomes[item.action_id];
    const state = outcome?.state ?? item.state;
    if (state === "executed") return t(($) => $.decision.effect_applied);
    if (state === "rejected") return t(($) => $.decision.effect_rejected);
    if (state === "expired") return t(($) => $.decision.request_state.expired);
    if (state !== "failed") return null;
    const reason = outcome?.error_message || outcome?.error_code || item.error_code;
    return reason ? t(($) => $.decision.effect_not_applied_reason, { reason }) : t(($) => $.decision.effect_not_applied);
  };

  const label = left > 0 ? t(($) => $.decision.batch_choose, { count: left })
    : approved.length === 0 ? t(($) => $.decision.batch_send_reject, { reject: rejected, shortcut: sendKey })
    : rejected === 0 ? t(($) => $.decision.batch_send_approve, { approve: approved.length, shortcut: sendKey })
    : t(($) => $.decision.batch_send_mixed, { approve: approved.length, reject: rejected, shortcut: sendKey });

  return (
    <form
      data-testid="decision-batch-form"
      className="min-w-0 space-y-2"
      onSubmit={(event) => { event.preventDefault(); void send(); }}
      onKeyDown={(event) => {
        // A plain-Enter send shortcut would approve while the person only moves between choices.
        if (isPlainShortcut(sendChord, "Enter") || !shortcutMatchesEvent(sendChord, event.nativeEvent)) return;
        event.preventDefault();
        void send();
      }}
    >
      <ul className="min-w-0 space-y-2">
        {items.map((item) => {
          const picked = choices[item.action_id];
          const shown = effect(item);
          const issue = item.issue_identifier || item.issue_id;
          return (
            <li key={item.action_id} className="flex min-w-0 flex-wrap items-center justify-between gap-x-3 gap-y-1">
              <span className="min-w-0 break-words text-label">
                <AppLink href={paths.issueDetail(item.issue_id)} className="font-medium hover:underline focus-visible:outline-2 focus-visible:outline-ring">{issue}</AppLink>
                {item.issue_title && <span className="text-muted-foreground">{` ${item.issue_title}`}</span>}
              </span>
              {choosable ? (
                <div role="radiogroup" aria-label={t(($) => $.decision.choice_item, { issue })} className="inline-flex shrink-0 overflow-hidden rounded-lg border border-surface-border">
                  {PICKS.map((value) => {
                    const selected = picked === value;
                    return (
                      // ui-pattern-ref: allow radio segment (role=radio, roving tabindex); Button has no radio variant, see choice-card
                      <button
                        key={value}
                        ref={(node) => { (radios.current[item.action_id] ??= {})[value] = node; }}
                        type="button"
                        role="radio"
                        aria-checked={selected}
                        tabIndex={selected || (!picked && value === "approve") ? 0 : -1}
                        disabled={busy}
                        onClick={() => choose(item.action_id, selected ? undefined : value)}
                        onKeyDown={(event) => move(event, item.action_id, value)}
                        className={cn(
                          "flex min-h-9 items-center gap-1.5 px-3 text-label font-medium hover:bg-accent focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring disabled:opacity-70 max-sm:min-h-11",
                          value === "reject" && "border-l border-surface-border",
                          selected && "bg-primary/10 text-primary hover:bg-primary/10",
                        )}
                      >
                        {selected && <Check className="size-3.5" aria-hidden />}
                        {t(($) => (value === "approve" ? $.decision.choice_approve : $.decision.choice_reject))}
                      </button>
                    );
                  })}
                </div>
              ) : shown && (
                <span data-testid={`decision-batch-effect-${item.action_id}`} className="text-caption text-muted-foreground">{shown}</span>
              )}
            </li>
          );
        })}
      </ul>
      {choosable && (
        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" size="sm" disabled={left > 0 || busy} className="disabled:opacity-70 max-sm:h-11">
            {busy && <Loader2 className="size-4 animate-spin" aria-hidden />}
            {left === 0 && approved.length > 0 && <KeyRound className="size-4" aria-hidden />}
            {label}
          </Button>
          {failed && <span role="alert" className="text-caption text-destructive">{t(($) => $.decision.answer_error)}</span>}
        </div>
      )}
    </form>
  );
}

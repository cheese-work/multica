import { useState } from "react";
import { Button } from "@multica/ui/components/ui/button";
import { Textarea } from "@multica/ui/components/ui/textarea";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { useT } from "../../i18n";

const ictFormat = new Intl.DateTimeFormat("en-GB", {
  timeZone: "Asia/Bangkok",
  day: "2-digit",
  month: "short",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});

/** "11 Nov 2026, 13:20 ICT", or "" for a missing or unreadable time. */
export function ictOf(iso?: string | null) {
  const date = new Date(iso ?? "");
  return Number.isNaN(date.getTime()) ? "" : `${ictFormat.format(date)} ICT`;
}

type Action = NonNullable<RequestCard["action"]>;

// What the decision did. An older server sends no `effect`; the action state says the same thing.
export function effectOf(action?: Action) {
  if (!action) return null;
  if (action.revoke) return { kind: "revoked" as const, reason: "" };
  if (action.effect) return { kind: action.effect, reason: action.effect_reason ?? "" };
  if (action.state === "executed") return { kind: "applied", reason: "" };
  if (action.state === "failed") return { kind: "not_applied", reason: "" };
  if (action.state === "rejected") return { kind: "not_applied", reason: "rejected" };
  if (action.state === "expired") return { kind: "expired", reason: "" };
  return null;
}

/** The effect line: Applied, Not applied (with its reason), Expired or Revoked; "" while undecided. */
export function effectText(decision: RequestCard, t: ReturnType<typeof useT<"chat">>["t"]) {
  const effect = effectOf(decision.action);
  if (!effect) return "";
  return effect.kind === "applied" ? t(($) => $.decision.effect_applied)
    : effect.kind === "expired" ? t(($) => $.decision.effect_expired)
    : effect.kind === "revoked" ? t(($) => $.decision.effect_revoked)
    : effect.reason === "rejected" ? t(($) => $.decision.effect_rejected)
    : effect.reason ? t(($) => $.decision.effect_not_applied_reason, { reason: effect.reason })
    : t(($) => $.decision.effect_not_applied);
}

// B v9 S9: the one collapsed block of a decided card, shared by the DM and the issue page.
// `onRevoke` is the DM's; the issue page passes none and shows the block read-only.
export function DecisionSummary({ decision, onRevoke }: { decision: RequestCard; onRevoke?: (reason: string) => Promise<void> }) {
  const { t } = useT("chat");
  const [asking, setAsking] = useState(false);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const action = decision.action;
  const effect = effectOf(action);
  if (!action || !effect) return null;

  const text = effectText(decision, t);
  const by = action.decided_by_name && action.on_behalf_of_name && action.decided_by_name !== action.on_behalf_of_name
    ? t(($) => $.decision.by_for, { by: action.decided_by_name, for: action.on_behalf_of_name })
    : action.decided_by_name;
  const asked = ictOf(decision.created_at);
  const decided = ictOf(action.decided_at);
  const revoked = action.revoke;
  // The server's flag alone is not enough: Revoke belongs to an applied, not yet revoked decision.
  const canRevoke = !!onRevoke && action.revocable === true && effect.kind === "applied";

  const confirm = async () => {
    setBusy(true);
    setFailed(false);
    try {
      await onRevoke!(reason.trim());
      setAsking(false);
    } catch {
      setFailed(true);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div data-testid="decision-summary" className="space-y-1 border-t border-surface-border px-3 py-2 text-label">
      <p data-testid="decision-request-effect" className="font-medium">{text}</p>
      <dl data-testid="decision-request-meta" className="grid grid-cols-[auto_1fr] gap-x-3 text-caption text-muted-foreground">
        {asked && (<><dt>{t(($) => $.decision.meta_asked)}</dt><dd><time dateTime={decision.created_at}>{asked}</time></dd></>)}
        {decided && (<><dt>{t(($) => $.decision.meta_decided)}</dt><dd><time dateTime={action.decided_at}>{decided}</time></dd></>)}
        {revoked && ictOf(revoked.at) && (<><dt>{t(($) => $.decision.meta_revoked)}</dt><dd><time dateTime={revoked.at}>{ictOf(revoked.at)}</time></dd></>)}
        {by && (<><dt>{t(($) => $.decision.meta_by)}</dt><dd>{by}</dd></>)}
        {decision.asking_agent_name && (<><dt>{t(($) => $.decision.meta_from)}</dt><dd>{decision.asking_agent_name}</dd></>)}
        {revoked?.reason && (<><dt>{t(($) => $.decision.revoke_reason)}</dt><dd className="break-words">{revoked.reason}</dd></>)}
      </dl>
      {canRevoke && !asking && (
        <Button type="button" size="sm" variant="outline" onClick={() => setAsking(true)}>{t(($) => $.decision.revoke)}</Button>
      )}
      {canRevoke && asking && (
        <div className="space-y-2 pt-1">
          <Textarea
            aria-label={t(($) => $.decision.revoke_reason)}
            value={reason}
            maxLength={1024}
            disabled={busy}
            onChange={(event) => setReason(event.target.value)}
            rows={2}
          />
          {failed && <p role="alert" className="text-caption text-destructive">{t(($) => $.decision.revoke_failed)}</p>}
          <div className="flex gap-2">
            <Button type="button" size="sm" disabled={busy || reason.trim() === ""} onClick={confirm}>
              {busy ? t(($) => $.decision.revoke_busy) : t(($) => $.decision.revoke_confirm)}
            </Button>
            <Button type="button" size="sm" variant="ghost" disabled={busy} onClick={() => setAsking(false)}>{t(($) => $.decision.revoke_cancel)}</Button>
          </div>
        </div>
      )}
    </div>
  );
}

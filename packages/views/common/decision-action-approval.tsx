"use client";

import { useEffect, useState } from "react";
import { api } from "@multica/core/api";
import { ApiError } from "@multica/core/api/client";
import type { PasskeyDecisionOutcome } from "@multica/core/api/passkey-schema";
import { signPasskeyChallenge } from "@multica/core/auth/webauthn";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../i18n";

type View =
  | { step: "loading" | "idle" | "busy" | "rejecting" | "no_passkey" | "retry" | "reject_retry" | "lost" | "preview_required" }
  | { step: "done"; outcome: PasskeyDecisionOutcome };

const TERMINAL = ["executed", "failed", "rejected", "expired", "cancelled"] as const;
type Terminal = (typeof TERMINAL)[number];
const isTerminal = (state: string): state is Terminal => (TERMINAL as readonly string[]).includes(state);

// An approve request that left this tab. Survives a remount, so an undecided
// action never offers approve again (N15); a new tab or session starts clean.
const sentKey = (actionId: string) => `decision-action-sent:${actionId}`;
function approveWasSent(actionId: string): boolean {
  try {
    return sessionStorage.getItem(sentKey(actionId)) !== null;
  } catch {
    return false; // storage blocked: the server still refuses a decided action
  }
}
function markApproveSent(actionId: string, sent = true) {
  try {
    if (sent) sessionStorage.setItem(sentKey(actionId), "1");
    else sessionStorage.removeItem(sentKey(actionId));
  } catch {
    // see approveWasSent
  }
}

// What the executor reported: `result.outcome` is `cap_applied`, `run_queued` or
// `still_blocked:<reason>`; a token this client does not know shows as plain "Executed".
function executedEffect(result: unknown): { token: string; change?: string } | null {
  if (typeof result !== "object" || result === null) return null;
  const { outcome, from, to } = result as Record<string, unknown>;
  if (typeof outcome !== "string") return null;
  return { token: outcome, change: typeof from === "number" && typeof to === "number" ? `${from} → ${to}` : undefined };
}

/**
 * Passkey approval of one executable decision action: challenge → WebAuthn
 * assertion → approve → outcome. After the approve request leaves, a lost or
 * uncertain response only reads the outcome back; the action is never approved
 * a second time (CHE-2798 N15), also after a remount in the same tab. A mount also reads the outcome first, so a
 * remount or a second session shows a decided action instead of offering
 * approve again. `onEnroll` opens passkey enrollment.
 */
export function DecisionActionApproval({
  actionId,
  payloadSha256,
  previewSha256,
  state,
  onEnroll,
}: {
  actionId: string;
  payloadSha256: string;
  /** The digest of the preview the card rendered; the server approves only against that preview. */
  previewSha256?: string;
  /** The action state the surrounding card reports; a change reads the outcome again (a decision made in another tab or by a worker). */
  state?: string;
  onEnroll?: () => void;
}) {
  const { t } = useT("common");
  const [view, setView] = useState<View>({ step: "loading" });

  // `ifPending` is the view for an action that is still undecided.
  const readBack = async (ifPending: View = { step: "lost" }) => {
    try {
      const outcome = await api.getDecisionActionOutcome(actionId);
      setView(isTerminal(outcome.state) ? { step: "done", outcome } : ifPending);
    } catch {
      setView({ step: "lost" });
    }
  };

  useEffect(() => {
    void readBack(approveWasSent(actionId) ? { step: "lost" } : { step: "idle" });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- read once per action and per reported state
  }, [actionId, state]);

  const approve = async () => {
    setView({ step: "busy" });
    let sent = false;
    try {
      const assertion = await signPasskeyChallenge(await api.challengeDecisionAction(actionId));
      sent = true;
      markApproveSent(actionId);
      const outcome = await api.approveDecisionAction(actionId, { payload_sha256: payloadSha256, ...(previewSha256 && { preview_sha256: previewSha256 }), assertion });
      setView(isTerminal(outcome.state) ? { step: "done", outcome } : { step: "lost" });
    } catch (error) {
      const refused = error instanceof ApiError && error.status < 500;
      if (sent && refused) markApproveSent(actionId, false); // the server answered: nothing was approved
      const reason = refused ? error.message.split(":")[0] : "";
      if (!sent && reason === "no_passkey") setView({ step: "no_passkey" });
      else if (!sent && reason === "not_pending") await readBack();
      else if (sent && reason === "preview_required") setView({ step: "preview_required" });
      else if (!sent && reason === "expired") await readBack({ step: "done", outcome: { id: actionId, state: "expired" } });
      else if (!sent || refused) setView({ step: "retry" });
      else await readBack();
    }
  };

  const reject = async () => {
    setView({ step: "rejecting" });
    try {
      const outcome = await api.rejectDecisionAction(actionId);
      setView(isTerminal(outcome.state) ? { step: "done", outcome } : { step: "lost" });
    } catch (error) {
      if (error instanceof ApiError && error.status < 500 && !error.message.startsWith("not_pending")) setView({ step: "reject_retry" });
      else await readBack(); // the click may have landed: show the real state
    }
  };

  if (view.step === "done") {
    const { state, error_code: code } = view.outcome;
    const detail =
      code === "stale_cap" ? t(($) => $.decision_action.failed_code.stale_cap)
      : code === "stale_window" ? t(($) => $.decision_action.failed_code.stale_window)
      : code === "not_budgeted" ? t(($) => $.decision_action.failed_code.not_budgeted)
      : code === "stale_base" ? t(($) => $.decision_action.failed_code.stale_base)
      : code ? t(($) => $.decision_action.failed_code.other, { code })
      : null;
    const effect = state === "executed" ? executedEffect(view.outcome.result) : null;
    return (
      <div role="status" data-state={state} className="space-y-1 text-label">
        <p className="font-medium">
          {effect?.token === "cap_applied" ? t(($) => $.decision_action.outcome.cap_applied)
            : effect?.token === "run_queued" ? t(($) => $.decision_action.outcome.run_queued)
            : effect?.token.startsWith("still_blocked:") ? t(($) => $.decision_action.outcome.still_blocked, { reason: effect.token.slice("still_blocked:".length) })
            : t(($) => $.decision_action.state[state as Terminal])}
        </p>
        {effect?.change && <p className="text-caption text-muted-foreground">{effect.change}</p>}
        {state === "failed" && detail && <p className="text-caption text-muted-foreground">{detail}</p>}
      </div>
    );
  }
  if (view.step === "lost") {
    return (
      <div className="space-y-1 text-label">
        <p role="alert" className="text-caption text-destructive">{t(($) => $.decision_action.lost)}</p>
        <Button size="sm" variant="outline" onClick={() => readBack()}>{t(($) => $.decision_action.check)}</Button>
      </div>
    );
  }
  if (view.step === "preview_required") {
    // The shown changes are stale: approving again would send the same digest, so only Reject is left.
    return (
      <div className="space-y-1 text-label">
        <p role="alert" className="text-caption text-destructive">{t(($) => $.decision_action.preview_required)}</p>
        <Button size="sm" variant="outline" onClick={reject}>{t(($) => $.decision_action.reject)}</Button>
      </div>
    );
  }
  if (view.step === "no_passkey") {
    return (
      <div className="space-y-1 text-label">
        <p role="alert" className="text-caption">{t(($) => $.decision_action.no_passkey)}</p>
        <div className="flex flex-wrap gap-2">
          {onEnroll && <Button size="sm" onClick={onEnroll}>{t(($) => $.decision_action.add_passkey)}</Button>}
          <Button size="sm" variant="outline" onClick={reject}>{t(($) => $.decision_action.reject)}</Button>
        </div>
      </div>
    );
  }
  if (view.step === "loading") return null;
  return (
    <div className="space-y-1 text-label">
      {view.step === "retry" && <p role="alert" className="text-caption text-destructive">{t(($) => $.decision_action.error)}</p>}
      {view.step === "reject_retry" && <p role="alert" className="text-caption text-destructive">{t(($) => $.decision_action.reject_error)}</p>}
      <div className="flex flex-wrap gap-2">
        <Button size="sm" onClick={approve} disabled={view.step === "busy" || view.step === "rejecting"}>
          {view.step === "busy" ? t(($) => $.decision_action.waiting) : t(($) => $.decision_action.approve)}
        </Button>
        <Button size="sm" variant="outline" onClick={reject} disabled={view.step === "busy" || view.step === "rejecting"}>{t(($) => $.decision_action.reject)}</Button>
      </div>
    </div>
  );
}

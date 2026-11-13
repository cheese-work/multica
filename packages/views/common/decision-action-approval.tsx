"use client";

import { useEffect, useRef, useState } from "react";
import { api } from "@multica/core/api";
import { ApiError } from "@multica/core/api/client";
import type { PasskeyDecisionOutcome } from "@multica/core/api/passkey-schema";
import { signPasskeyChallenge } from "@multica/core/auth/webauthn";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../i18n";

type View =
  | { step: "loading" | "idle" | "busy" | "rejecting" | "no_passkey" | "retry" | "reject_retry" | "lost" }
  | { step: "refused"; code: string }
  | { step: "done"; outcome: PasskeyDecisionOutcome };

const TERMINAL = ["executed", "failed", "rejected", "expired", "cancelled"] as const;
type Terminal = (typeof TERMINAL)[number];
const isTerminal = (state: string): state is Terminal => (TERMINAL as readonly string[]).includes(state);

// Refusal codes the server sends as `code: message` (CHE-3348). Each has its own text; a code this
// client does not know is shown as it came, never swallowed into a generic retry.
const REFUSAL_CODES = [
  "class_needs_owner", "payload_mismatch", "preview_required", "precondition_drift",
  "busy_retry", "not_permitted", "action_not_executable", "approval_proof_unavailable",
] as const;
type RefusalCode = (typeof REFUSAL_CODES)[number];
const isRefusalCode = (code: string): code is RefusalCode => (REFUSAL_CODES as readonly string[]).includes(code);
// Asking again cannot change these answers.
const FINAL_REFUSALS: readonly string[] = ["class_needs_owner", "not_permitted", "action_not_executable", "approval_proof_unavailable"];

// A definite server answer: any 4xx, and 501 (action_not_executable). Other 5xx leave the outcome unknown.
const isDefiniteAnswer = (error: unknown): error is ApiError => error instanceof ApiError && (error.status < 500 || error.status === 501);

/** The `code` of a `code: message` refusal, or "" when the error carries none. */
function refusalCode(error: ApiError): string {
  const [code, rest] = error.message.split(":", 2);
  return rest !== undefined && /^[a-z][a-z0-9_]*$/.test(code!) ? code! : "";
}

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
  canApprove = true,
  approveDisabled = false,
  state,
  onEnroll,
  onOutcome,
}: {
  actionId: string;
  payloadSha256: string;
  /** The digest of the preview the person was shown. The server refuses the approval when it differs from the stored one. */
  previewSha256?: string;
  /** False hides Approve (the person may not approve this action). Reject stays. */
  canApprove?: boolean;
  /** Shows Approve but not clickable: the person may approve, the details they would approve are unavailable. */
  approveDisabled?: boolean;
  /** The action state the surrounding card reports; a change reads the outcome again (a decision made in another tab or by a worker). */
  state?: string;
  onEnroll?: () => void;
  /** Called once when the action reaches a terminal state, so the caller can refresh what the action changed. */
  onOutcome?: (state: string) => void;
}) {
  const { t } = useT("common");
  const [view, setView] = useState<View>({ step: "loading" });
  // The preview digest the server refused (`preview_required`). Asking again with it gets the same answer,
  // so Approve stays off until the card shows a different preview.
  const [refusedPreview, setRefusedPreview] = useState<string | undefined>();
  const onOutcomeRef = useRef(onOutcome);
  onOutcomeRef.current = onOutcome;
  const doneState = view.step === "done" ? view.outcome.state : null;
  useEffect(() => {
    if (doneState) onOutcomeRef.current?.(doneState);
  }, [doneState]);

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
      const outcome = await api.approveDecisionAction(actionId, {
        payload_sha256: payloadSha256,
        assertion,
        ...(previewSha256 ? { preview_sha256: previewSha256 } : {}),
      });
      setView(isTerminal(outcome.state) ? { step: "done", outcome } : { step: "lost" });
    } catch (error) {
      const refused = isDefiniteAnswer(error);
      if (sent && refused) markApproveSent(actionId, false); // the server answered: nothing was approved
      const reason = refused ? refusalCode(error) : "";
      if (!sent && reason === "no_passkey") setView({ step: "no_passkey" });
      else if (!sent && reason === "not_pending") await readBack();
      else if (!sent && reason === "expired") await readBack({ step: "done", outcome: { id: actionId, state: "expired" } });
      else if (reason) {
        if (reason === "preview_required") setRefusedPreview(previewSha256 ?? "");
        setView({ step: "refused", code: reason });
      }
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
  const previewRefused = refusedPreview !== undefined && refusedPreview === (previewSha256 ?? "");
  // A `preview_required` refusal belongs to the preview it answered; a different preview starts clean.
  const refusal = view.step === "refused" && (view.code !== "preview_required" || previewRefused) ? view.code : undefined;
  const mayApprove = canApprove && !(refusal && FINAL_REFUSALS.includes(refusal));
  return (
    <div className="space-y-1 text-label">
      {refusal && (
        <p role="alert" data-testid="decision-action-refusal" className="text-caption text-destructive">
          {isRefusalCode(refusal) ? (
            <>
              {t(($) => $.decision_action.refusal[refusal])} <code className="font-mono">{refusal}</code>
            </>
          ) : t(($) => $.decision_action.refusal.other, { code: refusal })}
        </p>
      )}
      {!canApprove && <p data-testid="decision-action-owner-note" className="text-caption text-muted-foreground">{t(($) => $.decision_action.owner_only)}</p>}
      {view.step === "retry" && <p role="alert" className="text-caption text-destructive">{t(($) => $.decision_action.error)}</p>}
      {view.step === "reject_retry" && <p role="alert" className="text-caption text-destructive">{t(($) => $.decision_action.reject_error)}</p>}
      <div className="flex flex-wrap gap-2">
        {mayApprove && (
          <Button size="sm" data-testid="decision-action-approve" onClick={approve} disabled={approveDisabled || previewRefused || view.step === "busy" || view.step === "rejecting"}>
            {view.step === "busy" ? t(($) => $.decision_action.waiting) : t(($) => $.decision_action.approve)}
          </Button>
        )}
        <Button size="sm" variant="outline" onClick={reject} disabled={view.step === "busy" || view.step === "rejecting"}>{t(($) => $.decision_action.reject)}</Button>
      </div>
    </div>
  );
}

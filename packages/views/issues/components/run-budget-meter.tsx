"use client";

import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { openDecisionRequestsOptions } from "@multica/core/chat/queries";
import { useWorkspaceId } from "@multica/core/hooks";
import { runBudgetKeys, runBudgetTone, useRunBudget } from "@multica/core/run-budget";
import { memberListOptions } from "@multica/core/workspace/queries";
import { isRunBudgetActionType } from "@multica/core/issues/decision-action-preview";
import { Button } from "@multica/ui/components/ui/button";
import { DecisionActionApproval } from "../../common/decision-action-approval";
import { useT } from "../../i18n";

const TONE_CLASS = { ok: "text-foreground", warn: "text-warning", danger: "text-destructive" } as const;

type Kind = "raise" | "top_up";
type Asked = { kind: "idle" } | { kind: "busy" } | { kind: "failed" } | { kind: "asked"; actionId: string };

/** The action a request opened, approved in place with a passkey. The server accepts only an owner. */
function InlineApproval({ actionId, onOutcome }: { actionId: string; onOutcome: () => void }) {
  const { data } = useQuery({
    queryKey: ["decision-action", actionId],
    queryFn: () => api.getDecisionAction(actionId),
    retry: false,
  });
  if (!data) return null;
  return <DecisionActionApproval actionId={data.id} payloadSha256={data.payload_sha256} onOutcome={onOutcome} />;
}

/**
 * Run-budget meter for the issue page: runs used and left, the 80 % warning
 * and the exhausted state, with Raise and Top up. Both buttons only open a
 * request (A1d). An owner approves it in place; anyone else leaves it to the
 * owners. The meter hides for an unbudgeted issue.
 */
export function RunBudgetMeter({ issueId }: { issueId: string }) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const { data: budget } = useRunBudget(wsId, issueId);
  const qc = useQueryClient();
  const { data: members } = useQuery(memberListOptions(wsId));
  // An open budget request on this issue sits in the decision banner below: link to it instead of opening a second one.
  // Other open cards on the issue do not count.
  const { data: openRequests } = useQuery(openDecisionRequestsOptions(wsId));
  const hasOpenRequest = (openRequests ?? []).some(
    (r) => r.issue_id === issueId && isRunBudgetActionType(r.action?.action_type ?? ""),
  );
  const userId = useAuthStore((s) => s.user?.id);
  const [asked, setAsked] = useState<Asked>({ kind: "idle" });
  const windowId = budget?.windowId ?? null;
  // A new window is a new budget: an earlier request no longer applies.
  useEffect(() => setAsked({ kind: "idle" }), [windowId]);
  if (!budget || !budget.budgeted) return null;

  const refresh = () => void qc.invalidateQueries({ queryKey: runBudgetKeys.issue(wsId, issueId) });
  const isOwner = (members ?? []).some((m) => m.user_id === userId && m.role === "owner");
  const exhausted = budget.state === "at_limit" || budget.state === "over";
  const canAsk = budget.windowId !== null && asked.kind !== "busy";

  const request = async (kind: Kind) => {
    if (budget.windowId === null) return;
    setAsked({ kind: "busy" });
    try {
      const res = await api.requestRunBudgetAction(issueId, {
        kind,
        window_id: budget.windowId,
        ...(kind === "raise" && budget.raisePreview ? { new_cap: budget.raisePreview.limitAfter } : {}),
      });
      setAsked({ kind: "asked", actionId: res.requests[0]!.action_id });
    } catch {
      setAsked({ kind: "failed" });
    }
  };

  return (
    <div className="space-y-1.5 px-2" data-testid="run-budget-meter">
      <div className="flex items-baseline justify-between gap-2 text-caption">
        <span className={`font-medium ${TONE_CLASS[runBudgetTone(budget.state)]}`}>
          {t(($) => $.run_budget.runs, { used: budget.runsUsed, limit: budget.runsLimit })}
        </span>
        <span className="text-muted-foreground">{t(($) => $.run_budget.left, { count: Math.max(budget.runsLeft, 0) })}</span>
      </div>
      {budget.state === "near" && <p className="text-caption text-warning">{t(($) => $.run_budget.near)}</p>}
      {exhausted && <p className="text-caption text-destructive">{t(($) => $.run_budget.exhausted)}</p>}
      {hasOpenRequest ? (
        <a href="#issue-decision-banner" className="inline-block text-caption underline focus-visible:outline-2 focus-visible:outline-ring">
          {t(($) => $.run_budget.open_request)}
        </a>
      ) : (
      <div className="flex gap-1.5">
        {budget.canRaise && budget.raisePreview && (
          <Button size="sm" variant="outline" disabled={!canAsk} onClick={() => void request("raise")}>
            {t(($) => $.run_budget.raise)}
          </Button>
        )}
        <Button size="sm" variant="outline" disabled={!canAsk} onClick={() => void request("top_up")}>
          {t(($) => $.run_budget.top_up)}
        </Button>
      </div>
      )}
      {asked.kind === "failed" && (
        <p role="alert" className="text-caption text-destructive">{t(($) => $.run_budget.request_failed)}</p>
      )}
      {asked.kind === "asked" && members !== undefined &&
        (isOwner ? (
          <InlineApproval actionId={asked.actionId} onOutcome={refresh} />
        ) : (
          <p className="text-caption text-muted-foreground">{t(($) => $.run_budget.asked)}</p>
        ))}
    </div>
  );
}

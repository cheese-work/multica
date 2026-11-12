"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@multica/ui/components/ui/dialog";
import { api } from "@multica/core/api";
import { useT } from "../../i18n";
import { DecisionActionApproval } from "../../common/decision-action-approval";

const RAISE = "issue.run_budget.raise";
const TOP_UP = "issue.run_budget.top_up";
const SETTLED = ["executed", "failed", "rejected", "expired", "cancelled"];

/**
 * Passkey approval for the action a `?approve=<action>` link names. Only the
 * request's human can read it. The link is untrusted, so the panel offers
 * approval only for a run-budget action whose target is the issue on screen,
 * and it names that target and the cap before the passkey prompt. It is a
 * dialog so the link never lands on a silent page: a settled, unreadable or
 * foreign action says so in one sentence and tells the person to ask again.
 */
export function DecisionActionPanel({ actionId, issueId, issueLabel }: { actionId: string; issueId: string; issueLabel: string }) {
  const { t } = useT("common");
  const { data, isError } = useQuery({
    queryKey: ["decision-action", actionId],
    queryFn: () => api.getDecisionAction(actionId),
    retry: false,
  });
  const [open, setOpen] = useState(true);
  if (!data && !isError) return null;

  const cap = data?.params.new_cap;
  const isBudget = data?.action_type === RAISE || data?.action_type === TOP_UP;
  const targetsThisIssue = data?.params.issue_id === issueId;
  const approvable = data && isBudget && targetsThisIssue && (data.action_type === TOP_UP || typeof cap === "number");
  const title = t(($) => $.decision_action.panel.title);
  const settled = data && SETTLED.includes(data.state);
  const message = settled
    ? t(($) => $.decision_action.panel.settled, { state: t(($) => $.decision_action.state[data.state as "executed"]) })
    : data
      ? t(($) => $.decision_action.panel.mismatch)
      : t(($) => $.decision_action.panel.unreadable);

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent className="max-w-lg space-y-2">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription className="sr-only">{title}</DialogDescription>
        </DialogHeader>
        {approvable && !settled ? (
          <>
            <p className="text-caption">
              {data.action_type === RAISE
                ? t(($) => $.decision_action.panel.raise_cap, { cap: cap as number, issue: issueLabel })
                : t(($) => $.decision_action.panel.top_up, { issue: issueLabel })}
            </p>
            <DecisionActionApproval actionId={data.id} payloadSha256={data.payload_sha256} />
          </>
        ) : (
          <p role="alert" className="text-caption text-destructive">{message}</p>
        )}
      </DialogContent>
    </Dialog>
  );
}

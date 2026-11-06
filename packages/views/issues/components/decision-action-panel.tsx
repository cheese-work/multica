"use client";

import { useQuery } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { useT } from "../../i18n";
import { DecisionActionApproval } from "../../common/decision-action-approval";

const RAISE = "issue.run_budget.raise";
const TOP_UP = "issue.run_budget.top_up";

/**
 * Passkey approval for the action a `?approve=<action>` link names. Only the
 * request's human can read it. The link is untrusted, so the panel offers
 * approval only for a run-budget action whose target is the issue on screen,
 * and it names that target and the cap before the passkey prompt.
 */
export function DecisionActionPanel({ actionId, issueId, issueLabel }: { actionId: string; issueId: string; issueLabel: string }) {
  const { t } = useT("common");
  const { data, isError } = useQuery({
    queryKey: ["decision-action", actionId],
    queryFn: () => api.getDecisionAction(actionId),
    retry: false,
  });
  if (!data && !isError) return null;

  const cap = data?.params.new_cap;
  const isBudget = data?.action_type === RAISE || data?.action_type === TOP_UP;
  const targetsThisIssue = data?.params.issue_id === issueId;
  const approvable = data && isBudget && targetsThisIssue && (data.action_type === TOP_UP || typeof cap === "number");
  const title = t(($) => $.decision_action.panel.title);

  return (
    <section aria-label={title} className="mx-auto my-4 max-w-2xl space-y-2 rounded-xl border border-warning/40 bg-warning/5 p-3">
      <h2 className="text-label font-semibold">{title}</h2>
      {approvable ? (
        <>
          <p className="text-caption">
            {data.action_type === RAISE
              ? t(($) => $.decision_action.panel.raise_cap, { cap: cap as number, issue: issueLabel })
              : t(($) => $.decision_action.panel.top_up, { issue: issueLabel })}
          </p>
          <DecisionActionApproval actionId={data.id} payloadSha256={data.payload_sha256} />
        </>
      ) : (
        <p role="alert" className="text-caption text-destructive">
          {data ? t(($) => $.decision_action.panel.mismatch) : t(($) => $.decision_action.panel.unreadable)}
        </p>
      )}
    </section>
  );
}

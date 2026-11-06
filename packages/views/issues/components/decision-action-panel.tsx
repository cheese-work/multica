"use client";

import { useQuery } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { useT } from "../../i18n";
import { DecisionActionApproval } from "../../common/decision-action-approval";

/** Passkey approval for the action a `?approve=<action>` link names. Only the request's human can read it. */
export function DecisionActionPanel({ actionId }: { actionId: string }) {
  const { t } = useT("common");
  const { data, isError } = useQuery({
    queryKey: ["decision-action", actionId],
    queryFn: () => api.getDecisionAction(actionId),
    retry: false,
  });
  if (!data && !isError) return null;

  const cap = data?.params.new_cap;
  const summary =
    data?.action_type === "issue.run_budget.raise" && typeof cap === "number" ? t(($) => $.decision_action.panel.raise_cap, { cap })
    : data?.action_type === "issue.run_budget.top_up" ? t(($) => $.decision_action.panel.top_up)
    : t(($) => $.decision_action.panel.other, { type: data?.action_type ?? "" });

  return (
    <section aria-label={t(($) => $.decision_action.panel.title)} className="mx-auto my-4 max-w-2xl space-y-2 rounded-xl border border-warning/40 bg-warning/5 p-3">
      <h2 className="text-label font-semibold">{t(($) => $.decision_action.panel.title)}</h2>
      {data ? (
        <>
          <p className="text-caption">{summary}</p>
          <DecisionActionApproval actionId={data.id} payloadSha256={data.payload_sha256} />
        </>
      ) : (
        <p role="alert" className="text-caption text-destructive">{t(($) => $.decision_action.panel.unreadable)}</p>
      )}
    </section>
  );
}

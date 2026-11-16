"use client";

import { useQuery } from "@tanstack/react-query";
import { openDecisionRequestsOptions } from "@multica/core/chat/queries";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import type { OpenDecisionRequest } from "@multica/core/types";
import { Gauge } from "@multica/ui/icons";
import { useT } from "../i18n";
import { AppLink } from "../navigation";

const WORKFLOW_BUDGET_SET = "workflow.budget.set";

/** The cap a pending `workflow.budget.set` card proposes, or null when the card carries none. */
function proposedCap(after: unknown): number | null {
  const cap = (after as { max_runs_per_issue?: unknown } | null)?.max_runs_per_issue;
  return typeof cap === "number" ? cap : null;
}

/**
 * Banner for the workflow editor: an owner still has to approve a run-cap
 * change for this workflow. Links to the card, which sits on the issue that
 * asked. Hidden when no open card targets this workflow.
 */
export function WorkflowBudgetRequestBanner({ workflowId }: { workflowId: string }) {
  const wsId = useWorkspaceId();
  const { data: open } = useQuery(openDecisionRequestsOptions(wsId));
  const request = (open ?? []).find(
    (r) => r.action?.action_type === WORKFLOW_BUDGET_SET && r.action.config_change?.target_id === workflowId,
  );
  return request ? <Banner request={request} /> : null;
}

function Banner({ request }: { request: OpenDecisionRequest }) {
  const { t } = useT("issues");
  const paths = useWorkspacePaths();
  const cap = proposedCap(request.action?.config_change?.after);
  const issue = request.issue_identifier || request.issue_id;
  return (
    <div
      role="status"
      data-testid="workflow-budget-request-banner"
      className="flex items-start gap-2 rounded-lg border border-warning/40 bg-warning/10 px-3 py-2 text-label"
    >
      <Gauge aria-hidden className="mt-0.5 size-4 shrink-0 text-warning" />
      <p className="min-w-0">
        {cap === null
          ? t(($) => $.workflows.editor.budget_request_pending)
          : t(($) => $.workflows.editor.budget_request_pending_cap, { cap })}{" "}
        <AppLink
          href={`${paths.issueDetail(request.issue_id)}#issue-decision-banner`}
          className="font-medium underline-offset-2 hover:underline focus-visible:outline-2 focus-visible:outline-ring"
        >
          {issue}
        </AppLink>
      </p>
    </div>
  );
}

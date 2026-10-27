import type {
  HandoffPreviewBinding,
  UpdateIssueRequest,
  WorkflowHandoffPreview,
} from "@multica/core/types";

import { api } from "@/data/api";

/**
 * Status changes and project workflows (MUL-7420). Entering a workflow step
 * can hand the issue to the step's handler and stop the previous assignee's
 * runs, so a status is never written blind: the server previews the change,
 * the person confirms what it shows when it hands off, and the write carries
 * the binding of what was previewed. The server refuses (409
 * `handoff_preview_stale`) when the issue or its workflow moved since.
 * Mirrors `previewBinding` and the status picker in packages/views.
 */

/** What a preview showed, as the binding the write is checked against. A
 *  preview that found no handoff binds that result — no handler — so a handler
 *  that became resolvable since is refused instead of taking the issue. */
export function previewBinding(preview: WorkflowHandoffPreview): HandoffPreviewBinding {
  return {
    previous_assignee_type: preview.previous_assignee_type ?? null,
    previous_assignee_id: preview.previous_assignee_id ?? null,
    handler_type: preview.handoff ? preview.handler_type ?? null : null,
    handler_id: preview.handoff ? preview.handler_id ?? null : null,
  };
}

export type StatusChangePlan =
  /** The preview found no handoff: write `request` as is. */
  | { kind: "apply"; request: UpdateIssueRequest }
  /** The move hands the issue off: show `preview`, then write `request(stop)`. */
  | {
      kind: "confirm";
      preview: WorkflowHandoffPreview;
      request: (stopPreviousRuns: boolean) => UpdateIssueRequest;
    };

/**
 * Previews moving the issue to `status` and decides what the write needs.
 * Rejects when the preview fails (offline, 400 for a status the workflow omits,
 * malformed body): the caller reports it and writes nothing.
 */
export async function planStatusChange(issueId: string, status: string): Promise<StatusChangePlan> {
  const preview = await api.previewWorkflowHandoff(issueId, status);
  const handoff_preview = previewBinding(preview);
  if (!preview.handoff) return { kind: "apply", request: { status, handoff_preview } };
  return {
    kind: "confirm",
    preview,
    request: (stopPreviousRuns) => ({
      status,
      handoff_preview,
      ...(stopPreviousRuns ? { stop_previous_assignee_runs: true } : {}),
    }),
  };
}

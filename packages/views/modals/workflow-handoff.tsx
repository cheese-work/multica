"use client";

import { toast } from "sonner";
import { useUpdateIssue } from "@multica/core/issues/mutations";
import { useT } from "../i18n";
import { WorkflowHandoffConfirmDialog } from "../workflows/handoff-confirm-dialog";

/**
 * The workflow handoff confirmation for entry points that cannot host the
 * dialog themselves, such as an issue's context menu (MUL-7420). Applies the
 * status change once confirmed.
 */
export function WorkflowHandoffModal({
  onClose,
  data,
}: {
  onClose: () => void;
  data: Record<string, unknown> | null;
}) {
  const { t } = useT("issues");
  const { mutateAsync: updateIssue } = useUpdateIssue();
  const issueId = typeof data?.issueId === "string" ? data.issueId : "";
  const identifier = typeof data?.identifier === "string" ? data.identifier : "";
  const status = typeof data?.status === "string" ? data.status : "";
  if (!issueId || !status) return null;
  return (
    <WorkflowHandoffConfirmDialog
      issue={{ id: issueId, identifier }}
      toStatus={status}
      onCancel={onClose}
      onConfirm={({ stopPreviousRuns, preview }) => {
        onClose();
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
        updateIssue.mutate(
          {
            id: issueId,
            status,
            ...(stopPreviousRuns ? { stop_previous_assignee_runs: true } : {}),
            ...(preview ? { handoff_preview: preview } : {}),
          },
          {
            onError: (err) =>
              toast.error(err instanceof Error && err.message ? err.message : t(($) => $.detail.update_failed)),
          },
||||||| 2ae2f08005
        updateIssue.mutate(
          { id: issueId, status, ...(stopPreviousRuns ? { stop_previous_assignee_runs: true } : {}) },
          {
            onError: (err) =>
              toast.error(err instanceof Error && err.message ? err.message : t(($) => $.detail.update_failed)),
          },
=======
        // Closing unmounts this modal, so the failure follows the promise.
        updateIssue({ id: issueId, status, ...(stopPreviousRuns ? { stop_previous_assignee_runs: true } : {}) }).catch(
          (err) => toast.error(err instanceof Error && err.message ? err.message : t(($) => $.detail.update_failed)),
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
        );
      }}
    />
  );
}

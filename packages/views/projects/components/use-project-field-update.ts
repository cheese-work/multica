"use client";

import { useCallback } from "react";
import { toast } from "sonner";
import { useUpdateProject } from "@multica/core/projects/mutations";
import type { UpdateProjectRequest } from "@multica/core/types";
import { useT } from "../../i18n";

/**
 * Field updates for one project. A status change waits for the server (no
 * optimistic patch), so the caller gets `statusPending` for the control and a
 * toast carries the server error when it fails. Other fields stay optimistic.
 */
export function useProjectFieldUpdate(projectId: string) {
  const { t } = useT("search");
  const { mutate, isPending, variables } = useUpdateProject();
  const handleUpdate = useCallback(
    (data: UpdateProjectRequest) =>
      mutate(
        { id: projectId, ...data },
        {
          onError: (err) => {
            if (!("status" in data)) return;
            toast.error(
              err instanceof Error && err.message ? err.message : t(($) => $.toast.update_failed),
            );
          },
        },
      ),
    [mutate, projectId, t],
  );
  const statusPending = isPending && !!variables && "status" in variables;
  return { handleUpdate, statusPending };
}

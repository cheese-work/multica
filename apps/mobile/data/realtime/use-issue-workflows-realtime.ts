/**
 * Workspace workflow list realtime (CHE-2822). Mounted globally so an open
 * status picker never offers statuses a workflow edit already removed.
 *
 * Event coverage:
 *   - issue_workflow:changed → invalidate the list; the payload carries no
 *                              row, so one list read answers every write.
 *   - reconnect              → invalidate the list (edits may have been missed).
 */
import { useQueryClient } from "@tanstack/react-query";
import { issueWorkflowKeys } from "@/data/queries/issue-workflows";
import { useWSSubscriptions } from "@/lib/use-ws-subscriptions";

export function useIssueWorkflowsRealtime() {
  const qc = useQueryClient();

  useWSSubscriptions(
    (ws, wsId) => {
      const invalidateList = () =>
        qc.invalidateQueries({ queryKey: issueWorkflowKeys.list(wsId) });

      return [ws.on("issue_workflow:changed", invalidateList), ws.onReconnect(invalidateList)];
    },
    [qc],
  );
}

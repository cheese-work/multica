/**
 * Workflow-override picker route for an existing issue (CHE-2822). Setting
 * "Follow project" clears the override. The server limits the switch to
 * workspace admins and the project lead, so a refusal is shown, not hidden.
 */
import { Alert } from "react-native";
import { useLocalSearchParams, router } from "expo-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { WorkflowPickerBody } from "@/components/issue/pickers/workflow-picker-body";
import { api } from "@/data/api";
import { issueDetailOptions } from "@/data/queries/issues";
import { issueWorkflowListOptions } from "@/data/queries/issue-workflows";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useT } from "@/lib/i18n";

export default function IssueWorkflowPickerRoute() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const qc = useQueryClient();
  const { t } = useT("issues");
  const { data: issue } = useQuery(issueDetailOptions(wsId, id));
  const { data: workflows = [] } = useQuery(issueWorkflowListOptions(wsId));

  const choose = async (next: string | null) => {
    if (next === (issue?.workflow_id ?? null)) {
      router.back();
      return;
    }
    try {
      await api.setIssueWorkflow(id, next);
      await qc.invalidateQueries({ queryKey: issueDetailOptions(wsId, id).queryKey });
      router.back();
    } catch (err) {
      Alert.alert(
        t("workflow.failed_title"),
        err instanceof Error ? err.message : t("common:states.error"),
      );
    }
  };

  return (
    <WorkflowPickerBody
      value={issue?.workflow_id ?? null}
      workflows={workflows}
      onChange={(next) => void choose(next)}
    />
  );
}

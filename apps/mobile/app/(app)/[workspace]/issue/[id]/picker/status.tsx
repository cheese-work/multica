/**
 * Status picker route for an existing issue — presented as a formSheet
 * (UISheetPresentationController) by the parent Stack.
 *
 * Self-contained: reads the issue from the TanStack Query detail cache,
 * writes through `useUpdateIssue` on selection, then `router.back()`s. No
 * onChange callback to a parent.
 *
 * If the cache is cold (rare — the user reaches this screen by tapping
 * a chip on the issue-detail page that already populated it), the picker
 * still renders against the current value of `todo` and the optimistic
 * mutation patches the cache when the user picks.
 *
 * A project workflow can hand the issue off when it enters a status, so the
 * change is previewed first (`planStatusChange`): a handoff is confirmed in a
 * native alert, and the write carries the previewed binding either way. A
 * failed preview, a cancelled confirmation or a stale binding writes nothing.
 */
import { useRef } from "react";
import { Alert } from "react-native";
import { useLocalSearchParams, router } from "expo-router";
import { useQuery } from "@tanstack/react-query";
import type { IssueStatus, UpdateIssueRequest } from "@multica/core/types";
import { StatusPickerBody } from "@/components/issue/pickers/status-picker-body";
import { ApiError } from "@/data/api";
import { issueDetailOptions } from "@/data/queries/issues";
import { useUpdateIssue } from "@/data/mutations/issues";
import { useActorLookup } from "@/data/use-actor-name";
import { useWorkspaceStore } from "@/data/workspace-store";
import { planStatusChange, type StatusChangePlan } from "@/data/workflow-handoff";
import { statusOptions } from "@/lib/issue-status";
import { useIssueStatuses } from "@/lib/use-issue-statuses";
import { useT } from "@/lib/i18n";

export default function IssueStatusPickerRoute() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const { data: issue } = useQuery(issueDetailOptions(wsId, id));
  const updateIssue = useUpdateIssue(id);
  const { t } = useT("issues");
  const catalog = useIssueStatuses();
  const { getName } = useActorLookup();
  // One preview or write at a time: a second tap must not start another.
  const busy = useRef(false);

  const write = async (request: UpdateIssueRequest) => {
    try {
      await updateIssue.mutateAsync(request);
      router.back();
    } catch (err) {
      const stale = err instanceof ApiError && err.status === 409;
      Alert.alert(
        t("workflow_handoff.failed_title"),
        stale
          ? t("workflow_handoff.stale_message")
          : err instanceof Error
            ? err.message
            : t("common:states.error"),
      );
    }
  };

  const confirm = (plan: Extract<StatusChangePlan, { kind: "confirm" }>, next: IssueStatus) => {
    const { preview } = plan;
    const handler = getName(preview.handler_type, preview.handler_id);
    const stopsRuns = preview.previous_runs.length > 0;
    const label = statusOptions(catalog).find((option) => option.key === next)?.label ?? next;
    Alert.alert(
      t("workflow_handoff.confirm_title", { status: label }),
      t(stopsRuns ? "workflow_handoff.confirm_message_runs" : "workflow_handoff.confirm_message", { handler }),
      [
        { text: t("workflow_handoff.cancel"), style: "cancel" },
        { text: t("workflow_handoff.move"), onPress: () => void write(plan.request(false)) },
        ...(stopsRuns
          ? [{ text: t("workflow_handoff.move_and_stop"), onPress: () => void write(plan.request(true)) }]
          : []),
      ],
    );
  };

  const choose = async (next: IssueStatus) => {
    if (busy.current) return;
    if (next === issue?.status) {
      router.back();
      return;
    }
    busy.current = true;
    try {
      const plan = await planStatusChange(id, next);
      if (plan.kind === "apply") await write(plan.request);
      else confirm(plan, next);
    } catch (err) {
      Alert.alert(
        t("workflow_handoff.check_failed_title"),
        t("workflow_handoff.check_failed_message", { message: err instanceof Error ? err.message : "" }),
        [
          { text: t("workflow_handoff.cancel"), style: "cancel" },
          { text: t("workflow_handoff.retry"), onPress: () => void choose(next) },
        ],
      );
    } finally {
      busy.current = false;
    }
  };

  return <StatusPickerBody value={issue?.status ?? "todo"} onChange={(next) => void choose(next)} />;
}

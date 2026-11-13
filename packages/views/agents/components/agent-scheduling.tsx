"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import type { Agent, MemberWithUser } from "@multica/core/types";
import { api } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { agentListOptions, workspaceKeys } from "@multica/core/workspace/queries";
import { Badge } from "@multica/ui/components/ui/badge";
import { Input } from "@multica/ui/components/ui/input";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useT, useTimeAgo } from "../../i18n";

/** Shown only while an owner/admin has paused claims for the agent. */
export function AgentPausedBadge({ agent }: { agent: Pick<Agent, "scheduling_paused_at"> }) {
  const { t } = useT("agents");
  if (!agent.scheduling_paused_at) return null;
  return <Badge variant="outline">{t(($) => $.scheduling.paused_badge)}</Badge>;
}

/** Who paused the agent, when and why. Renders nothing for a running agent. */
export function AgentPauseNote({
  agent,
  members,
}: {
  agent: Pick<Agent, "scheduling_paused_at" | "scheduling_paused_by" | "scheduling_paused_reason">;
  members: Pick<MemberWithUser, "user_id" | "name">[];
}) {
  const { t } = useT("agents");
  const timeAgo = useTimeAgo();
  if (!agent.scheduling_paused_at) return null;
  const by = members.find((m) => m.user_id === agent.scheduling_paused_by)?.name;
  return (
    <p className="text-xs text-muted-foreground">
      {by
        ? t(($) => $.scheduling.paused_by_note, { name: by, time: timeAgo(agent.scheduling_paused_at!) })
        : t(($) => $.scheduling.paused_at_note, { time: timeAgo(agent.scheduling_paused_at!) })}
      {agent.scheduling_paused_reason
        ? ` ${t(($) => $.scheduling.paused_reason_note, { reason: agent.scheduling_paused_reason })}`
        : ""}
    </p>
  );
}

/** Same badge, looked up by agent id: issue assignee and squad member rows only carry the id. */
export function AgentPausedBadgeById({ agentId }: { agentId: string }) {
  const wsId = useWorkspaceId();
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const agent = agents.find((a) => a.id === agentId);
  return agent ? <AgentPausedBadge agent={agent} /> : null;
}

/** Pause / resume is for a workspace owner or admin; the API refuses anyone else. */
export function canScheduleAgent(role: string | undefined, isArchived: boolean): boolean {
  return !isArchived && (role === "owner" || role === "admin");
}

/**
 * Pause / resume confirm dialog (CHE-5334). Pause shows what the pause leaves
 * waiting and offers to cancel active runs; the default lets running runs
 * finish. The server stays the source of truth for who may do this.
 */
export function AgentSchedulingDialog({
  agent,
  action,
  onClose,
}: {
  agent: Agent;
  action: "pause" | "resume";
  onClose: () => void;
}) {
  const { t } = useT("agents");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const [cancelActive, setCancelActive] = useState(false);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const isPause = action === "pause";

  const impact = useQuery({
    queryKey: ["agent-scheduling-impact", agent.id],
    queryFn: () => api.getAgentSchedulingImpact(agent.id),
    enabled: isPause,
    staleTime: 0,
    gcTime: 0,
  });

  const confirm = async () => {
    setBusy(true);
    try {
      if (isPause) {
        await api.pauseAgent(agent.id, reason.trim() || undefined);
        if (cancelActive) await api.cancelAgentTasks(agent.id);
      } else {
        await api.resumeAgent(agent.id);
      }
      toast.success(t(($) => (isPause ? $.scheduling.paused_toast : $.scheduling.resumed_toast)));
      onClose();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.scheduling.failed_toast));
      setBusy(false);
    } finally {
      // A pause that succeeded before a failed cancel still changed the agent.
      qc.invalidateQueries({ queryKey: workspaceKeys.agents(wsId) });
    }
  };

  const data = impact.data;
  return (
    <AlertDialog open onOpenChange={(v) => !v && !busy && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {isPause
              ? t(($) => $.scheduling.pause_title, { name: agent.name })
              : t(($) => $.scheduling.resume_title, { name: agent.name })}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {isPause
              ? t(($) => $.scheduling.pause_description)
              : t(($) => $.scheduling.resume_description)}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {isPause && (
          <div className="space-y-3 text-sm">
            {impact.isPending && (
              <p className="text-muted-foreground">{t(($) => $.scheduling.impact_loading)}</p>
            )}
            {impact.isError && (
              <p className="text-muted-foreground">{t(($) => $.scheduling.impact_failed)}</p>
            )}
            {data && (
              <ul className="list-disc pl-5">
                <li>{t(($) => $.scheduling.impact_running, { count: data.running_tasks })}</li>
                <li>{t(($) => $.scheduling.impact_queued, { count: data.queued_tasks })}</li>
                <li>{t(($) => $.scheduling.impact_issues, { count: data.open_issues })}</li>
                <li>{t(($) => $.scheduling.impact_wakeups, { count: data.enabled_wakeups })}</li>
              </ul>
            )}
            <Input
              value={reason}
              maxLength={500}
              onChange={(e) => setReason(e.target.value)}
              placeholder={t(($) => $.scheduling.reason_placeholder)}
              aria-label={t(($) => $.scheduling.reason_label)}
              disabled={busy}
            />
            <label className="flex items-start gap-2">
              <Checkbox
                checked={cancelActive}
                onCheckedChange={(v) => setCancelActive(v === true)}
                className="mt-0.5"
              />
              <span>
                {t(($) => $.scheduling.cancel_active)}
                <span className="block text-xs text-muted-foreground">
                  {t(($) => $.scheduling.cancel_active_hint)}
                </span>
              </span>
            </label>
          </div>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={busy}>{t(($) => $.scheduling.keep)}</AlertDialogCancel>
          <AlertDialogAction
            variant={isPause ? "destructive" : "default"}
            disabled={busy}
            onClick={(e) => {
              e.preventDefault();
              void confirm();
            }}
          >
            {isPause
              ? t(($) => $.scheduling.pause_action)
              : t(($) => $.scheduling.resume_action)}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

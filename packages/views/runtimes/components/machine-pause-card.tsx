"use client";

import { useId, useState } from "react";
import { AlertCircle, Check, ChevronDown, Clock, Loader2, Lock, Pause, Play } from "lucide-react";
import { toast } from "sonner";
import { usePauseRuntimeMachine, useResumeRuntimeMachine } from "@multica/core/runtimes";
import type { MemberWithUser, RuntimePause, RuntimePauseReason } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuGroup,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { cn } from "@multica/ui/lib/utils";
import { useT, useTimeAgo } from "../../i18n";
import {
  deriveMachinePauseState,
  endedPauseNotice,
  pauseRightsHolders,
  pauseTarget,
} from "./machine-pause-state";
import type { RuntimeMachine } from "./runtime-machines";
import { useMachinePauseFormat } from "./use-machine-pause-format";

export interface MachinePauseCardProps {
  machine: RuntimeMachine;
  wsId: string;
  currentUserId: string | undefined;
  members: MemberWithUser[];
  now: number;
  /** The runtime list failed to refresh: its pause state is not trustworthy. */
  loadFailed?: boolean;
  onRetry?: () => void;
}

const REASONS: RuntimePauseReason[] = ["maintenance", "incident", "suspended"];

function samePause(a: RuntimePause | null, b: RuntimePause | null): boolean {
  return a?.reason === b?.reason && a?.paused_at === b?.paused_at && a?.expires_at === b?.expires_at;
}

/**
 * Machine-wide pause control. The server decides who may pause; the card only
 * offers the action to a viewer who owns a runtime here or owns the workspace,
 * and shows everyone else the same state read-only.
 */
export function MachinePauseCard({
  machine,
  wsId,
  currentUserId,
  members,
  now,
  loadFailed = false,
  onRetry,
}: MachinePauseCardProps) {
  const { t } = useT("runtimes");
  const timeAgo = useTimeAgo();
  const { reasonName, duration, clock } = useMachinePauseFormat();
  const headingId = useId();
  const pauseMutation = usePauseRuntimeMachine(wsId);
  const resumeMutation = useResumeRuntimeMachine(wsId);

  const state = deriveMachinePauseState(machine.runtimes, {
    now,
    online: machine.onlineCount > 0,
    fallbackTasks: machine.runningCount,
  });
  const role = members.find((member) => member.user_id === currentUserId)?.role;
  const target = pauseTarget(machine.runtimes, currentUserId, role);
  const canAct = target !== null && !loadFailed;

  // Remember the last live pause so a pause that runs out while the page is open
  // can be named. Adjusted during render, not in an effect, so no frame is stale.
  const [lastPause, setLastPause] = useState<RuntimePause | null>(null);
  if (state.pause && !samePause(state.pause, lastPause)) {
    setLastPause(state.pause);
  } else if (!state.pause && lastPause && Date.parse(lastPause.expires_at) > now) {
    // Lifted before its expiry (Resume): nothing ran out, so nothing to report.
    setLastPause(null);
  }
  const ended = endedPauseNotice(lastPause, state, now);

  const pausedBy = state.pause?.paused_by
    ? members.find((member) => member.user_id === state.pause?.paused_by)?.name
    : undefined;
  const pausing = pauseMutation.isPending;
  const resuming = resumeMutation.isPending;
  const paused = state.phase !== "running";
  const askNames = pauseRightsHolders(machine.runtimes, members);

  const pause = (reason: RuntimePauseReason) => {
    if (!target) return;
    pauseMutation.mutate(
      { runtimeId: target.id, reason },
      { onError: () => toast.error(t(($) => $.machine_pause.toast.pause_failed)) },
    );
  };
  const resume = () => {
    if (!target) return;
    resumeMutation.mutate(target.id, {
      onError: () => toast.error(t(($) => $.machine_pause.toast.resume_failed)),
    });
  };

  const tasks = state.tasks;
  let title: string;
  let detail: string | null = null;
  if (loadFailed) {
    title = t(($) => $.machine_pause.error.title);
    detail = t(($) => $.machine_pause.error.hint);
  } else if (paused) {
    const reason = reasonName(state.reason ?? "maintenance");
    title = state.online
      ? t(($) => $.machine_pause.paused, { reason })
      : t(($) => $.machine_pause.paused_offline, { reason });
    const lastReport = machine.lastSeenAt ? timeAgo(machine.lastSeenAt) : "";
    const progress =
      state.phase === "drained"
        ? t(($) => $.machine_pause.safe_to_shut_down)
        : state.online
          ? t(($) => $.machine_pause.draining, { count: tasks })
          : t(($) => $.machine_pause.draining_offline, { count: tasks, time: lastReport });
    const by =
      pausedBy && state.pause
        ? t(($) => $.machine_pause.paused_by, {
            name: pausedBy,
            time: clock(state.pause.paused_at),
          })
        : null;
    detail = by ? `${progress} · ${by}` : progress;
  } else if (!state.online) {
    title = t(($) => $.machine_pause.offline);
    detail = machine.lastSeenAt
      ? t(($) => $.machine_pause.offline_last_seen, { time: timeAgo(machine.lastSeenAt) })
      : null;
  } else {
    title = t(($) => $.machine_pause.taking_work);
    detail = t(($) => $.machine_pause.tasks_running, { count: tasks });
  }

  const endsIn =
    !loadFailed && state.pause && state.remainingMs !== null
      ? t(($) => $.machine_pause.ends_in, {
          duration: duration(state.remainingMs),
          time: clock(state.pause.expires_at),
        })
      : null;

  return (
    <section
      aria-labelledby={headingId}
      aria-busy={pausing || resuming}
      className="mb-6 rounded-lg border bg-card"
    >
      <div className="flex items-baseline justify-between gap-2 border-b px-4 py-3">
        <h2 id={headingId} className="text-body font-semibold">
          {t(($) => $.machine_pause.title)}
        </h2>
        {(machine.deviceSummary ?? machine.subtitle) && (
          <span className="truncate text-caption text-muted-foreground">
            {machine.deviceSummary ?? machine.subtitle}
          </span>
        )}
      </div>

      <div className="flex flex-col gap-3 px-4 py-3.5 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
        <div className="flex min-w-0 items-start gap-2.5">
          <StatusMark phase={state.phase} online={state.online} failed={loadFailed} />
          <div className="min-w-0">
            <div className="text-body font-semibold">{title}</div>
            {detail && <div className="text-caption text-muted-foreground">{detail}</div>}
            {endsIn && (
              <div className="mt-0.5 flex items-center gap-1 text-caption">
                <Clock aria-hidden="true" className="size-3 text-muted-foreground" />
                {state.endingSoon ? (
                  <span>
                    <strong className="font-semibold">{endsInLead(endsIn)}</strong>{" "}
                    {endsInTail(endsIn)}
                  </span>
                ) : (
                  endsIn
                )}
              </div>
            )}
          </div>
        </div>

        <div className="flex shrink-0 items-center gap-2">
          {loadFailed && (
            <Button type="button" size="sm" variant="outline" onClick={onRetry}>
              {t(($) => $.machine_pause.error.retry)}
            </Button>
          )}
          {paused && !loadFailed ? (
            <Button
              type="button"
              size="sm"
              disabled={!canAct || resuming}
              aria-busy={resuming}
              onClick={resume}
            >
              {resuming ? (
                <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
              ) : (
                <Play aria-hidden="true" className="size-3.5" />
              )}
              {t(($) => $.machine_pause.resume)}
            </Button>
          ) : (
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    disabled={!canAct || pausing}
                    aria-busy={pausing}
                  >
                    {pausing ? (
                      <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
                    ) : (
                      <Pause aria-hidden="true" className="size-3.5" />
                    )}
                    {t(($) => $.machine_pause.pause)}
                    <ChevronDown aria-hidden="true" className="size-3.5 opacity-70" />
                  </Button>
                }
              />
              <DropdownMenuContent align="end" className="w-80">
                <DropdownMenuGroup>
                  <DropdownMenuLabel className="whitespace-normal font-normal">
                    {t(($) => $.machine_pause.menu_hint)}
                  </DropdownMenuLabel>
                  {REASONS.map((reason) => (
                    <DropdownMenuItem
                      key={reason}
                      className="flex-col items-start gap-0"
                      onClick={() => pause(reason)}
                    >
                      <span className="font-medium">
                        {t(($) => $.machine_pause.reasons[reason].label)}
                      </span>
                      <span className="text-caption text-muted-foreground">
                        {t(($) => $.machine_pause.reasons[reason].hint)}
                      </span>
                    </DropdownMenuItem>
                  ))}
                </DropdownMenuGroup>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </div>
      </div>

      {loadFailed && (
        <Note tone="error" icon={<AlertCircle aria-hidden="true" className="size-3 text-destructive" />} role="alert">
          {t(($) => $.machine_pause.error.alert)}
        </Note>
      )}
      {!loadFailed && !canAct && (
        <Note icon={<Lock aria-hidden="true" className="size-3" />} role="note">
          {askNames.length > 0
            ? t(($) => $.machine_pause.no_rights_ask, { names: askNames.join(", ") })
            : t(($) => $.machine_pause.no_rights)}
        </Note>
      )}
      {!loadFailed && ended && (
        <Note icon={<Check aria-hidden="true" className="size-3 text-success" />} role="status">
          {(machine.onlineCount > 0
            ? t(($) => $.machine_pause.ended, {
                reason: reasonName(ended.reason),
                time: clock(ended.expires_at),
                machine: machine.title,
              })
            : t(($) => $.machine_pause.ended_offline, {
                reason: reasonName(ended.reason),
                time: clock(ended.expires_at),
                machine: machine.title,
              }))}
        </Note>
      )}
    </section>
  );
}

// The "(16:20 GMT+7)" tail of the expiry line stays regular weight when the lead is bold.
function endsInLead(text: string): string {
  const cut = text.lastIndexOf(" (");
  return cut === -1 ? text : text.slice(0, cut);
}

function endsInTail(text: string): string {
  const cut = text.lastIndexOf(" (");
  return cut === -1 ? "" : text.slice(cut + 1);
}

function StatusMark({
  phase,
  online,
  failed,
}: {
  phase: "running" | "draining" | "drained";
  online: boolean;
  failed: boolean;
}) {
  if (!failed && phase === "drained" && online) {
    return <Check aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-success" />;
  }
  const color =
    failed || !online ? "bg-faint-foreground" : phase === "draining" ? "bg-warning" : "bg-success";
  return <span aria-hidden="true" className={cn("mt-1.5 size-2 shrink-0 rounded-full", color)} />;
}

function Note({
  children,
  icon,
  role,
  tone = "neutral",
}: {
  children: React.ReactNode;
  icon: React.ReactNode;
  role: "alert" | "note" | "status";
  tone?: "neutral" | "error";
}) {
  return (
    <div
      role={role}
      className={cn(
        "mx-4 mb-3 flex gap-2 rounded-lg px-2.5 py-2 text-caption",
        tone === "error" ? "bg-destructive/10 text-foreground" : "bg-muted text-muted-foreground",
      )}
    >
      <span className="mt-0.5 shrink-0">{icon}</span>
      <span>{children}</span>
    </div>
  );
}

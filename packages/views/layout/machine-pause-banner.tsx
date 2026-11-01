"use client";

import { useEffect, useMemo, useState } from "react";
import { AlertTriangle, Check, Loader2 } from "@multica/ui/icons";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWSEvent } from "@multica/core/realtime";
import { useResumeRuntimeMachine } from "@multica/core/runtimes";
import { runtimeKeys, runtimeListOptions } from "@multica/core/runtimes/queries";
import { Button } from "@multica/ui/components/ui/button";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";
import {
  deriveMachinePauseState,
  pauseTarget,
  type MachinePauseState,
} from "../runtimes/components/machine-pause-state";
import {
  buildRuntimeMachines,
  type RuntimeMachine,
} from "../runtimes/components/runtime-machines";
import { useMachinePauseFormat } from "../runtimes/components/use-machine-pause-format";

const TICK_MS = 30_000;

function useNowTick(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => clearInterval(id);
  }, []);
  return now;
}

/**
 * Warns the owner of a machine, on every page, while that machine is paused.
 * Only the owner of a runtime on the paused machine sees it: other members
 * and workspace owners find the pause on the machine page.
 */
export function MachinePauseBanner() {
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const currentUserId = useAuthStore((state) => state.user?.id);
  const { data: runtimes = [] } = useQuery(runtimeListOptions(wsId));
  const now = useNowTick();
  useWSEvent("daemon:register", () => {
    qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
  });

  const paused = useMemo(() => {
    if (!currentUserId) return [];
    return buildRuntimeMachines(runtimes, { now, currentUserId }).flatMap((machine) => {
      if (!machine.runtimes.some((runtime) => runtime.owner_id === currentUserId)) return [];
      const state = deriveMachinePauseState(machine.runtimes, {
        now,
        online: machine.onlineCount > 0,
        fallbackTasks: 0,
      });
      return state.phase === "running" ? [] : [{ machine, state }];
    });
  }, [runtimes, now, currentUserId]);

  if (paused.length === 0) return null;
  return (
    <div className="flex shrink-0 flex-col gap-2 px-3 pt-3">
      {paused.map(({ machine, state }) => (
        <PausedMachineBanner
          key={machine.id}
          machine={machine}
          state={state}
          wsId={wsId}
          currentUserId={currentUserId}
        />
      ))}
    </div>
  );
}

function PausedMachineBanner({
  machine,
  state,
  wsId,
  currentUserId,
}: {
  machine: RuntimeMachine;
  state: MachinePauseState;
  wsId: string;
  currentUserId: string | undefined;
}) {
  const { t } = useT("runtimes");
  const { reasonName, duration } = useMachinePauseFormat();
  const resume = useResumeRuntimeMachine(wsId);
  const target = pauseTarget(machine.runtimes, currentUserId, undefined);
  const safe = state.phase === "drained" && state.online;
  const count = state.tasks;

  const progress = safe
    ? t(($) => $.machine_pause.banner.drained)
    : state.online
      ? t(($) => $.machine_pause.banner.tasks_left, { count })
      : t(($) => $.machine_pause.banner.tasks_left_stale, { count });
  const ends =
    state.remainingMs === null
      ? null
      : state.endingSoon
        ? t(($) => $.machine_pause.banner.ends_soon, {
            duration: duration(state.remainingMs),
            machine: machine.title,
          })
        : t(($) => $.machine_pause.banner.ends_in, { duration: duration(state.remainingMs) });
  const Icon = safe ? Check : AlertTriangle;

  return (
    <div
      role="status"
      className={cn(
        "flex items-center gap-2.5 rounded-lg border px-3 py-2 text-caption",
        safe
          ? "border-success/40 bg-success/10"
          : "border-warning/60 bg-warning/15",
      )}
    >
      <Icon
        aria-hidden="true"
        className={cn("size-4 shrink-0", safe ? "text-success" : "text-warning")}
      />
      <span className="min-w-0 flex-1">
        <strong className="font-semibold">
          {t(($) => $.machine_pause.banner.paused, {
            machine: machine.title,
            reason: reasonName(state.reason ?? "maintenance"),
          })}
        </strong>{" "}
        {progress} {ends}
      </span>
      <Button
        type="button"
        size="xs"
        variant="outline"
        disabled={!target || resume.isPending}
        aria-busy={resume.isPending}
        onClick={() => {
          if (!target) return;
          resume.mutate(target.id, {
            onError: () => toast.error(t(($) => $.machine_pause.toast.resume_failed)),
          });
        }}
      >
        {resume.isPending && <Loader2 aria-hidden="true" className="size-3 animate-spin" />}
        {t(($) => $.machine_pause.resume)}
      </Button>
    </div>
  );
}

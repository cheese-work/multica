"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { useDismissedMachineEventsStore } from "@multica/core/runtimes";
import { affectedMachinesOptions, runtimeListOptions } from "@multica/core/runtimes/queries";
import { memberListOptions } from "@multica/core/workspace/queries";
import type { AgentRuntime, RuntimePauseReason } from "@multica/core/types";
import {
  deriveMachinePauseState,
  pauseTarget,
  type MachinePauseState,
} from "../runtimes/components/machine-pause-state";
import { buildRuntimeMachines } from "../runtimes/components/runtime-machines";

const TICK_MS = 30_000;

/** Id of the banner element; the header icon moves focus here when it reopens the banner. */
export const MACHINE_BANNER_ID = "machine-banner";

export interface BannerMachine {
  /** `owner_id:daemon_id`, stable across events of one machine. */
  key: string;
  /** Changes with each new pause or offline period; a dismissal is per event id. */
  eventId: string;
  title: string;
  /** Name of the machine owner when the viewer is not it; null for the viewer's own machine. */
  ownerName: string | null;
  state: "paused" | "offline";
  reason: RuntimePauseReason | null;
  remainingMs: number | null;
  endingSoon: boolean;
  /** Tasks left to finish (viewer's own machine) or queued tasks that wait (everyone else). */
  tasks: number;
  /** Set for the viewer's own paused machine: the drain progress the L1 banner shows. */
  own: MachinePauseState | null;
  lastSeenAt: string | null;
  /** The runtime Resume addresses, or null when the viewer holds no pause rights. */
  target: AgentRuntime | null;
}

/** Soonest expiry first; offline machines, which have no expiry, last. */
function compareBannerMachines(a: BannerMachine, b: BannerMachine): number {
  const rank = (m: BannerMachine) => (m.remainingMs === null ? Infinity : m.remainingMs);
  return rank(a) - rank(b);
}

/**
 * What the machine banner and the header icon both read: the machines that
 * affect the viewer, whether the banner is open, and how to close and reopen it.
 *
 * The viewer's own paused machines come from the runtime list (they carry the
 * drain progress and show even when no task waits). Every other machine comes
 * from the server's affected query. While that query loads or fails, only the
 * viewer's own machines show: a banner with a guessed cause is worse than none.
 */
export function useMachineBanner() {
  const wsId = useWorkspaceId();
  const currentUserId = useAuthStore((state) => state.user?.id);
  const { data: runtimes = [] } = useQuery(runtimeListOptions(wsId));
  const { data: affected } = useQuery(affectedMachinesOptions(wsId));
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const dismissed = useDismissedMachineEventsStore((state) => state.dismissed);
  const dismiss = useDismissedMachineEventsStore((state) => state.dismiss);
  const reopenEvents = useDismissedMachineEventsStore((state) => state.reopen);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => clearInterval(id);
  }, []);

  const role = members.find((member) => member.user_id === currentUserId)?.role;

  const machines = useMemo(() => {
    if (!currentUserId) return [];
    const built = buildRuntimeMachines(runtimes, { now, currentUserId });
    const ownerName = (ownerId: string) =>
      members.find((member) => member.user_id === ownerId)?.name ?? null;
    const rows: BannerMachine[] = [];
    const seen = new Set<string>();

    for (const machine of built) {
      if (!machine.runtimes.some((runtime) => runtime.owner_id === currentUserId)) continue;
      const state = deriveMachinePauseState(machine.runtimes, {
        now,
        online: machine.onlineCount > 0,
        fallbackTasks: 0,
      });
      if (state.phase === "running" || !state.pause) continue;
      const key = `${currentUserId}:${machine.daemonId ?? machine.id}`;
      seen.add(key);
      rows.push({
        key,
        eventId: `own:${key}:${state.pause.paused_at}`,
        title: machine.title,
        ownerName: null,
        state: "paused",
        reason: state.reason,
        remainingMs: state.remainingMs,
        endingSoon: state.endingSoon,
        tasks: state.tasks,
        own: state,
        lastSeenAt: machine.lastSeenAt,
        target: pauseTarget(machine.runtimes, currentUserId, role),
      });
    }

    for (const item of affected ?? []) {
      const key = `${item.machine_owner_id}:${item.daemon_id}`;
      if (seen.has(key)) continue;
      seen.add(key);
      const machine = built.find(
        (candidate) =>
          candidate.daemonId === item.daemon_id &&
          candidate.runtimes.some(
            (runtime) => (runtime.owner_id ?? runtime.workspace_id) === item.machine_owner_id,
          ),
      );
      const expiresAt = item.expires_at ? Date.parse(item.expires_at) : NaN;
      const remainingMs = Number.isNaN(expiresAt) ? null : expiresAt - now;
      if (item.state === "paused" && remainingMs !== null && remainingMs <= 0) continue;
      rows.push({
        key,
        eventId: item.event_id,
        title: item.device_name || machine?.title || item.daemon_id,
        ownerName: ownerName(item.machine_owner_id),
        state: item.state,
        reason: item.reason,
        remainingMs,
        endingSoon: remainingMs !== null && remainingMs < 5 * 60_000,
        tasks: item.waiting_tasks,
        own: null,
        lastSeenAt: machine?.lastSeenAt ?? null,
        target: machine ? pauseTarget(machine.runtimes, currentUserId, role) : null,
      });
    }
    return rows.sort(compareBannerMachines);
  }, [runtimes, affected, members, now, currentUserId, role]);

  const eventIds = useMemo(() => machines.map((machine) => machine.eventId), [machines]);
  // The banner is open while any current event is undismissed; a new event
  // therefore reopens it with every current row.
  const open = machines.some((machine) => !dismissed.includes(machine.eventId));
  const closeBanner = useCallback(() => dismiss(eventIds), [dismiss, eventIds]);
  const reopenBanner = useCallback(() => reopenEvents(eventIds), [reopenEvents, eventIds]);

  return { machines, open, dismissed, now, closeBanner, reopenBanner };
}

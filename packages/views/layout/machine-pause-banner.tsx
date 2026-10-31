"use client";

import { AlertTriangle, Check, ChevronDown, Loader2, X } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWSEvent } from "@multica/core/realtime";
import { useMachineBannerDetailsStore, useResumeRuntimeMachine } from "@multica/core/runtimes";
import { runtimeKeys } from "@multica/core/runtimes/queries";
import { Button } from "@multica/ui/components/ui/button";
import { cn } from "@multica/ui/lib/utils";
import { useT, useTimeAgo } from "../i18n";
import { useMachinePauseFormat } from "../runtimes/components/use-machine-pause-format";
import {
  MACHINE_BANNER_ID,
  useMachineBanner,
  type BannerMachine,
} from "./use-machine-banner";

/**
 * Warns, on every page, about machines that are paused or offline and cost the
 * viewer something. The daemon owner sees their own paused machine; the
 * workspace owner and project leads see a machine only while a task of theirs
 * waits for it. One banner at most; the close control hides it until a new
 * event, and the header icon brings it back.
 */
export function MachinePauseBanner() {
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const { machines, open, dismissed, closeBanner } = useMachineBanner();
  useWSEvent("daemon:register", () => {
    qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
  });

  if (machines.length === 0 || !open) return null;
  // After a dismiss, only the events the viewer has not seen get the tag.
  const hasSeenSome = machines.some((machine) => dismissed.includes(machine.eventId));
  return (
    <div className="flex shrink-0 flex-col gap-2 px-3 pt-3">
      {machines.length === 1 ? (
        <MachineBanner
          machine={machines[0]!}
          wsId={wsId}
          onClose={closeBanner}
          isNew={false}
        />
      ) : (
        <SummaryBanner
          machines={machines}
          wsId={wsId}
          onClose={closeBanner}
          isNew={(machine) => hasSeenSome && !dismissed.includes(machine.eventId)}
        />
      )}
    </div>
  );
}

function MachineBanner({
  machine,
  wsId,
  onClose,
  isNew,
}: {
  machine: BannerMachine;
  wsId: string;
  onClose: () => void;
  isNew: boolean;
}) {
  const { t } = useT("runtimes");
  const safe = machine.own?.phase === "drained" && machine.own.online;
  const Icon = safe ? Check : AlertTriangle;
  return (
    <div
      id={MACHINE_BANNER_ID}
      tabIndex={-1}
      role="status"
      className={cn(
        "flex items-center gap-2.5 rounded-lg border px-3 py-2 text-caption outline-none",
        safe ? "border-success/40 bg-success/10" : "border-warning/60 bg-warning/15",
      )}
    >
      <Icon
        aria-hidden="true"
        className={cn("size-4 shrink-0", safe ? "text-success" : "text-warning")}
      />
      <MachineLine machine={machine} isNew={isNew} className="min-w-0 flex-1" />
      <ResumeButton machine={machine} wsId={wsId} />
      <CloseButton onClose={onClose} label={t(($) => $.machine_pause.banner.close)} />
    </div>
  );
}

function SummaryBanner({
  machines,
  wsId,
  onClose,
  isNew,
}: {
  machines: BannerMachine[];
  wsId: string;
  onClose: () => void;
  isNew: (machine: BannerMachine) => boolean;
}) {
  const { t } = useT("runtimes");
  const { duration } = useMachinePauseFormat();
  const detailsOpen = useMachineBannerDetailsStore((state) => state.open);
  const setDetailsOpen = useMachineBannerDetailsStore((state) => state.setOpen);
  const tasks = machines.reduce((sum, machine) => sum + machine.tasks, 0);
  const next = machines.find((machine) => machine.remainingMs !== null);
  return (
    <div
      id={MACHINE_BANNER_ID}
      tabIndex={-1}
      role="status"
      className="flex flex-col gap-2 rounded-lg border border-warning/60 bg-warning/15 px-3 py-2 text-caption outline-none"
    >
      <div className="flex items-center gap-2.5">
        <AlertTriangle aria-hidden="true" className="size-4 shrink-0 text-warning" />
        <span className="min-w-0 flex-1">
          <strong className="font-semibold">
            {t(($) => $.machine_pause.banner.summary, { count: machines.length })}
          </strong>{" "}
          {t(($) => $.machine_pause.banner.tasks_wait_total, { count: tasks })}
          {next?.remainingMs != null &&
            ` ${t(($) => $.machine_pause.banner.next_to_end, {
              machine: next.title,
              duration: duration(next.remainingMs),
            })}`}
        </span>
        <Button
          type="button"
          size="xs"
          variant="ghost"
          aria-expanded={detailsOpen}
          onClick={() => setDetailsOpen(!detailsOpen)}
        >
          {t(($) => $.machine_pause.banner.details)}
          <ChevronDown
            aria-hidden="true"
            className={cn("size-3 transition-transform", detailsOpen && "rotate-180")}
          />
        </Button>
        <CloseButton onClose={onClose} label={t(($) => $.machine_pause.banner.close)} />
      </div>
      {detailsOpen && (
        <ul className="flex flex-col gap-1.5 border-t border-warning/40 pt-2">
          {machines.map((machine) => (
            <li key={machine.key} className="flex items-center gap-2.5">
              <MachineLine
                machine={machine}
                isNew={isNew(machine)}
                className="min-w-0 flex-1"
              />
              <ResumeButton machine={machine} wsId={wsId} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** One machine in words: what happened to it, how many tasks, and until when. */
function MachineLine({
  machine,
  isNew,
  className,
}: {
  machine: BannerMachine;
  isNew: boolean;
  className?: string;
}) {
  const { t } = useT("runtimes");
  const timeAgo = useTimeAgo();
  const { reasonName, duration } = useMachinePauseFormat();
  const name = machine.ownerName
    ? t(($) => $.machine_pause.banner.machine_of, {
        machine: machine.title,
        owner: machine.ownerName,
      })
    : machine.title;
  const own = machine.own;
  const safe = own?.phase === "drained" && own.online;

  const head =
    machine.state === "offline"
      ? t(($) => $.machine_pause.banner.offline, { machine: name })
      : t(($) => $.machine_pause.banner.paused, {
          machine: name,
          reason: reasonName(machine.reason ?? "maintenance"),
        });
  // The owner's own machine keeps the drain wording; everyone else reads how
  // many of their tasks wait.
  const progress = own
    ? safe
      ? t(($) => $.machine_pause.banner.drained)
      : own.online
        ? t(($) => $.machine_pause.banner.tasks_left, { count: machine.tasks })
        : t(($) => $.machine_pause.banner.tasks_left_stale, { count: machine.tasks })
    : t(($) => $.machine_pause.banner.tasks_wait, { count: machine.tasks });
  const ends =
    machine.remainingMs === null
      ? null
      : machine.endingSoon
        ? t(($) => $.machine_pause.banner.ends_soon, {
            duration: duration(machine.remainingMs),
            machine: machine.title,
          })
        : t(($) => $.machine_pause.banner.ends_in, { duration: duration(machine.remainingMs) });
  const lastSeen =
    machine.state === "offline" && machine.lastSeenAt
      ? t(($) => $.machine_pause.banner.last_seen, { time: timeAgo(machine.lastSeenAt) })
      : null;
  return (
    <span className={className}>
      <strong className="font-semibold">{head}</strong>{" "}
      {isNew && (
        <span className="mr-1 rounded bg-warning/30 px-1 text-[0.6875rem] font-semibold">
          {t(($) => $.machine_pause.banner.new_event)}
        </span>
      )}
      {progress} {ends} {lastSeen}
      {machine.state === "paused" && !machine.target && ` ${t(($) => $.machine_pause.no_rights)}`}
    </span>
  );
}

function ResumeButton({ machine, wsId }: { machine: BannerMachine; wsId: string }) {
  const { t } = useT("runtimes");
  const resume = useResumeRuntimeMachine(wsId);
  if (machine.state !== "paused" || !machine.target) return null;
  const target = machine.target;
  return (
    <Button
      type="button"
      size="xs"
      variant="outline"
      disabled={resume.isPending}
      aria-busy={resume.isPending}
      onClick={() =>
        resume.mutate(target.id, {
          onError: () => toast.error(t(($) => $.machine_pause.toast.resume_failed)),
        })
      }
    >
      {resume.isPending && <Loader2 aria-hidden="true" className="size-3 animate-spin" />}
      {t(($) => $.machine_pause.resume)}
    </Button>
  );
}

function CloseButton({ onClose, label }: { onClose: () => void; label: string }) {
  return (
    <Button type="button" size="icon-xs" variant="ghost" aria-label={label} onClick={onClose}>
      <X aria-hidden="true" className="size-3.5" />
    </Button>
  );
}

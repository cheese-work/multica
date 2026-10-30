"use client";

import { Clock } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@multica/ui/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { chatWakeupsOptions } from "@multica/core/chat/queries";
import { useCancelChatWakeup } from "@multica/core/chat/mutations";
import { useT } from "../../i18n";
import { formatDateTimeWithZone } from "./message-time";

const firstLine = (text: string) => text.trim().split("\n", 1)[0] ?? "";

/**
 * "Scheduled N" in the DM header: the one-shot follow-ups the agent set with
 * `multica chat wakeup`, each cancellable. Hidden while nothing is pending.
 * Times show the viewer-local date plus the fired row's `HH:MM ZONE`;
 * `timeZone` exists so tests can pin it.
 */
export function ScheduledWakeupsChip({
  sessionId,
  agentName,
  timeZone,
}: {
  sessionId: string;
  agentName?: string | null;
  timeZone?: string;
}) {
  const { t } = useT("chat");
  const { data, isError, refetch } = useQuery(chatWakeupsOptions(sessionId));
  const cancel = useCancelChatWakeup();
  // A failed first load is not "nothing scheduled": keep a way to retry.
  if (!data && isError) {
    return (
      <Button
        variant="outline"
        size="sm"
        className="shrink-0 gap-1.5 text-muted-foreground"
        onClick={() => void refetch()}
      >
        <Clock className="h-3.5 w-3.5" />
        {t(($) => $.scheduled.load_failed)}
      </Button>
    );
  }
  const wakeups = data ?? [];
  if (wakeups.length === 0) return null;

  return (
    <Popover>
      <PopoverTrigger
        render={
          <Button
            variant="outline"
            size="sm"
            className="shrink-0 gap-1.5 text-muted-foreground"
            data-testid="scheduled-chip"
          />
        }
      >
        <Clock className="h-3.5 w-3.5" />
        {t(($) => $.scheduled.chip, { count: wakeups.length })}
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80">
        <div className="text-caption font-medium text-muted-foreground">
          {agentName
            ? t(($) => $.scheduled.title, { name: agentName })
            : t(($) => $.scheduled.title_unknown_agent)}
        </div>
        <ul className="flex flex-col gap-2">
          {wakeups.map((wakeup) => {
            const time = formatDateTimeWithZone(wakeup.fire_at, undefined, timeZone);
            return (
              <li key={wakeup.id} className="flex items-start gap-2" data-testid="scheduled-row">
                <div className="min-w-0 flex-1">
                  <div className="text-caption tabular-nums text-muted-foreground">{time}</div>
                  <div className="truncate text-body">{firstLine(wakeup.instruction)}</div>
                </div>
                <Button
                  variant="ghost"
                  size="sm"
                  data-testid="scheduled-cancel"
                  disabled={cancel.isPending && cancel.variables?.wakeupId === wakeup.id}
                  aria-label={t(($) => $.scheduled.cancel_label, { time })}
                  onClick={() =>
                    cancel.mutate(
                      { sessionId, wakeupId: wakeup.id },
                      { onError: () => toast.error(t(($) => $.scheduled.cancel_failed)) },
                    )
                  }
                >
                  {t(($) => $.scheduled.cancel)}
                </Button>
              </li>
            );
          })}
        </ul>
      </PopoverContent>
    </Popover>
  );
}

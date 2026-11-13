"use client";

import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { WifiOff } from "@multica/ui/icons";
import { useT } from "../../i18n";

/** Skeleton rows the height of a real row, so nothing jumps when data lands. */
export function ListLoading() {
  const { t } = useT("chat");
  return (
    <div role="status" aria-label={t(($) => $.list.states.loading)} className="flex flex-col gap-1">
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className="flex h-14 items-center gap-3 px-2" aria-hidden="true">
          <Skeleton className="size-8 shrink-0 rounded-full" />
          <div className="flex min-w-0 flex-1 flex-col gap-1.5">
            <Skeleton className="h-3.5 w-2/3" />
            <Skeleton className="h-3 w-1/2" />
          </div>
        </div>
      ))}
    </div>
  );
}

export function ListEmpty({ kind }: { kind: "none" | "view" | "waiting" }) {
  const { t } = useT("chat");
  return (
    <div className="flex flex-col items-center gap-1 px-4 py-10 text-center">
      <p className="text-body font-medium">
        {kind === "none"
          ? t(($) => $.list.states.empty_title)
          : kind === "waiting"
            ? t(($) => $.list.states.empty_waiting)
            : t(($) => $.list.states.empty_view)}
      </p>
      {kind !== "view" && (
        <p className="text-caption text-muted-foreground">
          {kind === "none" ? t(($) => $.list.states.empty_body) : t(($) => $.list.states.empty_waiting_body)}
        </p>
      )}
    </div>
  );
}

export function ListError({ onRetry }: { onRetry?: () => void }) {
  const { t } = useT("chat");
  return (
    <div role="alert" className="flex flex-col items-center gap-2 px-4 py-10 text-center">
      <p className="text-body font-medium">{t(($) => $.list.states.error_title)}</p>
      <p className="text-caption text-muted-foreground">{t(($) => $.list.states.error_body)}</p>
      {onRetry && (
        <button
          type="button"
          onClick={onRetry}
          className="mt-1 inline-flex h-9 items-center rounded-md border border-border px-3 text-caption font-medium outline-none hover:bg-accent/50 focus-visible:ring-2 focus-visible:ring-ring md:h-7"
        >
          {t(($) => $.list.states.retry)}
        </button>
      )}
    </div>
  );
}

export function ListOffline() {
  const { t } = useT("chat");
  return (
    <div
      role="status"
      aria-label={t(($) => $.list.states.offline_title)}
      className="mx-2 mt-2 flex items-start gap-2 rounded-md bg-muted px-2.5 py-2 text-caption text-muted-foreground"
    >
      <WifiOff className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
      <span>
        <span className="block font-medium text-foreground">{t(($) => $.list.states.offline_title)}</span>
        {t(($) => $.list.states.offline_body)}
      </span>
    </div>
  );
}

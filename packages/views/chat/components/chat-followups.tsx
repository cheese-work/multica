"use client";

import { AlertCircle, Clock, WifiOff } from "lucide-react";
import type { ChatWakeup } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { cn } from "@multica/ui/lib/utils";
import { useLocale, useT } from "../../i18n";
import { CHAT_COLUMN, CHAT_GUTTER } from "./chat-column";
import { dayKey } from "./message-time";

export interface ChatFollowUpsProps {
  /** The open DM. A follow-up whose `chat_session_id` differs was set in an earlier DM. */
  sessionId: string;
  agentName?: string;
  wakeups: ChatWakeup[];
  status: "loading" | "error" | "ready";
  /** Defaults to the reader's zone; the params exist so tests can pin them. */
  timeZone?: string;
  now?: Date;
  /** The follow-up whose cancel request is in flight. */
  cancellingId?: string | null;
  /** The follow-up whose last cancel request failed; it stays listed. */
  cancelErrorId?: string | null;
  offline?: boolean;
  onCancel: (wakeupId: string) => void;
  onRetry: () => void;
}

// Indochina Time has no short name in most ICU locales (it prints "GMT+7").
// The team reads these times as ICT, so name the zone the way they do.
const ICT_ZONES = new Set(["Asia/Ho_Chi_Minh", "Asia/Saigon", "Asia/Bangkok", "Asia/Phnom_Penh", "Asia/Vientiane"]);

function zoneLabel(parts: Intl.DateTimeFormatPart[], timeZone: string): string {
  if (ICT_ZONES.has(timeZone)) return "ICT";
  return parts.find((p) => p.type === "timeZoneName")?.value ?? "";
}

// "11:40 ICT", or "Oct 22, 11:40 ICT" when the day is not today in that zone.
function formatAt(iso: string, locale: string, timeZoneProp: string | undefined, now: Date): string {
  const timeZone = timeZoneProp ?? new Intl.DateTimeFormat().resolvedOptions().timeZone;
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return iso;
  const sameDay = dayKey(iso, timeZone) === dayKey(now.toISOString(), timeZone);
  const options: Intl.DateTimeFormatOptions = {
    ...(sameDay ? {} : { month: "short", day: "numeric" }),
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
    timeZoneName: "short",
  };
  let parts: Intl.DateTimeFormatPart[];
  try {
    parts = new Intl.DateTimeFormat(locale, { ...options, timeZone }).formatToParts(at);
  } catch {
    // A zone this runtime's ICU lacks → the reader's local zone.
    parts = new Intl.DateTimeFormat(locale, options).formatToParts(at);
  }
  const label = zoneLabel(parts, timeZone);
  const text = parts
    .filter((p) => p.type !== "timeZoneName")
    .map((p) => p.value)
    .join("")
    .trim()
    .replace(/[,\s]+$/, "");
  return label ? `${text} ${label}` : text;
}

export function ChatFollowUps({
  sessionId,
  agentName,
  wakeups,
  status,
  timeZone,
  now,
  cancellingId = null,
  cancelErrorId = null,
  offline = false,
  onCancel,
  onRetry,
}: ChatFollowUpsProps) {
  const { t } = useT("chat");
  const locale = useLocale();

  if (status === "ready" && wakeups.length === 0) return null;

  const name = agentName?.trim() || t(($) => $.followups.fallback_name);
  const clock = now ?? new Date();

  const conditionText = (wakeup: ChatWakeup): string | null => {
    const condition = wakeup.condition;
    if (!condition) return null;
    const identifier = condition.identifier || t(($) => $.followups.cond_unknown_issue);
    const check = condition.check;
    if (check?.type === "issue_field") {
      const value = String(check.value ?? "").replace(/_/g, " ");
      return check.field === "status"
        ? t(($) => $.followups.cond_issue_field, { identifier, value })
        : t(($) => $.followups.cond_issue_other_field, { identifier, value, field: check.field ?? "" });
    }
    if (check?.type === "pull_request") {
      if (check.event === "checks_finished") return t(($) => $.followups.cond_checks_finished, { identifier });
      if (check.event === "merged") return t(($) => $.followups.cond_merged, { identifier });
      return t(($) => $.followups.cond_pull_request, { identifier });
    }
    if (condition.comment_by) return t(($) => $.followups.cond_comment, { identifier });
    return t(($) => $.followups.cond_generic, { identifier });
  };

  const summarize = (wakeup: ChatWakeup): string => {
    const time = formatAt(wakeup.fire_at, locale, timeZone, clock);
    const condition = conditionText(wakeup);
    return condition
      ? t(($) => $.followups.at_condition, { name, condition, time })
      : t(($) => $.followups.at_time, { name, time });
  };

  const shown = status === "ready" ? wakeups.length : 0;
  const regionLabel =
    status === "ready"
      ? t(($) => $.followups.title, { count: shown })
      : t(($) => $.followups.label);

  return (
    <div
      data-slot="chat-followups-shell"
      data-testid="chat-followups"
      // Tucked behind the composer exactly like the queue: the negative margin
      // slides the bottom edge under the opaque composer surface.
      className={cn(CHAT_GUTTER, "relative z-0 -mb-3")}
    >
      <div className={CHAT_COLUMN}>
        <section
          data-slot="chat-followups"
          aria-label={regionLabel}
          aria-live="polite"
          aria-busy={status === "loading"}
          className={cn(
            "mx-3 overflow-hidden rounded-lg border border-surface-border bg-surface pb-4",
            "animate-in fade-in slide-in-from-bottom-2 duration-300",
          )}
        >
          <div className="px-3.5 pt-1.5 text-caption font-medium text-muted-foreground">
            {t(($) => $.followups.label)}
          </div>
          {status === "loading" ? (
            <div
              aria-busy="true"
              data-slot="chat-followup-loading"
              className="px-3.5 py-2 text-caption text-faint-foreground"
            >
              {t(($) => $.followups.loading)}
            </div>
          ) : null}
          {status === "error" ? (
            <div
              role="alert"
              data-testid="chat-followups-load-error"
              className="flex items-center gap-2 px-3.5 py-1.5 text-caption text-destructive"
            >
              <AlertCircle className="size-3.5 shrink-0" aria-hidden="true" />
              <span className="min-w-0 flex-1">{t(($) => $.followups.load_failed, { name })}</span>
              <Button variant="ghost" size="xs" onClick={onRetry}>
                {t(($) => $.followups.retry)}
              </Button>
            </div>
          ) : null}
          {status === "ready" ? (
            <div
              data-slot="chat-followup-list"
              // Three rows of 56px, then it scrolls.
              className="max-h-[168px] overflow-y-auto px-2 py-1"
            >
              {wakeups.map((wakeup) => {
                const summary = summarize(wakeup);
                const cancelling = cancellingId === wakeup.id;
                return (
                  <div key={wakeup.id}>
                    <div
                      data-slot="chat-followup-row"
                      data-testid="chat-followup-row"
                      data-wakeup={wakeup.id}
                      className="flex min-h-12 min-w-0 items-center gap-2 rounded-md px-1.5 py-1 text-caption"
                    >
                      <Clock className="size-3.5 shrink-0 text-faint-foreground" aria-hidden="true" />
                      <div className="flex min-w-0 flex-1 flex-col">
                        <span className="line-clamp-2 text-foreground">{summary}</span>
                        <span className="line-clamp-2 text-muted-foreground">
                          {wakeup.chat_session_id !== sessionId ? (
                            <>
                              <span>{t(($) => $.followups.inherited)}</span>{" "}
                            </>
                          ) : null}
                          <span>{t(($) => $.followups.will, { name })}</span>{" "}
                          <span>{wakeup.instruction}</span>
                        </span>
                      </div>
                      <Button
                        variant="ghost"
                        size="xs"
                        data-testid="chat-followup-cancel"
                        className="min-h-8 shrink-0 px-2 font-normal text-muted-foreground"
                        disabled={cancelling || offline}
                        aria-busy={cancelling}
                        aria-label={t(($) => $.followups.cancel_label, { summary })}
                        onClick={() => onCancel(wakeup.id)}
                      >
                        {cancelling ? t(($) => $.followups.cancelling) : t(($) => $.followups.cancel)}
                      </Button>
                    </div>
                    {cancelErrorId === wakeup.id ? (
                      <div
                        role="alert"
                        data-testid="chat-followup-cancel-error"
                        className="mx-1.5 mb-1 flex items-center gap-2 rounded-md bg-destructive/10 px-2 py-1 text-caption text-destructive"
                      >
                        <AlertCircle className="size-3.5 shrink-0" aria-hidden="true" />
                        <span className="min-w-0 flex-1">{t(($) => $.followups.cancel_failed)}</span>
                        <Button
                          variant="ghost"
                          size="xs"
                          className="min-h-8"
                          disabled={offline}
                          onClick={() => onCancel(wakeup.id)}
                        >
                          {t(($) => $.followups.retry)}
                        </Button>
                      </div>
                    ) : null}
                  </div>
                );
              })}
            </div>
          ) : null}
          {offline && status === "ready" ? (
            <div
              role="status"
              data-testid="chat-followups-offline"
              className="mx-3.5 mt-0.5 flex items-center gap-2 text-caption text-muted-foreground"
            >
              <WifiOff className="size-3.5 shrink-0 text-warning" aria-hidden="true" />
              <span>{t(($) => $.followups.offline)}</span>
            </div>
          ) : null}
        </section>
      </div>
    </div>
  );
}

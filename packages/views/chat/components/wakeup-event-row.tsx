"use client";

import { Fragment, useId, useState, type ReactNode } from "react";
import { ChevronDown, ChevronUp, Clock } from "lucide-react";
import { cn } from "@multica/ui/lib/utils";
import { useWorkspacePaths } from "@multica/core/paths";
import type { ChatScheduledWakeup } from "@multica/core/types";
import { useActorName } from "@multica/core/workspace/hooks";
import { ActorAvatar } from "../../common/actor-avatar";
import { AppLink } from "../../navigation";
import { useT } from "../../i18n";
import { formatTimeWithZone } from "./message-time";

// `[CHE-1](mention://issue/<uuid>)` markdown links, or a bare identifier.
const ISSUE_REF = /\[([^\]]*)\]\(mention:\/\/issue\/([0-9a-fA-F-]{36})\)|\b[A-Z][A-Z0-9]{1,9}-\d+\b/g;
const IDENTIFIER = /^[A-Z][A-Z0-9]{1,9}-\d+$/;

const CHIP_CLASS =
  "inline-flex items-center rounded-md border border-border bg-background px-1.5 text-caption leading-5 text-foreground hover:bg-accent focus-visible:outline-2 focus-visible:outline-ring";

/**
 * The instruction verbatim, with the first reference to each distinct issue as
 * a compact identifier-only chip. A mention and a bare identifier for the same
 * issue count once; later references stay plain label text, never a UUID.
 */
function InstructionText({ text }: { text: string }) {
  const { t } = useT("chat");
  const paths = useWorkspacePaths();
  const seen = new Set<string>();
  const parts: ReactNode[] = [];
  let last = 0;
  for (const match of text.matchAll(ISSUE_REF)) {
    const [raw, mentionLabel, mentionId] = match;
    const label = mentionId
      ? mentionLabel?.trim() || t(($) => $.wakeup.issue_fallback)
      : raw;
    // Identity: the identifier when the label is one (joins mention + bare
    // forms), else the mention's UUID.
    const key = IDENTIFIER.test(label) ? label : (mentionId ?? label);
    parts.push(text.slice(last, match.index));
    last = match.index + raw.length;
    if (seen.has(key) || (mentionId && seen.has(mentionId))) {
      parts.push(label);
      continue;
    }
    seen.add(key);
    if (mentionId) seen.add(mentionId);
    parts.push(
      <AppLink key={match.index} href={paths.issueDetail(mentionId ?? raw)} className={CHIP_CLASS}>
        {label}
      </AppLink>,
    );
  }
  parts.push(text.slice(last));
  return (
    <>
      {parts.map((part, i) => (
        <Fragment key={i}>{part}</Fragment>
      ))}
    </>
  );
}

/**
 * A fired chat wakeup as an event on the DM thread (CHE-2459, direction C):
 * a clock node on a left rail, one title line, and the instruction behind a
 * toggle. The model-facing header is never part of `wakeup.instruction`.
 * `timeZone` exists so tests can pin the reader's zone.
 */
export function WakeupEventRow({
  wakeup,
  timeZone,
}: {
  wakeup: ChatScheduledWakeup;
  timeZone?: string;
}) {
  const { t } = useT("chat");
  const { getActorName } = useActorName();
  const regionId = useId();
  const [open, setOpen] = useState(false);

  const firstLine = wakeup.instruction.split("\n").find((line) => line.trim())?.trim() ?? "";
  const title = wakeup.title.trim();
  const Chevron = open ? ChevronUp : ChevronDown;

  return (
    <div className="flex gap-2.5">
      <div className="-mb-2 flex w-6 shrink-0 flex-col items-center">
        <div
          role="img"
          aria-label={t(($) => $.wakeup.node_label)}
          className="flex size-6 items-center justify-center rounded-full border border-warning/30 bg-warning/10 text-warning"
        >
          <Clock className="size-3.5" aria-hidden />
        </div>
        <span aria-hidden className="mt-0.5 min-h-2.5 w-0.5 flex-1 bg-warning/30" />
      </div>
      <div className="min-w-0 flex-1 pb-3">
        <div className="flex min-h-6 flex-wrap items-center gap-x-2">
          <time
            dateTime={wakeup.fire_at}
            className="text-body font-semibold text-[color-mix(in_oklab,var(--warning)_55%,var(--foreground))]"
          >
            {t(($) => $.wakeup.scheduled_at, {
              time: formatTimeWithZone(wakeup.fire_at, undefined, timeZone),
            })}
          </time>
          <time dateTime={wakeup.created_at} className="text-label text-muted-foreground">
            {t(($) => $.wakeup.set_by, {
              agent: getActorName("agent", wakeup.agent_id),
              time: formatTimeWithZone(wakeup.created_at, undefined, timeZone),
            })}
          </time>
        </div>
        <div
          data-testid="wakeup-title"
          className={cn(
            "text-body",
            title
              ? "break-words font-semibold"
              : "truncate font-medium text-muted-foreground",
          )}
        >
          {title || firstLine}
        </div>
        <button
          type="button"
          aria-expanded={open}
          aria-controls={regionId}
          onClick={() => setOpen((v) => !v)}
          className="mt-[3px] inline-flex items-center gap-1 rounded-sm text-label text-foreground underline underline-offset-[3px] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring max-md:min-h-11"
        >
          {open ? t(($) => $.wakeup.hide_instruction) : t(($) => $.wakeup.show_instruction)}
          <Chevron className="size-3" aria-hidden />
        </button>
        {open && (
          <div
            id={regionId}
            role="region"
            aria-label={t(($) => $.wakeup.instruction_region)}
            className="mt-1.5 rounded-md border border-border bg-muted/40 px-3 py-2.5 text-label leading-7 text-foreground"
          >
            <small className="mb-1 block text-caption leading-4 text-muted-foreground">
              {t(($) => $.wakeup.instruction_note)}
            </small>
            <div className="whitespace-pre-wrap break-words">
              <InstructionText text={wakeup.instruction} />
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

/**
 * The thread rail carried into the replying agent: a short rail segment, then
 * the agent avatar, then the reply (or the running-task pill) beside it.
 */
export function WakeupReplyRail({
  agentId,
  children,
}: {
  agentId: string;
  children: ReactNode;
}) {
  return (
    <div data-testid="wakeup-reply" className="flex gap-2.5">
      <div className="-mt-2 flex w-6 shrink-0 flex-col items-center">
        <span aria-hidden className="h-3 w-0.5 bg-warning/30" />
        <ActorAvatar actorType="agent" actorId={agentId} size="md" profileLink={false} />
      </div>
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  );
}

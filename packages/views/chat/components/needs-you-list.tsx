"use client";

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { toast } from "sonner";
import {
  useChatAttentionLater,
  useChatAttentionUndoXongAll,
  useChatAttentionXong,
  useChatAttentionXongAll,
} from "@multica/core/chat/attention-mutations";
import { snoozableKeys } from "@multica/core/chat/attention";
import { createSafeId } from "@multica/core/utils";
import { Button } from "@multica/ui/components/ui/button";
import type { Agent, ChatSession, ChatStaleLevel } from "@multica/core/types";
import { AppLink } from "../../navigation";
import { useLocale, useT } from "../../i18n";
import { formatMessageDateTime } from "./message-time";
import { useAttentionRefresh } from "./use-attention-refresh";

/** Rows shown before "Show all"; the header reads "5 / N". */
const VISIBLE_ROWS = 5;
const LEVEL_RANK: Record<string, number> = { red: 0, yellow: 1, fresh: 2 };

type AttentionLevel = Exclude<ChatStaleLevel, "none">;

function isAttention(s: ChatSession): s is ChatSession & { stale_level: AttentionLevel } {
  return s.stale_level !== undefined && s.stale_level in LEVEL_RANK;
}

/** Red, yellow, fresh; then oldest start; then id (plan r4). */
function compareAttention(a: ChatSession, b: ChatSession): number {
  const level = LEVEL_RANK[a.stale_level ?? ""]! - LEVEL_RANK[b.stale_level ?? ""]!;
  if (level !== 0) return level;
  const since = Date.parse(a.attention_since ?? "") - Date.parse(b.attention_since ?? "");
  if (since !== 0 && Number.isFinite(since)) return since;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

/** One plain line for a list row: no markdown, no mention links, no parameter dump. */
export function decisionHeadline(ask: string, issueIdentifier: string): string {
  const proposal = ask.match(/^\*\*Proposed action\*\*\s*\n`([^`]+)`\s+version\s+(\d+)/);
  if (proposal) return `${proposal[1]} v${proposal[2]} on ${issueIdentifier}`;
  return (ask.match(/\*\*Decision\*\*\s*\n([\s\S]*?)(?:\n\s*\n|$)/)?.[1] ?? ask)
    .replace(/\[([^\]]*)\]\(mention:\/\/[^)]*\)/g, "$1")
    .replace(/[#*`>~]/g, "").replace(/\s+/g, " ").trim();
}

export function NeedsYouList({
  sessions,
  agents,
  refresh = true,
}: {
  sessions: ChatSession[];
  agents: Agent[];
  /** Off when the parent already runs `useAttentionRefresh`. */
  refresh?: boolean;
}) {
  const slug = useWorkspaceSlug();
  const locale = useLocale();
  const { t } = useT("chat");
  const xong = useChatAttentionXong();
  const later = useChatAttentionLater();
  const xongAll = useChatAttentionXongAll();
  const undoXongAll = useChatAttentionUndoXongAll();
  const [now, setNow] = useState(Date.now);
  const [showAll, setShowAll] = useState(false);
  const [groupOpen, setGroupOpen] = useState(false);
  const listRef = useRef<HTMLUListElement>(null);

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(timer);
  }, []);

  const rows = useMemo(() => sessions.filter(isAttention).sort(compareAttention), [sessions]);
  const unclassified = useMemo(
    () => sessions.filter((s) => (s.attention_unclassified_keys?.length ?? 0) > 0),
    [sessions],
  );
  const unclassifiedTotal = unclassified.reduce((n, s) => n + (s.attention_unclassified_keys?.length ?? 0), 0);

  useAttentionRefresh(sessions, refresh);

  // A background re-order moves keyed rows in the DOM, which can drop focus.
  // Put it back on the same session's row.
  const focusedId = useRef<string | null>(null);
  useLayoutEffect(() => {
    const list = listRef.current;
    if (!list) return;
    const id = focusedId.current;
    if (id && !list.contains(document.activeElement)) {
      list.querySelector<HTMLElement>(`[data-session-id="${CSS.escape(id)}"]`)?.focus();
    }
  });

  if (!slug) return null;

  const onActionError = () => toast.error(t(($) => $.needs_you.actions.failed));
  const keysOf = (s: ChatSession) => s.attention_keys ?? [];
  const allKeys = rows.flatMap(keysOf);
  const markAllDone = () =>
    xongAll.mutate(allKeys, {
      onError: onActionError,
      onSuccess: (batch) =>
        toast(t(($) => $.needs_you.actions.toast, { count: rows.length }), {
          duration: 10_000,
          ...(batch.batch_id
            ? {
                action: {
                  label: t(($) => $.needs_you.actions.undo),
                  onClick: () => undoXongAll.mutate(batch.batch_id, { onError: onActionError }),
                },
              }
            : {}),
        }),
    });

  const agentName = (s: ChatSession) => agents.find((a) => a.id === s.agent_id)?.name ?? "";
  const rowHref = (s: ChatSession) => `${paths.workspace(slug).chat()}?${new URLSearchParams({ session: s.id })}`;
  const titleOf = (s: ChatSession) => s.title || t(($) => $.session_history.untitled);
  const ageOf = (since: string | null | undefined) => {
    const minutes = Math.max(0, Math.floor((now - Date.parse(since ?? "")) / 60_000));
    if (!Number.isFinite(minutes)) return "";
    if (minutes < 60) return t(($) => $.needs_you.age.minutes, { count: minutes });
    if (minutes < 2880) return t(($) => $.needs_you.age.hours, { count: Math.floor(minutes / 60) });
    return t(($) => $.needs_you.age.days, { count: Math.floor(minutes / 1440) });
  };

  const shown = showAll ? rows : rows.slice(0, VISIBLE_ROWS);
  const count = rows.length > VISIBLE_ROWS
    ? t(($) => $.needs_you.count, { shown: shown.length, total: rows.length })
    : String(rows.length);

  return (
    <section aria-label={t(($) => $.needs_you.title)} className="pb-2">
      <h3 className="flex items-center justify-between px-2 py-2 text-caption font-semibold">
        <span>
          {t(($) => $.needs_you.title)}
          {" "}
          <span className="tabular-nums text-muted-foreground">{count}</span>
        </span>
        {allKeys.length > 0 && (
          <Button type="button" variant="ghost" size="xs" disabled={xongAll.isPending} onClick={markAllDone}>
            {t(($) => $.needs_you.actions.done_all)}
          </Button>
        )}
      </h3>
      <div>
        <ul
          ref={listRef}
          onFocus={(e) => {
            focusedId.current = (e.target as HTMLElement).closest("[data-session-id]")?.getAttribute("data-session-id") ?? null;
          }}
          onBlur={(e) => {
            // A blur to nowhere is the DOM move; a blur to another element is the user.
            if (e.relatedTarget) focusedId.current = null;
          }}
        >
          {shown.map((s) => {
            const level = s.stale_level as AttentionLevel;
            const kind = t(($) => $.needs_you.kind[s.attention_source ?? "ask"]);
            const levelLabel = t(($) => $.needs_you.level[level]);
            const age = ageOf(s.attention_since);
            return (
              <li key={s.id} data-testid={`attention-row-container-${s.id}`}>
                <AppLink
                  href={rowHref(s)}
                  data-testid="attention-row"
                  data-session-id={s.id}
                  aria-label={t(($) => $.needs_you.row_aria, { kind, title: titleOf(s), level: levelLabel, age })}
                  className="flex min-h-11 flex-col gap-1 rounded-md px-2 py-2 text-caption outline-none hover:bg-accent/50 focus-visible:ring-1 focus-visible:ring-ring"
                >
                  <span className="flex items-center gap-2 text-muted-foreground">
                    <span className="font-medium text-foreground">{kind}</span>
                    <span className="min-w-0 flex-1 truncate">{agentName(s)}</span>
                    <time
                      dateTime={s.attention_since ?? undefined}
                      title={s.attention_since ? formatMessageDateTime(s.attention_since, locale) : undefined}
                      className={level === "red" ? "shrink-0 font-medium text-destructive" : level === "yellow" ? "shrink-0 font-medium text-warning" : "shrink-0"}
                    >
                      {t(($) => $.needs_you.level_age, { level: levelLabel, age })}
                    </time>
                  </span>
                  <span className="line-clamp-2 font-medium">{titleOf(s)}</span>
                </AppLink>
                {keysOf(s).length > 0 && (
                  <div className="flex gap-1 px-2 pb-1">
                    {snoozableKeys(keysOf(s)).length > 0 && (
                      <Button
                        type="button"
                        variant="ghost"
                        size="xs"
                        aria-label={t(($) => $.needs_you.actions.done_aria, { title: titleOf(s) })}
                        disabled={xong.isPending}
                        onClick={() => xong.mutate({ sessionId: s.id, keys: keysOf(s) }, { onError: onActionError })}
                      >
                        {t(($) => $.needs_you.actions.done)}
                      </Button>
                    )}
                    <Button
                      type="button"
                      variant="ghost"
                      size="xs"
                      aria-label={t(($) => $.needs_you.actions.later_aria, { title: titleOf(s) })}
                      disabled={later.isPending}
                      onClick={() =>
                        later.mutate(
                          { sessionId: s.id, keys: keysOf(s), requestId: createSafeId() },
                          { onError: onActionError },
                        )
                      }
                    >
                      {t(($) => $.needs_you.actions.later)}
                    </Button>
                  </div>
                )}
              </li>
            );
          })}
        </ul>
        {rows.length > VISIBLE_ROWS && (
          <button
            type="button"
            onClick={() => setShowAll((v) => !v)}
            className="min-h-9 px-2 text-caption underline focus-visible:outline-2 focus-visible:outline-ring"
          >
            {showAll ? t(($) => $.needs_you.show_less) : t(($) => $.needs_you.show_all, { count: rows.length })}
          </button>
        )}
        {unclassified.length > 0 && (
          <div>
            <button
              type="button"
              aria-expanded={groupOpen}
              onClick={() => setGroupOpen((v) => !v)}
              className="flex min-h-9 w-full items-center justify-between px-2 text-caption text-muted-foreground focus-visible:outline-2 focus-visible:outline-ring"
            >
              {t(($) => $.needs_you.unclassified.title)}
              <span className="tabular-nums">{unclassifiedTotal}</span>
            </button>
            {groupOpen && (
              <ul>
                {unclassified.map((s) => (
                  <li key={s.id}>
                    <AppLink
                      href={rowHref(s)}
                      data-testid="unclassified-row"
                      data-subject-keys={s.attention_unclassified_keys?.join(" ")}
                      className="flex min-h-11 flex-col gap-1 rounded-md px-2 py-2 text-caption outline-none hover:bg-accent/50 focus-visible:ring-1 focus-visible:ring-ring"
                    >
                      <span className="flex items-center gap-2 text-muted-foreground">
                        <span className="min-w-0 flex-1 truncate">{agentName(s)}</span>
                        <span className="shrink-0">
                          {t(($) => $.needs_you.unclassified.replies, { count: s.attention_unclassified_keys?.length ?? 0 })}
                        </span>
                      </span>
                      <span className="line-clamp-2 font-medium">{titleOf(s)}</span>
                    </AppLink>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </div>
    </section>
  );
}

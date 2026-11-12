"use client";

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { toast } from "sonner";
import { chatKeys, openDecisionRequestsOptions } from "@multica/core/chat/queries";
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

/** Rows shown before "Show all"; the header reads "5 / N". */
const VISIBLE_ROWS = 5;
/** Wait past the server's instant so the refetch lands after the change. */
const REFRESH_SLACK_MS = 500;
/** Floor for a refetch timer, so a stale next_change_at cannot spin. */
const REFRESH_MIN_MS = 5_000;
/** setTimeout overflows past 2^31-1 ms; the refetch is harmless if early. */
const REFRESH_MAX_MS = 2_147_483_647;
/** Refetches per next_change_at value when the server keeps returning it. */
const REFRESH_MAX_RETRIES = 5;

const LEVEL_RANK: Record<string, number> = { red: 0, yellow: 1, fresh: 2 };

type AttentionLevel = Exclude<ChatStaleLevel, "none">;

function requestAge(createdAt: string, now: number, locale: string): string {
  const elapsed = now - Date.parse(createdAt);
  if (!Number.isFinite(elapsed)) return createdAt;
  const minutes = Math.max(0, Math.floor(elapsed / 60_000));
  const formatter = new Intl.RelativeTimeFormat(locale, { numeric: "always" });
  if (minutes < 60) return formatter.format(-minutes, "minute");
  if (minutes < 1440) return formatter.format(-Math.floor(minutes / 60), "hour");
  return formatter.format(-Math.floor(minutes / 1440), "day");
}

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
function decisionHeadline(ask: string, issueIdentifier: string): string {
  const proposal = ask.match(/^\*\*Proposed action\*\*\s*\n`([^`]+)`\s+version\s+(\d+)/);
  if (proposal) return `${proposal[1]} v${proposal[2]} on ${issueIdentifier}`;
  return (ask.match(/\*\*Decision\*\*\s*\n([\s\S]*?)(?:\n\s*\n|$)/)?.[1] ?? ask)
    .replace(/\[([^\]]*)\]\(mention:\/\/[^)]*\)/g, "$1")
    .replace(/[#*`>~]/g, "").replace(/\s+/g, " ").trim();
}

export function NeedsYouList({ sessions, agents }: { sessions: ChatSession[]; agents: Agent[] }) {
  const wsId = useWorkspaceId();
  const slug = useWorkspaceSlug();
  const locale = useLocale();
  const { t } = useT("chat");
  const qc = useQueryClient();
  const query = useQuery(openDecisionRequestsOptions(wsId));
  const xong = useChatAttentionXong();
  const later = useChatAttentionLater();
  const xongAll = useChatAttentionXongAll();
  const undoXongAll = useChatAttentionUndoXongAll();
  const requests = query.data ?? [];
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

  // The list query never goes stale on its own (staleTime: Infinity), so the
  // projection is refreshed when the server says it changes, and whenever the
  // window can have missed time passing. WS reconnects are handled in realtime.
  const nextChange = useMemo(
    () => Math.min(...sessions.map((s) => Date.parse(s.next_change_at ?? "")).filter(Number.isFinite)),
    [sessions],
  );
  // A refetch that returns the same next_change_at (server clock behind, failed
  // fetch, early timeout) would not re-run the effect; `attempt` re-arms it.
  const [attempt, setAttempt] = useState(0);
  const retries = useRef({ for: nextChange, n: 0 });
  useEffect(() => {
    const refresh = () => void qc.invalidateQueries({ queryKey: chatKeys.sessions(wsId) });
    const onVisible = () => {
      if (document.visibilityState === "visible") refresh();
    };
    window.addEventListener("focus", refresh);
    window.addEventListener("online", refresh);
    document.addEventListener("visibilitychange", onVisible);
    const delay = Math.min(Math.max(nextChange - Date.now() + REFRESH_SLACK_MS, REFRESH_MIN_MS), REFRESH_MAX_MS);
    if (retries.current.for !== nextChange) retries.current = { for: nextChange, n: 0 };
    const timer =
      Number.isFinite(nextChange) && retries.current.n < REFRESH_MAX_RETRIES
        ? setTimeout(() => {
            retries.current.n += 1;
            refresh();
            setAttempt((a) => a + 1);
          }, delay)
        : undefined;
    return () => {
      window.removeEventListener("focus", refresh);
      window.removeEventListener("online", refresh);
      document.removeEventListener("visibilitychange", onVisible);
      clearTimeout(timer);
    };
  }, [qc, wsId, nextChange, attempt]);

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
    <section aria-label={t(($) => $.needs_you.title)} className="sticky top-0 z-10 border-b border-surface-border bg-background pb-2">
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
      <div className="max-h-64 overflow-y-auto">
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
      <h4 className="flex items-center justify-between px-2 pt-2 text-caption font-semibold text-muted-foreground">
        {t(($) => $.needs_you.decisions)}
        {" "}
        <span className="tabular-nums">{query.data ? requests.length : "—"}</span>
      </h4>
      {query.isPending && <p role="status" className="px-2 text-caption text-muted-foreground">{t(($) => $.needs_you.loading)}</p>}
      {query.isError && (
        <div role="alert" className="px-2 text-caption text-destructive">
          {t(($) => $.needs_you.error)}
          <button type="button" onClick={() => void query.refetch()} className="ml-2 min-h-9 underline focus-visible:outline-2 focus-visible:outline-ring">
            {t(($) => $.needs_you.retry)}
          </button>
        </div>
      )}
      {!query.isPending && !query.isError && requests.length === 0 && (
        <p className="px-2 text-caption text-muted-foreground">{t(($) => $.needs_you.empty)}</p>
      )}
      <ul className="max-h-64 overflow-y-auto">
        {requests.map((request) => {
          const headline = decisionHeadline(request.ask, request.issue_identifier);
          const params = new URLSearchParams({ session: request.chat_session_id });
          if (request.message_id) params.set("message", request.message_id);
          return (
            <li key={request.id}>
              <AppLink href={`${paths.workspace(slug).chat()}?${params}`} className="flex min-h-11 flex-col gap-1 rounded-md px-2 py-2 text-caption outline-none hover:bg-accent/50 focus-visible:ring-1 focus-visible:ring-ring">
                <span className="flex items-center gap-2 text-muted-foreground">
                  <span className="font-medium text-foreground">{request.issue_identifier}</span>
                  <span className="min-w-0 flex-1 truncate">{request.asking_agent_name || request.asking_agent_id}</span>
                  <time dateTime={request.created_at} title={formatMessageDateTime(request.created_at, locale)} className="shrink-0">
                    {requestAge(request.created_at, now, locale)}
                  </time>
                </span>
                <span className="line-clamp-2 font-medium">{headline}</span>
                <span className="text-muted-foreground">{request.escalation_class.replaceAll("_", " ")}</span>
              </AppLink>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

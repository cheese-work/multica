"use client";

import { useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from "react";
import { matchMutation, useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { Archive, ArchiveRestore, Loader2, Pin, PinOff, Search, X } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import { useWorkspaceId } from "@multica/core/hooks";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { chatKeys } from "@multica/core/chat/queries";
import {
  CHAT_SEARCH_MAX_QUERY_LENGTH,
  chatSearchFailure,
  chatSearchInfiniteOptions,
  isChatSearchQueryTooLong,
  splitHighlight,
  useChatSearchStore,
  type ChatSearchScope,
} from "@multica/core/chat/search";
import { getShortcut, isEditableShortcutTarget, shortcutMatchesEvent } from "@multica/core/shortcuts";
import type { Agent, ChatSearchHit, ChatSession } from "@multica/core/types";
import { isImeComposing } from "@multica/core/utils";
import { ActorAvatar } from "../../common/actor-avatar";
import { useLocale, useT } from "../../i18n";
import { useOptionalNavigation } from "../../navigation";
import { formatChatTime } from "./format-chat-time";

const SEARCH_DEBOUNCE_MS = 250;

interface ChatSearchProps {
  scope: ChatSearchScope;
  sessions: ChatSession[];
  agents: Agent[];
  activeSessionId: string | null;
  /** Open the hit's chat; a message hit also carries the message to scroll to. */
  onOpenHit: (hit: ChatSearchHit) => void;
  onUnarchive: (session: ChatSession) => void;
  onTogglePin: (session: ChatSession) => void;
  onArchive: (session: ChatSession) => void;
  /** Bind the registry's bare-key shortcut to this field (one field per page). */
  bindShortcut?: boolean;
  /** The normal list, shown while the field is empty. */
  children: ReactNode;
}

/**
 * Search field above the DM list. A query replaces the list with a labelled
 * list of hits in place; Escape or Clear drops the query and brings the list
 * back. The query lives in `useChatSearchStore`, so the field and its cached
 * results survive a remount and the history Back from an opened hit.
 */
export function ChatSearch({ scope, bindShortcut, children, ...results }: ChatSearchProps) {
  const { t } = useT("chat");
  const query = useChatSearchStore((s) => s.queries[scope] ?? "");
  const setQuery = useChatSearchStore((s) => s.setQuery);
  const clear = useChatSearchStore((s) => s.clear);
  const inputRef = useRef<HTMLInputElement>(null);
  const trimmed = query.trim();
  const tooLong = isChatSearchQueryTooLong(query);
  // One request per pause in typing, not one per keystroke.
  const [settled, setSettled] = useState(trimmed);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(trimmed), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [trimmed]);

  useEffect(() => {
    if (!bindShortcut) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.repeat || isImeComposing(event)) return;
      if (isEditableShortcutTarget(event.target)) return;
      if (!shortcutMatchesEvent(getShortcut("searchChat"), event)) return;
      event.preventDefault();
      inputRef.current?.focus();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [bindShortcut]);

  // Escape anywhere in the field or the results drops the query. The popover
  // and dialogs close on Escape; with a query typed it only drops the query,
  // and a second press does the usual thing.
  const onSurfaceKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key !== "Escape" || !query || e.defaultPrevented) return;
    e.preventDefault();
    e.stopPropagation();
    clear(scope);
    inputRef.current?.focus();
  };

  return (
    <div className="contents" onKeyDown={onSurfaceKeyDown}>
      <div role="search" className="sticky top-0 z-10 bg-background px-1 pb-1">
        <div className="relative flex items-center">
          <Search aria-hidden="true" className="pointer-events-none absolute left-2 size-3.5 text-muted-foreground" />
          <input
            ref={inputRef}
            type="search"
            role="searchbox"
            aria-label={t(($) => $.search.label)}
            placeholder={t(($) => $.search.placeholder)}
            value={query}
            onChange={(e) => setQuery(scope, e.target.value)}
            // 16 px at phone width keeps iOS from zooming the page on focus.
            className="h-8 w-full appearance-none rounded-md border border-input bg-transparent pl-7 pr-14 text-base outline-none placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring md:text-caption [&::-webkit-search-cancel-button]:hidden"
          />
          {query ? (
            <button
              type="button"
              aria-label={t(($) => $.search.clear)}
              onClick={() => {
                clear(scope);
                inputRef.current?.focus();
              }}
              className="absolute right-1.5 inline-flex size-6 items-center justify-center rounded-xs text-muted-foreground hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
            >
              <X className="size-3.5" />
            </button>
          ) : bindShortcut ? (
            <kbd
              aria-hidden="true"
              className="absolute right-2 rounded-xs border border-border px-1 text-micro text-muted-foreground"
            >
              {t(($) => $.search.shortcut_hint)}
            </kbd>
          ) : null}
        </div>
      </div>
      {!trimmed ? (
        children
      ) : tooLong ? (
        <p role="alert" className="px-2 py-1.5 text-caption text-muted-foreground">
          {t(($) => $.search.too_long, { max: CHAT_SEARCH_MAX_QUERY_LENGTH })}
        </p>
      ) : settled !== trimmed ? (
        <SearchingStatus />
      ) : (
        <ChatSearchResults query={trimmed} {...results} />
      )}
    </div>
  );
}

function SearchingStatus() {
  const { t } = useT("chat");
  return (
    <p role="status" className="flex items-center gap-2 px-2 py-1.5 text-caption text-muted-foreground">
      <Loader2 className="size-3.5 animate-spin" />
      {t(($) => $.search.searching)}
    </p>
  );
}

function ChatSearchResults({
  query,
  sessions,
  agents,
  activeSessionId,
  onOpenHit,
  onUnarchive,
  onTogglePin,
  onArchive,
}: Omit<ChatSearchProps, "scope" | "bindShortcut" | "children"> & { query: string }) {
  const { t } = useT("chat");
  const wsId = useWorkspaceId();
  const search = useInfiniteQuery(chatSearchInfiniteOptions(wsId, query));
  const queryClient = useQueryClient();
  const sessionById = useMemo(() => new Map(sessions.map((s) => [s.id, s])), [sessions]);
  const agentById = useMemo(() => new Map(agents.map((a) => [a.id, a])), [agents]);

  // Title hits first, then message hits; the server order holds inside each.
  const hits = useMemo(() => {
    const all = search.data?.pages.flatMap((page) => page.results) ?? [];
    return [...all.filter((h) => !h.message_id), ...all.filter((h) => h.message_id)];
  }, [search.data]);

  // A pin or archive changes a hit's chat. Refetch once the mutation has
  // settled, success or failure, so the rows show what the server now holds.
  useEffect(() => {
    const filters = { mutationKey: chatKeys.sessionAction(wsId) };
    return queryClient.getMutationCache().subscribe((event) => {
      if (event.type !== "updated" || !matchMutation(filters, event.mutation)) return;
      const { status } = event.mutation.state;
      if (status !== "success" && status !== "error") return;
      void queryClient.invalidateQueries({ queryKey: [...chatKeys.all(wsId), "search"] });
    });
  }, [queryClient, wsId]);

  // Offline, TanStack pauses the request: it neither runs nor fails.
  const pausedOffline = search.isPending && search.fetchStatus === "paused";
  if (search.isPending && !pausedOffline) return <SearchingStatus />;
  if (search.isError || pausedOffline) {
    const failure = pausedOffline ? "disconnected" : chatSearchFailure(search.error);
    return (
      <div role="alert" className="flex flex-col items-start gap-1.5 px-2 py-1.5 text-caption text-muted-foreground">
        <p>{t(($) => $.search[failure === "failed" ? "failed" : failure === "timed_out" ? "timed_out" : "disconnected"])}</p>
        <button
          type="button"
          onClick={() => void search.refetch()}
          className="rounded-xs px-1.5 py-0.5 font-medium text-foreground hover:bg-accent focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
        >
          {t(($) => $.search.retry)}
        </button>
      </div>
    );
  }
  if (hits.length === 0) {
    return (
      <p role="status" className="px-2 py-1.5 text-caption text-muted-foreground">
        {t(($) => $.search.empty, { query })}
      </p>
    );
  }

  return (
    <>
      <p role="status" className="px-2 py-1 text-micro text-muted-foreground">
        {t(($) => $.search.count, { count: hits.length })}
      </p>
      <ul aria-label={t(($) => $.search.results_label)} className="m-0 flex list-none flex-col p-0">
        {hits.map((hit) => (
          <ChatSearchRow
            key={`${hit.session_id}:${hit.message_id ?? "title"}`}
            hit={hit}
            query={query}
            agent={agentById.get(hit.agent_id) ?? null}
            session={sessionById.get(hit.session_id) ?? null}
            isCurrent={hit.session_id === activeSessionId}
            onOpenHit={onOpenHit}
            onUnarchive={onUnarchive}
            onTogglePin={onTogglePin}
            onArchive={onArchive}
          />
        ))}
      </ul>
      {search.hasNextPage && (
        <button
          type="button"
          disabled={search.isFetchingNextPage}
          onClick={() => void search.fetchNextPage()}
          className="mx-2 my-1 rounded-md px-2 py-1.5 text-left text-caption font-medium text-muted-foreground hover:bg-accent/50 hover:text-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:opacity-60"
        >
          {search.isFetchingNextPage ? t(($) => $.search.loading_more) : t(($) => $.search.show_more)}
        </button>
      )}
    </>
  );
}

function ChatSearchRow({
  hit,
  query,
  agent,
  session,
  isCurrent,
  onOpenHit,
  onUnarchive,
  onTogglePin,
  onArchive,
}: {
  hit: ChatSearchHit;
  query: string;
  agent: Agent | null;
  session: ChatSession | null;
  isCurrent: boolean;
  onOpenHit: (hit: ChatSearchHit) => void;
  onUnarchive: (session: ChatSession) => void;
  onTogglePin: (session: ChatSession) => void;
  onArchive: (session: ChatSession) => void;
}) {
  const { t } = useT("chat");
  const locale = useLocale();
  const slug = useWorkspaceSlug();
  const openInNewTab = useOptionalNavigation()?.openInNewTab;
  const title = hit.title.trim() || t(($) => $.window.untitled);
  const href = `${slug ? paths.workspace(slug).chat() : ""}?session=${hit.session_id}${
    hit.message_id ? `&message=${hit.message_id}` : ""
  }`;
  const parts = splitHighlight(hit.snippet, query);

  const actions = !session
    ? []
    : hit.archived
      ? [{ key: "unarchive", label: t(($) => $.list.unarchive), icon: <ArchiveRestore className="size-3.5" />, run: () => onUnarchive(session) }]
      : [
          {
            key: "pin",
            label: session.pinned ? t(($) => $.list.unpin) : t(($) => $.list.pin),
            icon: session.pinned ? <PinOff className="size-3.5" /> : <Pin className="size-3.5 -rotate-45" />,
            run: () => onTogglePin(session),
          },
          { key: "archive", label: t(($) => $.list.archive), icon: <Archive className="size-3.5" />, run: () => onArchive(session) },
        ];

  return (
    <li className={cn("group/hit relative flex min-w-0 items-start gap-1 rounded-md pr-1", isCurrent ? "bg-accent" : "hover:bg-accent/50")}>
      <a
        href={href}
        aria-current={isCurrent ? "true" : undefined}
        onClick={(e) => {
          // On the web a modified click is the browser's (a real background tab);
          // the desktop shell has no second chat tab, so it keeps the selection.
          const modified = e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0;
          if (modified && !openInNewTab) return;
          e.preventDefault();
          onOpenHit(hit);
        }}
        className="flex min-w-0 flex-1 gap-3 rounded-md px-2 py-2 outline-none focus-visible:ring-1 focus-visible:ring-ring"
      >
        {agent ? (
          <ActorAvatar actorType="agent" actorId={agent.id} size="lg" className="ring-1 ring-inset ring-border" />
        ) : (
          <span className="size-8 shrink-0" />
        )}
        <span className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span className="flex items-center gap-1.5">
            <span className="min-w-0 flex-1 truncate text-body font-medium">{title}</span>
            <span className="shrink-0 text-micro text-muted-foreground">{formatChatTime(hit.updated_at, locale)}</span>
          </span>
          <span className="flex items-center gap-1.5 text-caption text-muted-foreground">
            {agent && <span className="max-w-[50%] truncate font-medium">{agent.name}</span>}
            {hit.archived && (
              <span className="shrink-0 rounded-xs border border-border px-1 text-micro">{t(($) => $.search.archived_badge)}</span>
            )}
            {!hit.message_id && <span className="shrink-0 text-micro">{t(($) => $.search.title_match)}</span>}
          </span>
          {hit.message_id && (
            <span className="line-clamp-1 text-caption text-muted-foreground">
              {parts.map((part, i) =>
                part.match ? (
                  <mark key={i} className="rounded-xs bg-brand/20 font-medium text-foreground">
                    {part.text}
                  </mark>
                ) : (
                  <span key={i}>{part.text}</span>
                ),
              )}
            </span>
          )}
        </span>
      </a>
      {actions.length > 0 && (
        <div
          role="group"
          aria-label={t(($) => $.search.actions_aria, { title })}
          className="flex shrink-0 items-center gap-0.5 pt-2"
        >
          {actions.map((action) => (
            <button
              key={action.key}
              type="button"
              aria-label={action.label}
              onClick={action.run}
              className="inline-flex size-7 items-center justify-center rounded-xs text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:bg-accent focus-visible:text-foreground focus-visible:outline-none"
            >
              {action.icon}
            </button>
          ))}
        </div>
      )}
    </li>
  );
}

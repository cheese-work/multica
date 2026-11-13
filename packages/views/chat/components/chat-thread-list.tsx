"use client";

import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Archive,
  ArchiveRestore,
  Clock,
  EyeOff,
  ListChecks,
  Loader2,
  Pencil,
  Pin,
  PinOff,
  Square,
  Trash2,
} from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import { useWorkspaceId } from "@multica/core/hooks";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { useWorkspacePresenceMap } from "@multica/core/agents";
import { api } from "@multica/core/api";
import {
  pendingChatTasksOptions,
  chatKeys,
  filterChatSessions,
  openDecisionRequestsOptions,
  sessionActivityTime,
  sortArchivedChatSessions,
  sortChatSessions,
} from "@multica/core/chat/queries";
import {
  useDeleteChatSession,
  useSetChatSessionPinned,
  useUpdateChatSession,
} from "@multica/core/chat/mutations";
import { useChatStore } from "@multica/core/chat";
import type { Agent, ChatSearchHit, ChatSession, PendingChatTasksResponse } from "@multica/core/types";
import { ActorAvatar } from "../../common/actor-avatar";
import {
  RowActionsMenu,
  RowContextMenu,
  handleRowActivationKey,
  type RowActionItem,
} from "../../common/row-actions-menu";
import { resolveClickIntent, useOptionalNavigation } from "../../navigation";
import { createLogger } from "@multica/core/logger";
import { removeChatMessageFromCaches } from "@multica/core/realtime";
import { useLocale, useT } from "../../i18n";
import { useChatSessionActions } from "./use-chat-session-actions";
import { handleRowShortcut } from "./use-row-shortcuts";
import { SessionRenameInput } from "./session-rename-input";
import { ChatSearch } from "./chat-search";
import { formatChatClock } from "./format-chat-time";
import { groupByDay, type DayGroupKey } from "../lib/group-by-day";
import { NeedsYouList } from "./needs-you-list";
import { useAttentionRefresh } from "./use-attention-refresh";
import { ChatFilterChips, type ChatFilterCounts } from "./chat-filter-chips";
import { ListEmpty, ListError, ListLoading, ListOffline } from "./chat-thread-list-states";
import { useOnline } from "../../settings/components/use-online";

const apiLogger = createLogger("chat.api");

// Collapse a (possibly markdown / multi-line) message into a one-line preview.
function toPreview(content: string): string {
  return content
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/[#*`>~]/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

/**
 * IM-style conversation list: each row is agent avatar + session title + agent
 * name + last-message preview + time, with a red unread *count* badge. An
 * in-flight agent shows a "typing…" indicator; a failed last reply shows a
 * destructive hint. Rows are rendered in the server's order (most-recent
 * activity first). Renaming lives in the conversation header's ⋯ menu, not
 * here.
 *
 * Two views, toggled locally: the default "history" view lists active chats and
 * hovering a row reveals pin + archive (or stop, while running) — archiving is
 * the reversible, one-click default so nothing is destroyed by accident. A
 * footer entry ("Archived · N") switches to the "archived" view, which lists
 * archived chats and is the ONLY place a chat can be hard-deleted (hover →
 * unarchive + delete). Both views read the same flat sessions cache and split
 * on `status` locally.
 */
export function ChatThreadList(props: ChatThreadListProps) {
  const { sessions, agents, activeSessionId, onSelectSession, onArchive, onOpenSearchHit } = props;
  const setPinned = useSetChatSessionPinned();
  const sessionActions = useChatSessionActions();
  return (
    <ChatSearch
      scope="page"
      bindShortcut
      sessions={sessions}
      agents={agents}
      activeSessionId={activeSessionId}
      onOpenHit={(hit) => {
        if (onOpenSearchHit) return onOpenSearchHit(hit);
        const session = sessions.find((s) => s.id === hit.session_id);
        if (session) onSelectSession(session);
      }}
      onUnarchive={(session) => void sessionActions.unarchive(session)}
      onTogglePin={(session) => setPinned.mutate({ sessionId: session.id, pinned: !session.pinned })}
      onArchive={onArchive}
    >
      <ChatThreadListBody {...props} />
    </ChatSearch>
  );
}

interface ChatThreadListProps {
  sessions: ChatSession[];
  agents: Agent[];
  activeSessionId: string | null;
  onSelectSession: (session: ChatSession) => void;
  // Archiving is owned by the parent so the selection advance stays layout-
  // aware (desktop advances to the next chat; mobile drops back to the list)
  // and routes through the shared controller — see ChatPage.handleArchive.
  onArchive: (session: ChatSession) => void;
  /** Opens a search hit; a message hit also lands on that message. Defaults to selecting the chat. */
  onOpenSearchHit?: (hit: ChatSearchHit) => void;
  /** First load of the list. Only shown while there are no rows to show. */
  loadState?: "loading" | "error";
  onRetry?: () => void;
  /**
   * Enters multi-select with this row. Seam for the select UI: while the host
   * does not pass it, the `X` shortcut and the "Select multiple" item are off.
   */
  onEnterSelectMode?: (session: ChatSession) => void;
}

function ChatThreadListBody({
  sessions,
  agents,
  activeSessionId,
  onSelectSession,
  onArchive,
  loadState,
  onRetry,
  onEnterSelectMode,
}: ChatThreadListProps) {
  const { t } = useT("chat");
  const locale = useLocale();
  const wsId = useWorkspaceId();
  // Null-safe slug (not useWorkspacePaths, which throws): the list renders in
  // tests outside a workspace route; without a slug the web modifier-click
  // affordance simply stays off.
  const slug = useWorkspaceSlug();
  const sessionHref = (sessionId: string) =>
    slug ? `${paths.workspace(slug).chat()}?session=${sessionId}` : null;
  // Optional: the list renders bare in tests; without an adapter the web
  // modifier-click affordance stays off (desktop keeps selection anyway).
  const navigation = useOptionalNavigation();
  const openInNewTab = navigation?.openInNewTab;
  const getShareableUrl = navigation?.getShareableUrl;
  const agentById = useMemo(() => new Map(agents.map((a) => [a.id, a])), [agents]);

  // One flat cache; the chip decides the slice. Every non-archived chip keeps
  // pinned-first order, Archived sorts by activity only.
  // A continued DM lives on in its successor's transcript, so only the newest
  // DM of a chain gets a row. A successor missing from the cache leaves its
  // source listed rather than lost.
  const rows = useMemo(() => {
    const ids = new Set(sessions.map((s) => s.id));
    return sessions.filter((s) => !s.continued_into_session_id || !ids.has(s.continued_into_session_id));
  }, [sessions]);

  const listView = useChatStore((s) => s.listView);
  const setListView = useChatStore((s) => s.setListView);
  const decisionsQuery = useQuery(openDecisionRequestsOptions(wsId));
  const decisionRequests = Array.isArray(decisionsQuery.data) ? decisionsQuery.data : undefined;
  const decisionSessionIds = useMemo(
    () => new Set((decisionRequests ?? []).map((r) => r.chat_session_id)),
    [decisionRequests],
  );
  const visibleSessions = useMemo(() => {
    const picked = filterChatSessions(rows, listView, decisionSessionIds);
    return [
      ...sortChatSessions(picked.filter((s) => s.status !== "archived")),
      ...sortArchivedChatSessions(picked.filter((s) => s.status === "archived")),
    ];
  }, [rows, listView, decisionSessionIds]);
  const counts = useMemo<ChatFilterCounts>(() => {
    if (loadState === "loading" && rows.length === 0) return {};
    const n = (view: Parameters<typeof filterChatSessions>[1]) =>
      filterChatSessions(rows, view, decisionSessionIds).length;
    return {
      ...(decisionRequests ? { decisions: n("decisions") } : {}),
      waiting: n("waiting"),
      unseen: n("unseen"),
      archived: n("archived"),
    };
  }, [rows, decisionSessionIds, decisionRequests, loadState]);
  const hasUnclassified = rows.some((s) => (s.attention_unclassified_keys?.length ?? 0) > 0);
  useAttentionRefresh(sessions);
  const online = useOnline();

  const [confirmingDeleteId, setConfirmingDeleteId] = useState<string | null>(null);
  const [confirmingStopId, setConfirmingStopId] = useState<string | null>(null);
  const [stoppingTaskId, setStoppingTaskId] = useState<string | null>(null);
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const deleteSession = useDeleteChatSession();
  const updateSession = useUpdateChatSession();
  const setPinned = useSetChatSessionPinned();
  const sessionActions = useChatSessionActions();
  const setActiveSession = useChatStore((s) => s.setActiveSession);
  const queryClient = useQueryClient();

  const { data: pending } = useQuery(pendingChatTasksOptions(wsId));
  const pendingTaskBySessionId = useMemo(
    () => new Map((pending?.tasks ?? []).map((task) => [task.chat_session_id, task])),
    [pending],
  );

  // Per-agent presence, so a pending task on an OFFLINE agent shows "waiting"
  // rather than a misleading "typing…" (the task is queued until the agent is
  // back). Same availability source the conversation pane's status pill uses,
  // keeping the two surfaces consistent.
  const presence = useWorkspacePresenceMap(wsId);

  useEffect(() => {
    if (!confirmingStopId || pendingTaskBySessionId.has(confirmingStopId)) return;
    setConfirmingStopId(null);
  }, [confirmingStopId, pendingTaskBySessionId]);

  const handleConfirmDelete = (session: ChatSession) => {
    const sessionId = session.id;
    if (activeSessionId === sessionId) setActiveSession(null);
    deleteSession.mutate(sessionId, {
      onSettled: () => setConfirmingDeleteId(null),
    });
  };

  const handleConfirmStop = (
    session: ChatSession,
    task: PendingChatTasksResponse["tasks"][number],
  ) => {
    setStoppingTaskId(task.task_id);
    queryClient.setQueryData<PendingChatTasksResponse>(chatKeys.pendingTasks(wsId), (current) => {
      if (!current) return current;
      return {
        ...current,
        tasks: current.tasks.filter((item) => item.task_id !== task.task_id),
      };
    });
    queryClient.setQueryData(chatKeys.pendingTask(session.id), {});
    queryClient.invalidateQueries({ queryKey: chatKeys.messages(session.id) });
    queryClient.invalidateQueries({ queryKey: chatKeys.messagesPage(session.id) });

    api.cancelTaskById(task.task_id).then(
      (result) => {
        const restored = result.cancelled_chat_message;
        if (restored?.restore_to_input) {
          removeChatMessageFromCaches(queryClient, restored.chat_session_id, restored.message_id);
        }
        apiLogger.info("cancelTask.success (list row)", { taskId: task.task_id, sessionId: session.id });
      },
      (err) =>
        apiLogger.warn("cancelTask.error (list row; task may have already finished)", {
          taskId: task.task_id,
          sessionId: session.id,
          err,
        }),
    ).finally(() => {
      queryClient.invalidateQueries({ queryKey: chatKeys.pendingTasks(wsId) });
      queryClient.invalidateQueries({ queryKey: chatKeys.pendingTask(session.id) });
      setStoppingTaskId(null);
      setConfirmingStopId(null);
    });
  };

  const handleRenameSubmit = (session: ChatSession, raw: string) => {
    setRenamingId(null);
    const title = raw.trim();
    if (!title || title === session.title) return;
    if (title.length > 200) {
      toast.error(t(($) => $.input.rename_title_too_long_toast));
      return;
    }
    updateSession.mutate(
      { sessionId: session.id, title },
      { onError: () => toast.error(t(($) => $.input.rename_failed_toast)) },
    );
  };

  const renderRow = (session: ChatSession) => {
    const isCurrent = session.id === activeSessionId;
    const agent = agentById.get(session.agent_id) ?? null;
    const agentName = agent?.name.trim() || null;
    const pendingTask = pendingTaskBySessionId.get(session.id);
    const isRunning = !!pendingTask;
    // Only "offline" (definitively long-offline) downgrades typing → waiting.
    // Unknown/loading presence keeps the optimistic "typing…" so we never
    // suppress it just because presence data hasn't landed yet.
    const agentOffline = agent
      ? presence.byAgent.get(agent.id)?.availability === "offline"
      : false;
    const unread = isCurrent ? 0 : (session.unread_count ?? 0);
    const isConfirmingDelete = confirmingDeleteId === session.id;
    const isConfirmingStop = confirmingStopId === session.id && !!pendingTask;
    const isConfirmingAction = isConfirmingDelete || isConfirmingStop;
    const titleText = session.title?.trim() || t(($) => $.window.untitled);
    const last = session.last_message ?? null;
    const timeText = formatChatClock(sessionActivityTime(session), locale);
    const unseen = !isCurrent && (session.has_unread || (session.unread_count ?? 0) > 0);
    // The active chip already says "Decisions" / "Unseen"; only day-grouped
    // views need the tag.
    const showDecisionTag =
      decisionSessionIds.has(session.id) && listView !== "decisions" && listView !== "unseen";

    // The second line: typing/waiting → failed → preview.
    let previewNode: React.ReactNode;
    if (isRunning && agentOffline) {
      // Task is queued but the agent is offline — it will run once the agent
      // is back. Show a static "waiting", not an animated "typing".
      previewNode = (
        <span className="flex min-w-0 items-center gap-1.5 text-muted-foreground">
          <Clock className="size-3 shrink-0" />
          <span className="truncate">{t(($) => $.list.waiting)}</span>
        </span>
      );
    } else if (isRunning) {
      previewNode = (
        <span className="flex min-w-0 items-center gap-1.5 text-emerald-500">
          <Loader2 className="size-3 shrink-0 animate-spin" />
          <span className="truncate">{t(($) => $.list.typing)}</span>
        </span>
      );
    } else if (last?.failure_reason) {
      previewNode = <span className="block truncate text-destructive">{t(($) => $.list.failed)}</span>;
    } else if (last?.message_kind === "no_response") {
      // A no_response turn stores a non-empty English fallback as its content,
      // so the preview is never blank even on older clients; new clients show a
      // localized, italic hint instead of that fallback text (MUL-4351).
      previewNode = (
        <span className="block truncate italic text-muted-foreground">
          {t(($) => $.list.no_response_preview)}
        </span>
      );
    } else if (last) {
      previewNode = (
        <span className={cn("block truncate", unseen ? "font-semibold text-foreground" : "text-muted-foreground")}>
          {last.role === "user" ? t(($) => $.list.you_prefix) : ""}
          {toPreview(last.content)}
        </span>
      );
    } else {
      previewNode = <span className="block truncate text-muted-foreground">{t(($) => $.list.no_messages)}</span>;
    }

    // One list drives every action surface (⋯ menu, right-click menu, row
    // shortcuts), so they cannot drift. The archived view is the only place
    // hard-delete lives; the history view offers the reversible archive.
    const renameItem: RowActionItem = {
      key: "rename",
      icon: <Pencil className="size-3.5" />,
      label: t(($) => $.list.rename),
      shortcut: "R",
      onSelect: () => setRenamingId(session.id),
    };
    const selectItem: RowActionItem[] = onEnterSelectMode
      ? [
          {
            key: "select",
            icon: <ListChecks className="size-3.5" />,
            label: t(($) => $.list.select),
            shortcut: "X",
            onSelect: () => onEnterSelectMode(session),
          },
        ]
      : [];
    const rowActions: RowActionItem[] =
      session.status === "archived"
        ? [
            {
              key: "unarchive",
              icon: <ArchiveRestore className="size-3.5" />,
              label: t(($) => $.list.unarchive),
              shortcut: "E",
              onSelect: () =>
                void sessionActions.unarchive(session),
            },
            {
              key: "delete",
              icon: <Trash2 className="size-3.5" />,
              label: t(($) => $.session_history.row_delete_aria),
              danger: true,
              onSelect: () => setConfirmingDeleteId(session.id),
            },
          ]
        : [
            {
              key: "unseen",
              icon: <EyeOff className="size-3.5" />,
              label: t(($) => $.list.unseen),
              shortcut: "U",
              onSelect: () =>
                void sessionActions.markUnseen(session, {
                  onMarked: () => {
                    if (isCurrent) setActiveSession(null);
                  },
                }),
            },
            {
              key: "pin",
              icon: session.pinned ? (
                <PinOff className="size-3.5" />
              ) : (
                <Pin className="size-3.5 -rotate-45" />
              ),
              label: session.pinned
                ? t(($) => $.list.unpin)
                : t(($) => $.list.pin),
              shortcut: "P",
              onSelect: () =>
                setPinned.mutate({ sessionId: session.id, pinned: !session.pinned }),
            },
            isRunning
              ? {
                  key: "stop",
                  icon: <Square className="size-3 fill-current" />,
                  label: t(($) => $.session_history.row_stop_aria),
                  danger: true,
                  onSelect: () => setConfirmingStopId(session.id),
                }
              : {
                  key: "archive",
                  icon: <Archive className="size-3.5" />,
                  label: t(($) => $.list.archive),
                  shortcut: "E",
                  onSelect: () => onArchive(session),
                },
          ];
    const menuGroups = [rowActions, [renameItem, ...selectItem]];
    const shortcutItems = menuGroups.flat();
    const isRenaming = renamingId === session.id;

    return (
      <RowContextMenu
        key={session.id}
        groups={isConfirmingAction ? [] : menuGroups}
        data-testid="chat-thread-row"
        aria-current={isCurrent ? "true" : undefined}
        tabIndex={0}
        onClick={(e) => {
          if (isConfirmingAction || e.defaultPrevented) return;
          // Plain click keeps the master-detail selection. On web, a modifier
          // click opens the session as its own browser tab. Desktop tabs
          // dedupe chat by pathname (a session is view state, not a subject —
          // see tab-store resourceKey), so a second chat tab cannot exist
          // there; modifier clicks keep the selection behavior instead.
          const href = sessionHref(session.id);
          if (
            href &&
            getShareableUrl &&
            !openInNewTab &&
            resolveClickIntent(e) !== "push"
          ) {
            window.open(
              getShareableUrl(href),
              "_blank",
              "noopener,noreferrer",
            );
            return;
          }
          onSelectSession(session);
        }}
        onAuxClick={(e) => {
          if (isConfirmingAction || e.defaultPrevented || e.button !== 1) return;
          if (openInNewTab) return; // desktop: no second chat tab exists
          const href = sessionHref(session.id);
          if (!href || !getShareableUrl) return;
          e.preventDefault();
          window.open(getShareableUrl(href), "_blank", "noopener,noreferrer");
        }}
        onKeyDown={(e) => {
          if (isConfirmingAction) return;
          handleRowActivationKey(e, () => onSelectSession(session));
          // A portal (the open ⋯ menu) still bubbles React events to the row;
          // only keys pressed inside the row's own DOM are row shortcuts.
          if (e.currentTarget.contains(e.target as Node)) handleRowShortcut(e, shortcutItems);
        }}
        className={cn(
          // Fixed height so nothing (hover actions, confirm prompts) can change
          // the row size and make the list jump. Content is vertically centered.
          "group/row relative flex h-14 min-w-0 cursor-default items-center gap-3 rounded-md px-2 outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring",
          isCurrent ? "bg-accent" : "hover:bg-accent/50",
          isConfirmingAction && "bg-destructive/5 hover:bg-destructive/5",
        )}
      >
        {/* Thin ring keeps photo + fallback avatars reading as the same circle
            (the fallback's faint bg otherwise looks smaller). */}
        {agent ? (
          <ActorAvatar actorType="agent" actorId={agent.id} size="lg" enableHoverCard className="ring-1 ring-inset ring-border" />
        ) : (
          <span className="size-8 shrink-0" />
        )}

        <div className="min-w-0 flex-1">
          {/* Line 1: name + time (time stays put; hover actions overlay below) */}
          <div className="flex items-center gap-1.5">
            {session.pinned && session.status !== "archived" && (
              <Pin
                aria-label={t(($) => $.list.pinned)}
                className="size-3 shrink-0 -rotate-45 fill-current text-muted-foreground"
              />
            )}
            {isRenaming ? (
              <div className="min-w-0 flex-1">
                <SessionRenameInput
                  initialValue={session.title?.trim() ?? ""}
                  onSubmit={(value) => handleRenameSubmit(session, value)}
                  onCancel={() => setRenamingId(null)}
                />
              </div>
            ) : (
              <span title={titleText} className={cn("min-w-0 flex-1 truncate text-body", unseen ? "font-bold text-foreground" : "font-medium")}>
                {titleText}
              </span>
            )}
            <span className={cn("ml-auto shrink-0 text-micro", unseen ? "font-semibold text-brand" : "text-muted-foreground")}>{timeText}</span>
          </div>

          {/* Line 2: preview + unread badge, or an inline confirm prompt */}
          <div className="mt-0.5 flex items-center gap-2">
            {isConfirmingDelete ? (
                <ConfirmRow
                  label={t(($) => $.session_history.delete_dialog.title)}
                  cancelText={t(($) => $.session_history.delete_dialog.cancel)}
                  confirmText={
                    deleteSession.isPending
                      ? t(($) => $.session_history.delete_dialog.confirming)
                      : t(($) => $.session_history.delete_dialog.confirm)
                  }
                  pending={deleteSession.isPending}
                  onCancel={() => setConfirmingDeleteId(null)}
                  onConfirm={() => handleConfirmDelete(session)}
                />
              ) : isConfirmingStop && pendingTask ? (
                <ConfirmRow
                  label={t(($) => $.session_history.stop_dialog.title)}
                  cancelText={t(($) => $.session_history.stop_dialog.cancel)}
                  confirmText={
                    stoppingTaskId === pendingTask.task_id
                      ? t(($) => $.session_history.stop_dialog.confirming)
                      : t(($) => $.session_history.stop_dialog.confirm)
                  }
                  pending={stoppingTaskId === pendingTask.task_id}
                  onCancel={() => setConfirmingStopId(null)}
                  onConfirm={() => handleConfirmStop(session, pendingTask)}
                />
              ) : (
                <>
                  <div className="flex min-w-0 flex-1 items-center gap-1.5 overflow-hidden text-caption">
                    {unseen && (
                      <span
                        data-testid="chat-unseen-dot"
                        role="img"
                        aria-label={t(($) => $.list.unseen_dot)}
                        className="size-[7px] shrink-0 rounded-full bg-brand"
                      />
                    )}
                    {showDecisionTag && (
                      <span
                        data-testid="chat-decision-tag"
                        className="shrink-0 rounded-xs bg-brand/15 px-1.5 text-caption font-semibold text-brand"
                      >
                        {t(($) => $.list.tag_decision)}
                      </span>
                    )}
                    {agentName && (
                      <>
                        <span className="max-w-[40%] shrink-0 truncate font-medium text-muted-foreground">
                          {agentName}
                        </span>
                        <span aria-hidden="true" className="shrink-0 text-faint-foreground">
                          ·
                        </span>
                      </>
                    )}
                    <div className="min-w-0 flex-1 overflow-hidden">{previewNode}</div>
                  </div>
                  {unread > 0 && (
                    <span
                      aria-label={t(($) => $.session_history.row_subtitle.new_reply)}
                      // Softer, warmer red than the vivid `destructive` token —
                      // an IM unread badge, not an error.
                      className="inline-flex h-[18px] min-w-[18px] shrink-0 items-center justify-center rounded-full bg-[oklch(0.62_0.14_18)] px-1 text-micro font-semibold text-white"
                    >
                      {unread > 99 ? "99+" : unread}
                    </span>
                  )}
                </>
              )}
            </div>
        </div>

        {/* One ⋯ menu. Where the pointer can hover it shows on row hover or
            keyboard focus; right-click opens the same menu. */}
        {!isConfirmingAction && (
          <RowActionsMenu
            label={t(($) => $.list.row_actions_aria)}
            groups={menuGroups}
            revealOnHover
          />
        )}
      </RowContextMenu>
    );
  };

  // Pinned rows get their own header in Main / All; everything else sits under
  // its day. The Pinned chip is all pinned rows, so a header there says nothing.
  const sections = useMemo(() => {
    const label: Record<DayGroupKey | "pinned", string> = {
      pinned: t(($) => $.list.group.pinned),
      today: `${t(($) => $.list.group.today)} · ${new Date().toLocaleDateString(locale, { weekday: "long", day: "numeric", month: "numeric" })}`,
      yesterday: t(($) => $.list.group.yesterday),
      thisWeek: t(($) => $.list.group.this_week),
      older: t(($) => $.list.group.older),
    };
    const splitPinned = listView === "main" || listView === "all";
    const pinned = splitPinned ? visibleSessions.filter((s) => s.pinned && s.status !== "archived") : [];
    const rest = pinned.length > 0 ? visibleSessions.filter((s) => !pinned.includes(s)) : visibleSessions;
    return [
      { key: "pinned" as const, sessions: pinned },
      ...groupByDay(rest, new Date(), Intl.DateTimeFormat().resolvedOptions().timeZone),
    ]
      .filter((g) => g.sessions.length > 0)
      .map((g) => ({ ...g, label: label[g.key] }));
  }, [visibleSessions, listView, locale, t]);

  let body: React.ReactNode;
  if (rows.length === 0 && loadState === "loading") body = <ListLoading />;
  else if (rows.length === 0 && loadState === "error") body = <ListError onRetry={onRetry} />;
  else if (listView === "waiting" && (visibleSessions.length > 0 || hasUnclassified))
    body = <NeedsYouList sessions={rows} agents={agents} refresh={false} />;
  else if (visibleSessions.length === 0)
    body = <ListEmpty kind={rows.length === 0 ? "none" : listView === "waiting" ? "waiting" : "view"} />;
  else
    body = (
      <div className="contents" data-testid={`chat-thread-group-${listView}`}>
        {sections.map((g) => (
          <section key={g.key}>
            <div
              data-testid="chat-day-header"
              data-group={g.key}
              style={{ position: "sticky", top: 0 }}
              className="z-10 flex items-center gap-2 bg-background px-2 pb-1 pt-2.5 text-caption font-semibold text-muted-foreground"
            >
              {/* Slot for the group checkbox (select mode). */}
              <span data-slot="group-select" className="empty:hidden" />
              <span className="min-w-0 flex-1 truncate">{g.label}</span>
              <span className="shrink-0 tabular-nums">{g.sessions.length}</span>
            </div>
            {g.sessions.map(renderRow)}
          </section>
        ))}
      </div>
    );

  return (
    <>
      <ChatFilterChips value={listView} counts={counts} onChange={setListView} />
      {!online && <ListOffline />}
      {body}
    </>
  );
}

function ConfirmRow({
  label,
  cancelText,
  confirmText,
  pending,
  onCancel,
  onConfirm,
}: {
  label: string;
  cancelText: string;
  confirmText: string;
  pending: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <div className="flex min-w-0 flex-1 items-center gap-2">
      <span className="min-w-0 flex-1 truncate text-caption font-medium text-destructive">{label}</span>
      <div className="flex shrink-0 items-center gap-1">
        <button
          type="button"
          onPointerDown={(e) => {
            e.preventDefault();
            e.stopPropagation();
          }}
          onClick={(e) => {
            e.stopPropagation();
            e.preventDefault();
            onCancel();
          }}
          disabled={pending}
          className="inline-flex h-6 items-center rounded-xs px-2 text-micro font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-50"
        >
          {cancelText}
        </button>
        <button
          type="button"
          onPointerDown={(e) => {
            e.preventDefault();
            e.stopPropagation();
          }}
          onClick={(e) => {
            e.stopPropagation();
            e.preventDefault();
            onConfirm();
          }}
          disabled={pending}
          className="inline-flex h-6 items-center rounded-xs px-2 text-micro font-medium text-destructive transition-colors hover:bg-destructive/10 disabled:opacity-50"
        >
          {confirmText}
        </button>
      </div>
    </div>
  );
}

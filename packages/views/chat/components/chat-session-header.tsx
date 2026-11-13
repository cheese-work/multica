"use client";

import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import {
  Archive,
  ArchiveRestore,
  Info,
  Minimize2,
  MoreHorizontal,
  Pencil,
  EyeOff,
  Pin,
  PinOff,
  Trash2,
  UserRound,
} from "@multica/ui/icons";
import { IconButton } from "@multica/ui/components/common/icon-button";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from "@multica/ui/components/ui/dropdown-menu";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useWorkspacePaths } from "@multica/core/paths";
import {
  useUpdateChatSession,
  useDeleteChatSession,
  useSetChatSessionArchived,
} from "@multica/core/chat/mutations";
import { useChatStore } from "@multica/core/chat";
import { getChatSessionMenuItems } from "@multica/core/chat/commands";
import type { Agent, ChatSession } from "@multica/core/types";
import { isImeComposing } from "@multica/core/utils";
import { ActorAvatar } from "../../common/actor-avatar";
import { AppLink } from "../../navigation";
import { BreadcrumbHeader } from "../../layout/breadcrumb-header";
import { useT } from "../../i18n";
import { ChatContextMeter } from "./chat-context-meter";
import { ChatPresetPicker } from "./chat-preset-picker";
import { OpenDecisionsBadge } from "./open-decisions-badge";
import { ScheduledWakeupsChip } from "./scheduled-wakeups-chip";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";

/**
 * Per-session header for the conversation pane, read as a breadcrumb: the
 * agent the chat belongs to (a link to its page), then the chat's own title,
 * which renames in place. The ⋯ menu holds the session actions.
 */
export function ChatSessionHeader({
  session,
  agent,
  onArchive,
  onUnarchive,
  onTogglePin,
  onMarkUnseen,
  onOpenStatistics,
  renameRequest = 0,
  autoCompactThresholdTokens = null,
  lastTurnTokens,
  onCompact,
  onSetPreset,
  leading,
}: {
  session: ChatSession;
  agent: Agent | null;
  // Archiving the open conversation must move the pane off it (advance to the
  // next chat on desktop, back to the list on mobile), so the parent owns it —
  // see ChatPage.handleArchive. Falls back to a plain status flip if unwired.
  onArchive?: (session: ChatSession) => void;
  /** Unarchive through the shared guard. Falls back to a plain status flip if unwired. */
  onUnarchive?: (session: ChatSession) => void;
  /** Pins or unpins an active DM. Omit to hide the Pin item. */
  onTogglePin?: (session: ChatSession) => void;
  /** Marks an active DM unseen and leaves it. Omit to hide the item. */
  onMarkUnseen?: (session: ChatSession) => void;
  /** Opens the Statistics dialog. Omit to hide the Statistics item. */
  onOpenStatistics?: () => void;
  renameRequest?: number;
  /** Prompt size at which this DM auto-compacts, or null when auto-compact is off. */
  autoCompactThresholdTokens?: number | null;
  /** Prompt growth of the last turn, when known. */
  lastTurnTokens?: number | null;
  /** Sends /compact. Omit to hide the "Compact now" item. */
  onCompact?: () => void;
  /** Sends `/preset <reset|base|name>`. Omit to hide the preset picker. */
  onSetPreset?: (name: string) => void;
  /**
   * Far-left control: the nav toggle while the list is collapsed, or the way
   * back to the list at compact widths. Nothing renders there without one.
   */
  leading?: ReactNode;
}) {
  const { t } = useT("chat");
  const { t: tCommon } = useT("common");
  const wsPaths = useWorkspacePaths();
  const updateSession = useUpdateChatSession();
  const deleteSession = useDeleteChatSession();
  const setArchived = useSetChatSessionArchived();
  const setActiveSession = useChatStore((s) => s.setActiveSession);

  const isArchived = session.status === "archived";
  const menuItems = getChatSessionMenuItems({ archived: isArchived });
  const mainMenuItems = menuItems.filter((item) => item.section === "main");
  const trailingMenuItems = menuItems.filter((item) => item.section === "trailing");

  const [editing, setEditing] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [draft, setDraft] = useState(session.title ?? "");
  const inputRef = useRef<HTMLInputElement>(null);
  const isComposingRef = useRef(false);
  // A browser can blur the input before it emits compositionend. Remember
  // that intent so the final composed value is committed, not the draft.
  const commitAfterCompositionRef = useRef(false);
  const blurCommitTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const handledRenameRequest = useRef(renameRequest);

  const title = session.title?.trim() || t(($) => $.window.untitled);

  useEffect(() => {
    if (editing) {
      inputRef.current?.focus();
      inputRef.current?.select();
    }
  }, [editing]);

  useEffect(
    () => () => {
      if (blurCommitTimeoutRef.current !== null) {
        clearTimeout(blurCommitTimeoutRef.current);
      }
    },
    [],
  );

  const clearPendingBlurCommit = useCallback(() => {
    if (blurCommitTimeoutRef.current === null) return;
    clearTimeout(blurCommitTimeoutRef.current);
    blurCommitTimeoutRef.current = null;
  }, []);

  const startRename = useCallback(() => {
    clearPendingBlurCommit();
    isComposingRef.current = false;
    commitAfterCompositionRef.current = false;
    setDraft(session.title ?? "");
    setEditing(true);
  }, [clearPendingBlurCommit, session.title]);

  useEffect(() => {
    if (renameRequest === handledRenameRequest.current) return;
    handledRenameRequest.current = renameRequest;
    startRename();
  }, [renameRequest, startRename]);

  const commitRename = (raw = draft) => {
    clearPendingBlurCommit();
    isComposingRef.current = false;
    commitAfterCompositionRef.current = false;
    setEditing(false);
    const trimmed = raw.trim();
    if (!trimmed || trimmed === session.title) return;
    updateSession.mutate({ sessionId: session.id, title: trimmed });
  };

  const doDelete = () => {
    setConfirmDelete(false);
    setActiveSession(null);
    deleteSession.mutate(session.id);
  };

  const doArchive = () =>
    onArchive
      ? onArchive(session)
      : setArchived.mutate({ sessionId: session.id, archived: true });
  const doUnarchive = () =>
    onUnarchive
      ? onUnarchive(session)
      : setArchived.mutate({ sessionId: session.id, archived: false });

  const leaf = editing ? (
    <input
      ref={inputRef}
      value={draft}
      maxLength={200}
      aria-label={t(($) => $.header.rename)}
      onChange={(e) => setDraft(e.target.value)}
      onCompositionStart={() => {
        isComposingRef.current = true;
      }}
      onCompositionEnd={(e) => {
        isComposingRef.current = false;
        if (!commitAfterCompositionRef.current) return;
        commitRename(e.currentTarget.value);
      }}
      onBlur={(e) => {
        if (isComposingRef.current) {
          commitAfterCompositionRef.current = true;
          const input = e.currentTarget;
          // Let a compositionend queued by the same focus change win.
          // If it never arrives, commit on the next task instead of
          // leaving an unfocused editor open indefinitely.
          clearPendingBlurCommit();
          blurCommitTimeoutRef.current = setTimeout(() => {
            if (!commitAfterCompositionRef.current) return;
            // Without compositionend, the current DOM value is not
            // known to be final. Close without persisting a partial
            // composition instead of recreating the original bug.
            if (isComposingRef.current) {
              clearPendingBlurCommit();
              isComposingRef.current = false;
              commitAfterCompositionRef.current = false;
              setEditing(false);
              return;
            }
            commitRename(input.value);
          }, 0);
          return;
        }
        commitRename(e.currentTarget.value);
      }}
      onKeyDown={(e) => {
        if (isImeComposing(e)) return;
        if (e.key === "Enter") {
          e.preventDefault();
          commitRename();
        } else if (e.key === "Escape") {
          e.preventDefault();
          clearPendingBlurCommit();
          isComposingRef.current = false;
          commitAfterCompositionRef.current = false;
          setEditing(false);
        }
      }}
      className="min-w-0 max-w-96 flex-1 rounded-sm bg-background px-1 py-0.5 text-body font-medium outline-none ring-1 ring-border focus-visible:ring-brand"
    />
  ) : (
    <TooltipHint label={title}>
      <button
        type="button"
        onClick={startRename}
        className="min-w-0 truncate text-left text-body font-medium text-foreground outline-none hover:text-foreground/80 focus-visible:text-foreground/80"
      >
        {title}
      </button>
    </TooltipHint>
  );

  return (
    <>
      <BreadcrumbHeader
        leading={leading ?? false}
        segments={
          agent
            ? [
                {
                  href: wsPaths.agentDetail(agent.id),
                  label: (
                    <>
                      <ActorAvatar
                        actorType="agent"
                        actorId={agent.id}
                        size="sm"
                        profileLink={false}
                        showStatusDot
                      />
                      <span className="truncate">{agent.name}</span>
                    </>
                  ),
                  className: "flex min-w-0 max-w-48 shrink items-center gap-1.5",
                },
              ]
            : []
        }
        leaf={leaf}
        actions={
          <>
            <OpenDecisionsBadge />
            {isArchived ? null : (
              <ScheduledWakeupsChip sessionId={session.id} agentName={agent?.name} />
            )}

            {isArchived ? null : (
              <ChatContextMeter
                tokens={session.context_tokens}
                window={session.context_window}
                thresholdTokens={autoCompactThresholdTokens}
                lastTurnTokens={lastTurnTokens}
              />
            )}

            {isArchived || !agent || !onSetPreset ? null : (
              <ChatPresetPicker
                agentId={agent.id}
                current={session.preset_name ?? null}
                onSelect={onSetPreset}
              />
            )}

            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <IconButton
                    label={tCommon(($) => $.more_actions)}
                    tooltipSide="bottom"
                    className="text-muted-foreground"
                  />
                }
              >
                <MoreHorizontal className="h-4 w-4" />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-auto">
                {mainMenuItems.map((item) => {
                  if (item.id === "statistic") {
                    return onOpenStatistics ? (
                      <DropdownMenuItem
                        key={item.id}
                        data-testid="chat-more-menu-statistics"
                        onClick={onOpenStatistics}
                      >
                        <Info className="h-4 w-4" />
                        {t(($) => $.header.statistics)}
                      </DropdownMenuItem>
                    ) : null;
                  }
                  if (item.id === "pin") {
                    return onTogglePin ? (
                      <DropdownMenuItem
                        key={item.id}
                        data-testid="chat-more-menu-pin"
                        onClick={() => onTogglePin(session)}
                      >
                        {session.pinned ? <PinOff className="h-4 w-4" /> : <Pin className="h-4 w-4 -rotate-45" />}
                        {session.pinned ? t(($) => $.header.unpin) : t(($) => $.header.pin)}
                      </DropdownMenuItem>
                    ) : null;
                  }
                  if (item.id === "unseen") {
                    return onMarkUnseen ? (
                      <DropdownMenuItem
                        key={item.id}
                        data-testid="chat-more-menu-unseen"
                        onClick={() => onMarkUnseen(session)}
                      >
                        <EyeOff className="h-4 w-4" />
                        {t(($) => $.header.unseen)}
                      </DropdownMenuItem>
                    ) : null;
                  }
                  if (item.id === "rename") {
                    return (
                      <DropdownMenuItem
                        key={item.id}
                        data-testid="chat-more-menu-rename"
                        onClick={startRename}
                      >
                        <Pencil className="h-4 w-4" />
                        {t(($) => $.header.rename)}
                      </DropdownMenuItem>
                    );
                  }
                  return null;
                })}
                {agent && (
                  <DropdownMenuItem
                    data-testid="chat-more-menu-view-profile"
                    render={<AppLink href={wsPaths.agentDetail(agent.id)} />}
                  >
                    <UserRound className="h-4 w-4" />
                    {t(($) => $.header.view_profile)}
                  </DropdownMenuItem>
                )}
                {onCompact && !isArchived ? (
                  <DropdownMenuItem
                    data-testid="chat-more-menu-compact"
                    disabled={session.compacting === true}
                    onClick={onCompact}
                  >
                    <Minimize2 className="h-4 w-4" />
                    {t(($) => $.header.compact_now)}
                  </DropdownMenuItem>
                ) : null}
                <DropdownMenuSeparator />
                {trailingMenuItems.map((item) => {
                  if (item.id === "unarchive") {
                    return (
                      <DropdownMenuItem
                        key={item.id}
                        data-testid="chat-more-menu-archive"
                        onClick={doUnarchive}
                      >
                        <ArchiveRestore className="h-4 w-4" />
                        {t(($) => $.header.unarchive)}
                      </DropdownMenuItem>
                    );
                  }
                  if (item.id === "delete") {
                    return (
                      <DropdownMenuItem
                        key={item.id}
                        data-testid="chat-more-menu-delete"
                        variant={item.destructive ? "destructive" : undefined}
                        onClick={() => setConfirmDelete(true)}
                      >
                        <Trash2 className="h-4 w-4" />
                        {t(($) => $.header.delete)}
                      </DropdownMenuItem>
                    );
                  }
                  if (item.id === "archive") {
                    return (
                      <DropdownMenuItem
                        key={item.id}
                        data-testid="chat-more-menu-archive"
                        onClick={doArchive}
                      >
                        <Archive className="h-4 w-4" />
                        {t(($) => $.header.archive)}
                      </DropdownMenuItem>
                    );
                  }
                  return null;
                })}
              </DropdownMenuContent>
            </DropdownMenu>
          </>
        }
      />

      <AlertDialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.session_history.delete_dialog.title)}</AlertDialogTitle>
            <AlertDialogDescription>{title}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.session_history.delete_dialog.cancel)}</AlertDialogCancel>
            <AlertDialogAction
              onClick={doDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {t(($) => $.session_history.delete_dialog.confirm)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

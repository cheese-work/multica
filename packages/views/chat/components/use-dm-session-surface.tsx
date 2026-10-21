"use client";

import { useState, type ComponentProps, type ReactNode } from "react";
import type { ChatSession } from "@multica/core/types";
import type { ChatInput } from "./chat-input";
import { ChatSessionStatsDialog } from "./chat-session-stats-dialog";
import { useArchivedSend } from "./use-archived-send";
import { useChatSessionActions } from "./use-chat-session-actions";

type InputProps = ComponentProps<typeof ChatInput>;
type SendArgs = Parameters<InputProps["onSend"]>;

/**
 * The statistics dialog, the DM commands and the archived-send confirmation
 * for one open DM, shared by the Chat page and the floating window so the two
 * surfaces cannot drift. The owner passes its own navigation for archive and
 * for opening a DM; everything else is the same.
 */
export function useDmSessionSurface({
  session,
  sessions,
  send,
  onOpenSession,
  onArchived,
  onRename,
  onFork,
}: {
  session: ChatSession | null | undefined;
  sessions: ChatSession[];
  send: (...args: SendArgs) => ReturnType<InputProps["onSend"]>;
  onOpenSession: (session: ChatSession) => void;
  /** Moves the pane off a DM after the server archived it. */
  onArchived: (session: ChatSession) => void;
  onRename?: InputProps["onRename"];
  onFork?: InputProps["onFork"];
}): {
  inputProps: Pick<
    InputProps,
    | "onSend"
    | "onRename"
    | "onFork"
    | "onStatistic"
    | "onArchive"
    | "onPin"
    | "onUnarchive"
    | "sessionArchived"
  >;
  dialogs: ReactNode;
  openStatistics: (sessionId: string) => void;
  archive: (session: ChatSession) => Promise<boolean>;
  unarchive: (session: ChatSession) => Promise<boolean>;
  togglePin: (session: ChatSession) => Promise<boolean>;
} {
  const actions = useChatSessionActions();
  const [statsSessionId, setStatsSessionId] = useState<string | null>(null);

  const archive = (target: ChatSession) =>
    actions.archive(target, { onArchived: () => onArchived(target) });

  const archivedSend = useArchivedSend<SendArgs>({
    session,
    sessions,
    send,
    unarchive: actions.unarchive,
    onOpenSession,
  });

  const archived = session?.status === "archived";
  const active = session && !archived ? session : null;

  return {
    inputProps: {
      onSend: archivedSend.guardedSend,
      // An archived DM answers only /unarchive; the agent commands wait for it.
      onRename: archived ? undefined : onRename,
      onFork: archived ? undefined : onFork,
      onStatistic: active ? () => setStatsSessionId(active.id) : undefined,
      onArchive: active ? () => archive(active) : undefined,
      onPin: active ? () => actions.togglePin(active) : undefined,
      onUnarchive: session && archived ? () => actions.unarchive(session) : undefined,
      sessionArchived: archived,
    },
    dialogs: (
      <>
        {archivedSend.dialog}
        <ChatSessionStatsDialog
          sessionId={statsSessionId}
          open={statsSessionId !== null}
          onOpenChange={(open) => !open && setStatsSessionId(null)}
          sessions={sessions}
          onOpenSession={onOpenSession}
        />
      </>
    ),
    openStatistics: setStatsSessionId,
    archive,
    unarchive: actions.unarchive,
    togglePin: actions.togglePin,
  };
}

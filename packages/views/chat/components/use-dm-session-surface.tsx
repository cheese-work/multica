"use client";

import { useState, type ComponentProps, type ReactNode } from "react";
import { toast } from "sonner";
import { useChatAttentionLater } from "@multica/core/chat/attention-mutations";
import { parseLaterDuration } from "@multica/core/chat/attention";
import type { ChatSession } from "@multica/core/types";
import { createSafeId } from "@multica/core/utils";
import { useT } from "../../i18n";
import type { ChatInput } from "./chat-input";
import { ChatSessionStatsDialog } from "./chat-session-stats-dialog";
import { useArchivedSend } from "./use-archived-send";
import { useChatSessionActions } from "./use-chat-session-actions";

type InputProps = ComponentProps<typeof ChatInput>;
type SendArgs = Parameters<InputProps["onSend"]>;

const DEFAULT_LATER_SECONDS = 2 * 3600;

/** Resume time as people read it here: 24-hour clock in ICT. */
function formatIctTime(date: Date): string {
  return new Intl.DateTimeFormat("en-GB", {
    timeZone: "Asia/Ho_Chi_Minh",
    day: "2-digit",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  }).format(date);
}

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
  onUnseen,
  onRename,
  onFork,
}: {
  session: ChatSession | null | undefined;
  sessions: ChatSession[];
  send: (...args: SendArgs) => ReturnType<InputProps["onSend"]>;
  onOpenSession: (session: ChatSession) => void;
  /** Moves the pane off a DM after the server archived it. */
  onArchived: (session: ChatSession) => void;
  /** Moves the pane off a DM after the server marked it unseen. */
  onUnseen: (session: ChatSession) => void;
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
    | "onUnseen"
    | "onUnarchive"
    | "onLater"
    | "replyAskIds"
    | "presetAgentId"
    | "sessionArchived"
  >;
  dialogs: ReactNode;
  openStatistics: (sessionId: string) => void;
  archive: (session: ChatSession) => Promise<boolean>;
  unarchive: (session: ChatSession) => Promise<boolean>;
  togglePin: (session: ChatSession) => Promise<boolean>;
  markUnseen: (session: ChatSession) => Promise<boolean>;
} {
  const actions = useChatSessionActions();
  const { t } = useT("chat");
  const later = useChatAttentionLater();
  const [statsSessionId, setStatsSessionId] = useState<string | null>(null);

  const archive = (target: ChatSession) =>
    actions.archive(target, { onArchived: () => onArchived(target) });

  const markUnseen = (target: ChatSession) =>
    actions.markUnseen(target, { onMarked: () => onUnseen(target) });

  const archivedSend = useArchivedSend<SendArgs>({
    session,
    sessions,
    send,
    unarchive: actions.unarchive,
    onOpenSession,
  });

  const archived = session?.status === "archived";
  const active = session && !archived ? session : null;

  // /later snoozes everything this DM shows, decisions included (T5); the
  // command never reaches the agent as a message (E16).
  const snoozeSession = async (target: ChatSession, duration: string): Promise<boolean> => {
    const parsed = parseLaterDuration(duration);
    if (!parsed.ok) {
      toast.error(t(($) => $.input.later_bad_duration));
      return false;
    }
    try {
      await later.mutateAsync({
        sessionId: target.id,
        keys: target.attention_keys ?? [],
        durationSeconds: parsed.seconds,
        requestId: createSafeId(),
      });
    } catch {
      toast.error(t(($) => $.input.later_failed_toast));
      return false;
    }
    const until = new Date(Date.now() + (parsed.seconds ?? DEFAULT_LATER_SECONDS) * 1000);
    toast(t(($) => $.input.later_set_toast, { time: formatIctTime(until) }));
    return true;
  };

  return {
    inputProps: {
      onSend: archivedSend.guardedSend,
      // An archived DM answers only /unarchive; the agent commands wait for it.
      onRename: archived ? undefined : onRename,
      onFork: archived ? undefined : onFork,
      onStatistic: active ? () => setStatsSessionId(active.id) : undefined,
      onArchive: active ? () => archive(active) : undefined,
      onPin: active ? () => actions.togglePin(active) : undefined,
      onUnseen: active ? () => markUnseen(active) : undefined,
      onLater: active ? (duration) => snoozeSession(active, duration) : undefined,
      replyAskIds: active?.attention_ask_ids,
      onUnarchive: session && archived ? () => actions.unarchive(session) : undefined,
      // The server answers /preset only inside a saved, active DM.
      presetAgentId: active?.agent_id,
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
    markUnseen,
  };
}

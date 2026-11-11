"use client";

import { useEffect, useRef, type ReactNode } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { motion, useIsPresent } from "motion/react";
import type { AgentAvailability } from "@multica/core/agents";
import { errorCode } from "@multica/core/api";
import type { ChatMessage, ChatPendingTask, ChatQuickAction, ChatSession } from "@multica/core/types";
import { chatMessagesPageOptions } from "@multica/core/chat/queries";
import { hideQueuedChatMessages } from "@multica/core/chat/pending";
import { useRegenerateChatQuickActions } from "@multica/core/chat/mutations";
import { ChatMessageList, ChatMessageSkeleton } from "./chat-message-list";
import { InteractiveLoginBanner } from "./interactive-login-banner";

/** Message-only observers leave with the fading list; the composer stays alive. */
export function ChatWindowMessages({
  sessionId,
  session,
  sessions,
  interactiveLoginRequired,
  onMessages,
  pendingTask,
  pendingTaskLoading,
  availability,
  onQuickAction,
  quickActionsDisabled,
  quickActionsPendingMessageId,
  onQuoteMessage,
  onReady,
  children,
}: {
  sessionId: string | null;
  /** Active session; its continuation / fork links keep an empty DM on the list. */
  session?: ChatSession | null;
  /** Session list cache, the title source for fork links; undefined while loading. */
  sessions?: ChatSession[];
  /** The session list already failed with `interactive_login_required`. */
  interactiveLoginRequired: boolean;
  /**
   * The messages this list shows, for the window's annotations. They are
   * reported up because the observer that reads them must leave with the list;
   * an empty list is reported when it unmounts.
   */
  onMessages?: (messages: ChatMessage[]) => void;
  pendingTask: ChatPendingTask | null | undefined;
  pendingTaskLoading: boolean;
  availability: AgentAvailability | undefined;
  onQuickAction: (action: ChatQuickAction) => void | Promise<unknown>;
  quickActionsDisabled: boolean;
  quickActionsPendingMessageId: string | null;
  onQuoteMessage?: (content: string) => void;
  /** This area has something to show — the window can fade in around it. */
  onReady?: () => void;
  children: ReactNode;
}) {
  const isPresent = useIsPresent();
  const regenerateQuickActions = useRegenerateChatQuickActions();
  const {
    data,
    isLoading,
    error,
    fetchNextPage,
    hasNextPage,
    isFetchingNextPage,
  } = useInfiniteQuery(chatMessagesPageOptions(sessionId ?? ""));
  const pages = sessionId ? data?.pages ?? [] : [];
  const messages = hideQueuedChatMessages(
    [...pages].reverse().flatMap((page) => page.messages),
    pendingTask ?? undefined,
  );
  const loginRequired =
    interactiveLoginRequired || errorCode(error) === "interactive_login_required";
  const hasSessionLinks = !!(
    session?.continued_from_session_id ||
    session?.continued_into_session_id ||
    session?.forked_from_session_id
  );

  const showSkeleton = !loginRequired && !!sessionId && (isLoading || pendingTaskLoading);
  const showList =
    !loginRequired &&
    !showSkeleton &&
    (messages.length > 0 || !!pendingTask?.task_id || hasSessionLinks);

  // The skeleton and the empty state are right on their first frame; the list
  // is hidden until it lands, so it reports through `onRevealed` instead.
  useEffect(() => {
    if (isPresent && !showList) onReady?.();
  }, [isPresent, showList, onReady]);

  const messagesRef = useRef(messages);
  messagesRef.current = messages;
  // Only a change of the message set is news; a re-render with the same rows is not.
  const messageSet = messages.map((m) => `${m.id}:${m.role}`).join(",");
  useEffect(() => {
    onMessages?.(messagesRef.current);
  }, [messageSet, onMessages]);
  useEffect(() => () => onMessages?.([]), [onMessages]);

  return (
    <motion.div
      className="flex flex-1 min-h-0 flex-col"
      initial={false}
      animate={{ opacity: 1 }}
      exit={{ opacity: 0 }}
      transition={{ duration: 0.15 }}
    >
      {loginRequired ? (
        <InteractiveLoginBanner />
      ) : showSkeleton ? (
        <ChatMessageSkeleton />
      ) : showList ? (
        <ChatMessageList
          key={sessionId}
          active={isPresent}
          revealKey={sessionId}
          onRevealed={onReady}
          messages={messages}
          continuedFromSessionId={session?.continued_from_session_id}
          continuedIntoSessionId={session?.continued_into_session_id}
          sessionId={sessionId}
          forkedFromSessionId={session?.forked_from_session_id}
          forkLastMessageId={session?.fork_last_message_id}
          forkMessageCount={session?.fork_message_count}
          sessions={sessions}
          pendingTask={pendingTask}
          availability={availability}
          hasOlderMessages={!!hasNextPage}
          isFetchingOlderMessages={isFetchingNextPage}
          onLoadOlderMessages={() => void fetchNextPage()}
          onQuickAction={onQuickAction}
          quickActionsDisabled={quickActionsDisabled}
          onRegenerateQuickActions={(message) => sessionId
            ? regenerateQuickActions.mutateAsync({ sessionId, messageId: message.id })
            : undefined}
          quickActionsPendingMessageId={quickActionsPendingMessageId}
          onQuoteMessage={onQuoteMessage}
        />
      ) : children}
    </motion.div>
  );
}

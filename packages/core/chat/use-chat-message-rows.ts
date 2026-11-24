import { skipToken, useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import type { ChatMessage } from "../types";
import type { EntityTable } from "../normalized/entity-table";
import { selectChatMessageRows, type ChatMessageIds } from "./message-cache";
import { chatKeys, chatMessagesPageOptions } from "./queries";

/**
 * The transcript of one session, oldest first, read from the by-id store
 * (CHE-3236 B1a.2). `query` is the paged fetch: it owns loading, errors and
 * `fetchNextPage`, and its `pageParams` give the page order. The rows come
 * from the entity table and the session's ordered id lists, so an upsert by id
 * reaches every reader without rewriting a page array.
 */
export function useChatMessageRows(sessionId: string) {
  const query = useInfiniteQuery(chatMessagesPageOptions(sessionId));
  // Observer only: the store is written by the message door, never fetched here.
  const { data: entities } = useQuery({
    queryKey: chatKeys.messageEntities(),
    queryFn: skipToken,
    staleTime: Infinity,
  });
  const { data: messageIds } = useQuery({
    queryKey: chatKeys.messageIds(sessionId),
    queryFn: skipToken,
    staleTime: Infinity,
  });
  const pageParams = query.data?.pageParams;
  const messages = useMemo<ChatMessage[]>(
    () =>
      sessionId
        ? selectChatMessageRows(
            entities as EntityTable<ChatMessage> | undefined,
            messageIds as ChatMessageIds | undefined,
            pageParams,
          )
        : EMPTY,
    [sessionId, entities, messageIds, pageParams],
  );
  return { query, messages };
}

const EMPTY: ChatMessage[] = [];

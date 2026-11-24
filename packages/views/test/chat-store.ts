import type { QueryClient } from "@tanstack/react-query";
import { ingestChatMessagesPage } from "@multica/core/chat/message-cache";
import { chatKeys } from "@multica/core/chat/queries";
import type { ChatMessage } from "@multica/core/types";

/**
 * Seed one loaded page of a chat session the way a fetch lands it: the rows go
 * into the by-id store and the page cache keeps the page order. Readers resolve
 * rows from the store, so setting only the page cache shows an empty transcript.
 */
export function seedChatPages(qc: QueryClient, sessionId: string, messages: ChatMessage[]) {
  const page = ingestChatMessagesPage(qc, sessionId, null, {
    messages,
    limit: 50,
    has_more: false,
    next_cursor: null,
  });
  qc.setQueryData(chatKeys.messagesPage(sessionId), { pages: [page], pageParams: [null] });
}

import type { InfiniteData, QueryClient } from "@tanstack/react-query";
import { chatKeys } from "./queries";
import type { ChatMessage, ChatMessagesPage } from "../types";
import {
  mergeFillGaps,
  mergeSnapshot,
  removeEntity,
  resolveEntities,
  upsertEntities,
  type EntityTable,
} from "../normalized/entity-table";

/**
 * The single door a chat message goes through on its way into the message
 * caches (MUL-5711), and the owner of the by-id message store behind them
 * (CHE-3236 B1a.1).
 *
 * Store: `chatKeys.messageEntities()` holds one row per message id, and
 * `chatKeys.messageIds(sessionId)` holds the ordered ids of one session (per
 * fetched page, plus the flat Agent Builder list). Readers resolve rows with
 * `selectChatMessageRows` (see `useChatMessageRows`); `chatKeys.messagesPage`
 * only carries the page order and cursors, so its `messages` are the fetched
 * snapshot and no reader uses them. `chatKeys.messages` (Agent Builder) is a
 * derived row copy written only here and by the fetch.
 *
 * Two rules, both order-independent, so the same message arriving twice by
 * different routes converges instead of duplicating or downgrading:
 *
 *   1. Identity is `message.id`. A second write for an id already present
 *      merges instead of appending, and an id appears once across all pages.
 *   2. On a realtime or send write the row already in the store wins every
 *      field it defines; the incoming row only fills fields that are missing. A
 *      send response carrying `attachments` therefore survives the WebSocket
 *      echo that has no attachments field at all — in either arrival order.
 *      An HTTP page is an authoritative snapshot instead: its fields replace
 *      the stored ones (a refetch is how a decided card or edited row
 *      refreshes), and fields only the store holds survive.
 */

export interface ChatMessageIds {
  /** Ids of the flat (Agent Builder) list, oldest first. */
  flat?: string[];
  /** Ids per fetched page, keyed by `pageParamKey`; each oldest first. */
  byPage: Record<string, string[]>;
}

type PageParam = ChatMessagesPage["next_cursor"] | undefined;

export function pageParamKey(param: PageParam): string {
  return param ? `${param.created_at}|${param.id}` : "first";
}

const ids = (messages: readonly ChatMessage[]) => messages.map((m) => m.id);

function sameRows<T>(a: readonly T[], b: readonly T[]): boolean {
  return a.length === b.length && a.every((row, i) => row === b[i]);
}

/**
 * Rows of every fetched page, newest page first, each oldest first. Page 0 is
 * the newest window and later pages are older cursor pages, so an id already
 * shown by a newer page is skipped on an older one. `pageParams` is the page
 * order of the infinite query; the rows come from the store, never from the
 * page arrays.
 */
export function selectChatMessagePageRows(
  entities: EntityTable<ChatMessage> | undefined,
  messageIds: ChatMessageIds | undefined,
  pageParams: readonly unknown[] | undefined,
): ChatMessage[][] {
  const seen = new Set<string>();
  return (pageParams ?? []).map((param) => {
    const pageIds = messageIds?.byPage[pageParamKey(param as PageParam)] ?? [];
    const rows = resolveEntities(entities, pageIds.filter((id) => !seen.has(id)));
    rows.forEach((row) => seen.add(row.id));
    return rows;
  });
}

/** The transcript, oldest first, resolved from the store. */
export function selectChatMessageRows(
  entities: EntityTable<ChatMessage> | undefined,
  messageIds: ChatMessageIds | undefined,
  pageParams: readonly unknown[] | undefined,
): ChatMessage[] {
  return selectChatMessagePageRows(entities, messageIds, pageParams).reverse().flat();
}

function selectChatMessages(
  entities: EntityTable<ChatMessage> | undefined,
  flatIds: readonly string[],
  current: ChatMessage[],
): ChatMessage[] {
  const rows = resolveEntities(entities, flatIds);
  return sameRows(rows, current) ? current : rows;
}

function emptySeedPage(): InfiniteData<ChatMessagesPage> {
  return {
    pages: [{ messages: [], limit: 50, has_more: false, next_cursor: null }],
    pageParams: [null],
  };
}

function readStore(qc: QueryClient, sessionId: string) {
  return {
    entities: qc.getQueryData<EntityTable<ChatMessage>>(chatKeys.messageEntities()),
    ids: qc.getQueryData<ChatMessageIds>(chatKeys.messageIds(sessionId)) ?? { byPage: {} },
  };
}

function writeStore(
  qc: QueryClient,
  sessionId: string,
  entities: EntityTable<ChatMessage> | undefined,
  messageIds: ChatMessageIds,
) {
  if (entities) qc.setQueryData(chatKeys.messageEntities(), entities);
  qc.setQueryData(chatKeys.messageIds(sessionId), messageIds);
}

/**
 * Fold what a cache already shows into the store. Covers a cache populated
 * before the store existed or after the store entry was garbage collected: a
 * page or flat list with no id list yet is adopted as it stands.
 */
function adoptCaches(
  qc: QueryClient,
  sessionId: string,
  entities: EntityTable<ChatMessage> | undefined,
  messageIds: ChatMessageIds,
) {
  const flat = qc.getQueryData<ChatMessage[]>(chatKeys.messages(sessionId));
  const paged = qc.getQueryData<InfiniteData<ChatMessagesPage>>(chatKeys.messagesPage(sessionId));
  let nextEntities = entities;
  let nextIds = messageIds;
  if (flat && !messageIds.flat) {
    nextEntities = upsertEntities(nextEntities, flat, mergeSnapshot);
    nextIds = { ...nextIds, flat: ids(flat) };
  }
  paged?.pages.forEach((page, index) => {
    const key = pageParamKey(paged.pageParams[index] as PageParam);
    if (nextIds.byPage[key]) return;
    nextEntities = upsertEntities(nextEntities, page.messages, mergeSnapshot);
    nextIds = { ...nextIds, byPage: { ...nextIds.byPage, [key]: ids(page.messages) } };
  });
  return { entities: nextEntities, ids: nextIds };
}

/** Rewrite the flat row copy (Agent Builder) from the store. Reference-stable when nothing moved. */
function project(
  qc: QueryClient,
  sessionId: string,
  entities: EntityTable<ChatMessage> | undefined,
  messageIds: ChatMessageIds,
) {
  qc.setQueryData<ChatMessage[] | undefined>(chatKeys.messages(sessionId), (old) =>
    old && messageIds.flat ? selectChatMessages(entities, messageIds.flat, old) : old,
  );
}

/**
 * HTTP route, paged: upsert the fetched page into the store and return it with
 * rows resolved from the store. A page fetched after its request was aborted is
 * returned untouched, so a stale response cannot overwrite a newer write.
 */
export function ingestChatMessagesPage(
  qc: QueryClient,
  sessionId: string,
  pageParam: PageParam,
  page: ChatMessagesPage,
  signal?: AbortSignal,
): ChatMessagesPage {
  if (signal?.aborted) return page;
  const { entities, ids: messageIds } = readStore(qc, sessionId);
  const nextEntities = upsertEntities(entities, page.messages, mergeSnapshot);
  const nextIds = {
    ...messageIds,
    byPage: { ...messageIds.byPage, [pageParamKey(pageParam)]: ids(page.messages) },
  };
  writeStore(qc, sessionId, nextEntities, nextIds);
  const messages = resolveEntities(nextEntities, nextIds.byPage[pageParamKey(pageParam)]!);
  return sameRows(messages, page.messages) ? page : { ...page, messages };
}

/** HTTP route, flat list (Agent Builder). */
export function ingestChatMessages(
  qc: QueryClient,
  sessionId: string,
  messages: ChatMessage[],
  signal?: AbortSignal,
): ChatMessage[] {
  if (signal?.aborted) return messages;
  const { entities, ids: messageIds } = readStore(qc, sessionId);
  const nextEntities = upsertEntities(entities, messages, mergeSnapshot);
  writeStore(qc, sessionId, nextEntities, { ...messageIds, flat: ids(messages) });
  return selectChatMessages(nextEntities, ids(messages), messages);
}

export interface UpsertChatMessageOptions {
  /**
   * Create the cache entry when the session has never been fetched.
   *
   * Send paths pass `true`: a brand-new session must render its first message
   * before any fetch exists, and the send's own invalidate reconciles the
   * placeholder page immediately after.
   *
   * WebSocket handlers pass `false` (the default). An event for a session this
   * client has not opened must not fabricate a one-message history that a later
   * reader would mistake for the whole transcript — the first real fetch owns
   * that.
   */
  seedIfMissing?: boolean;
}

/** Run a store change for one session, then re-derive the row-shaped caches. */
function writeThroughStore(
  qc: QueryClient,
  sessionId: string,
  change: (
    entities: EntityTable<ChatMessage> | undefined,
    messageIds: ChatMessageIds,
  ) => { entities: EntityTable<ChatMessage> | undefined; ids: ChatMessageIds },
) {
  const store = readStore(qc, sessionId);
  const adopted = adoptCaches(qc, sessionId, store.entities, store.ids);
  const next = change(adopted.entities, adopted.ids);
  writeStore(qc, sessionId, next.entities, next.ids);
  project(qc, sessionId, next.entities, next.ids);
}

function addId(list: string[] | undefined, id: string): string[] | undefined {
  return !list || list.includes(id) ? list : [...list, id];
}

/**
 * Upsert one settled message into the store and both message caches.
 *
 * Both are written because both are read: `chatKeys.messagesPage` backs the
 * chat surfaces, `chatKeys.messages` backs the Agent Builder. Writing one and
 * not the other is how they drift.
 */
export function upsertChatMessageToCaches(
  qc: QueryClient,
  sessionId: string,
  message: ChatMessage,
  { seedIfMissing = false }: UpsertChatMessageOptions = {},
): void {
  const keys = [chatKeys.messages(sessionId), chatKeys.messagesPage(sessionId)] as const;
  const recoveringKeys = keys.filter((queryKey) =>
    qc.getQueryState?.(queryKey)?.fetchStatus === "fetching",
  );
  const writeMessage = () => {
    if (seedIfMissing) {
      if (!qc.getQueryData(keys[0])) qc.setQueryData<ChatMessage[]>(keys[0], []);
      if (!qc.getQueryData<InfiniteData<ChatMessagesPage>>(keys[1])?.pages.length) {
        qc.setQueryData(keys[1], emptySeedPage());
      }
    }
    // An event for a session this client never opened must leave no trace.
    if (!qc.getQueryData(keys[0]) && !qc.getQueryData(keys[1])) return Promise.resolve([]);
    writeThroughStore(qc, sessionId, (entities, messageIds) => {
      const nextEntities = upsertEntities(entities, [message], mergeFillGaps);
      const firstPage = qc.getQueryData<InfiniteData<ChatMessagesPage>>(keys[1])?.pageParams[0];
      const firstKey = pageParamKey(firstPage as PageParam);
      const onSomePage = Object.values(messageIds.byPage).some((list) => list.includes(message.id));
      const byPage =
        !onSomePage && messageIds.byPage[firstKey]
          ? { ...messageIds.byPage, [firstKey]: [...messageIds.byPage[firstKey]!, message.id] }
          : messageIds.byPage;
      return {
        entities: nextEntities,
        ids: { flat: addId(messageIds.flat, message.id), byPage },
      };
    });
    return Promise.all(
      recoveringKeys.map((queryKey) =>
        qc.invalidateQueries({ queryKey, refetchType: "active" }),
      ),
    );
  };
  if (recoveringKeys.length) {
    void Promise.all(recoveringKeys.map((queryKey) => qc.cancelQueries({ queryKey })))
      .then(writeMessage);
  } else {
    void writeMessage();
  }
}

/** Replace one stored message in place (a late field such as quick actions). Absent ids are left alone. */
export function patchChatMessageInCaches(
  qc: QueryClient,
  sessionId: string,
  messageId: string,
  patch: (message: ChatMessage) => ChatMessage,
): void {
  writeThroughStore(qc, sessionId, (entities, messageIds) => {
    const current = entities?.byId[messageId];
    if (!entities || !current) return { entities, ids: messageIds };
    const next = patch(current);
    return {
      entities: next === current ? entities : { byId: { ...entities.byId, [messageId]: next } },
      ids: messageIds,
    };
  });
}

export function removeChatMessageFromCaches(
  qc: QueryClient,
  sessionId: string,
  messageId: string,
): void {
  writeThroughStore(qc, sessionId, (entities, messageIds) => ({
    entities: removeEntity(entities, messageId),
    ids: {
      ...(messageIds.flat ? { flat: messageIds.flat.filter((id) => id !== messageId) } : {}),
      byPage: Object.fromEntries(
        Object.entries(messageIds.byPage).map(([key, list]) => [
          key,
          list.filter((id) => id !== messageId),
        ]),
      ),
    },
  }));
}

/** Every message id stored for a session, across the flat list and all pages. */
function storedIds(messageIds: ChatMessageIds | undefined): string[] {
  return [...(messageIds?.flat ?? []), ...Object.values(messageIds?.byPage ?? {}).flat()];
}

/** Rows the store holds for a session, whichever list they sit on. */
export function storedChatMessages(qc: QueryClient, sessionId: string): ChatMessage[] {
  const { entities, ids: messageIds } = readStore(qc, sessionId);
  return resolveEntities(entities, storedIds(messageIds));
}

/** True when the session's id lists hold `messageId`. */
export function hasStoredChatMessage(qc: QueryClient, sessionId: string, messageId: string): boolean {
  return storedIds(readStore(qc, sessionId).ids).includes(messageId);
}

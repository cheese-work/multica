import { QueryClient, type InfiniteData } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ChatMessage, ChatMessagesPage } from "../types";
import type { EntityTable } from "../normalized/entity-table";

const listChatMessagesPage = vi.fn();
const listChatMessages = vi.fn();
vi.mock("../api", () => ({ api: { listChatMessagesPage, listChatMessages } }));

const { chatKeys, chatMessagesOptions, chatMessagesPageOptions } = await import("./queries");
const {
  patchChatMessageInCaches,
  removeChatMessageFromCaches,
  selectChatMessagePages,
  upsertChatMessageToCaches,
} = await import("./message-cache");
type ChatMessageIds = import("./message-cache").ChatMessageIds;

const sid = "s1";
const cursor = { created_at: "2026-05-13T04:00:00Z", id: "m-3" };

const msg = (id: string, over: Partial<ChatMessage> = {}): ChatMessage => ({
  id,
  chat_session_id: sid,
  role: "user",
  content: id,
  task_id: "t",
  created_at: "2026-05-13T05:00:00Z",
  ...over,
});
const page = (messages: ChatMessage[], over: Partial<ChatMessagesPage> = {}): ChatMessagesPage => ({
  messages,
  limit: 50,
  has_more: false,
  next_cursor: null,
  ...over,
});

const newClient = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });
const entities = (qc: QueryClient) =>
  qc.getQueryData<EntityTable<ChatMessage>>(chatKeys.messageEntities());
const idLists = (qc: QueryClient) => qc.getQueryData<ChatMessageIds>(chatKeys.messageIds(sid));
const pageIds = (qc: QueryClient) =>
  qc
    .getQueryData<InfiniteData<ChatMessagesPage>>(chatKeys.messagesPage(sid))
    ?.pages.map((p) => p.messages.map((m) => m.id));
const flatIds = (qc: QueryClient) =>
  qc.getQueryData<ChatMessage[]>(chatKeys.messages(sid))?.map((m) => m.id);

/** Fetch the newest page, then (when asked) one older page, the way the chat surface does. */
async function fetchPages(qc: QueryClient, withOlder = false) {
  const options = { ...chatMessagesPageOptions(sid), staleTime: 0 };
  await qc.fetchInfiniteQuery(withOlder ? { ...options, pages: 2 } : options);
}

beforeEach(() => {
  listChatMessagesPage.mockReset();
  listChatMessages.mockReset();
});

describe("by-id chat message store", () => {
  it("upserts a new id into the store, the id list and the derived caches", async () => {
    listChatMessagesPage.mockResolvedValue(page([msg("m-1")]));
    listChatMessages.mockResolvedValue([msg("m-1")]);
    const qc = newClient();
    await fetchPages(qc);
    await qc.fetchQuery(chatMessagesOptions(sid));

    upsertChatMessageToCaches(qc, sid, msg("m-2"));

    expect(Object.keys(entities(qc)!.byId)).toEqual(["m-1", "m-2"]);
    expect(idLists(qc)).toEqual({ flat: ["m-1", "m-2"], byPage: { first: ["m-1", "m-2"] } });
    expect(pageIds(qc)).toEqual([["m-1", "m-2"]]);
    expect(flatIds(qc)).toEqual(["m-1", "m-2"]);
  });

  it("converges to one row when a send response and its WebSocket echo race, in either order", () => {
    const send = msg("m-1", { attachments: [{ id: "att" } as never] });
    const echo = msg("m-1");
    for (const order of [[send, echo], [echo, send]]) {
      const qc = newClient();
      upsertChatMessageToCaches(qc, sid, order[0]!, { seedIfMissing: true });
      upsertChatMessageToCaches(qc, sid, order[1]!, { seedIfMissing: true });

      expect(pageIds(qc)).toEqual([["m-1"]]);
      expect(flatIds(qc)).toEqual(["m-1"]);
      expect(entities(qc)!.byId["m-1"]!.attachments).toHaveLength(1);
    }
  });

  it("converges when an HTTP page and a WebSocket upsert carry the same message, in either order", async () => {
    const richer = msg("m-1", { elapsed_ms: 5 });
    // HTTP first, then WS: the stored row wins what it defines, the echo fills nothing.
    listChatMessagesPage.mockResolvedValue(page([richer]));
    const httpFirst = newClient();
    await fetchPages(httpFirst);
    upsertChatMessageToCaches(httpFirst, sid, msg("m-1", { elapsed_ms: 999 }));
    expect(pageIds(httpFirst)).toEqual([["m-1"]]);
    expect(entities(httpFirst)!.byId["m-1"]!.elapsed_ms).toBe(5);

    // WS first (cache seeded by a send), then the HTTP page: one row, snapshot fields applied,
    // field only the store holds (attachments) survives.
    const wsFirst = newClient();
    upsertChatMessageToCaches(wsFirst, sid, msg("m-1", { attachments: [{ id: "a" } as never] }), {
      seedIfMissing: true,
    });
    await wsFirst.invalidateQueries({ queryKey: chatKeys.messagesPage(sid) });
    await fetchPages(wsFirst);
    expect(pageIds(wsFirst)).toEqual([["m-1"]]);
    expect(entities(wsFirst)!.byId["m-1"]).toMatchObject({ elapsed_ms: 5, attachments: [{ id: "a" }] });
  });

  it("applies the merge rule on realtime writes: existing wins, incoming fills gaps", () => {
    const qc = newClient();
    upsertChatMessageToCaches(qc, sid, msg("m-1", { content: "kept", elapsed_ms: 1 }), {
      seedIfMissing: true,
    });
    upsertChatMessageToCaches(qc, sid, msg("m-1", { content: "lost", task_id: undefined as never }));
    expect(entities(qc)!.byId["m-1"]).toMatchObject({ content: "kept", elapsed_ms: 1 });
  });

  it("keeps order: an appended message goes to the newest page, an older page is added after it", async () => {
    listChatMessagesPage.mockImplementation(async (_s: string, { before }: { before?: unknown }) =>
      before
        ? page([msg("m-1"), msg("m-2")])
        : page([msg("m-3"), msg("m-4")], { has_more: true, next_cursor: cursor }),
    );
    const qc = newClient();
    await fetchPages(qc, true);
    expect(pageIds(qc)).toEqual([["m-3", "m-4"], ["m-1", "m-2"]]);

    upsertChatMessageToCaches(qc, sid, msg("m-5"));
    upsertChatMessageToCaches(qc, sid, msg("m-5"));

    expect(pageIds(qc)).toEqual([["m-3", "m-4", "m-5"], ["m-1", "m-2"]]);
    expect(Object.keys(idLists(qc)!.byPage)).toHaveLength(2);
  });

  it("never shows an id twice across pages", () => {
    const data: InfiniteData<ChatMessagesPage> = {
      pages: [page([msg("m-2")]), page([msg("m-1"), msg("m-2")])],
      pageParams: [null, cursor],
    };
    const store: EntityTable<ChatMessage> = { byId: { "m-1": msg("m-1"), "m-2": msg("m-2") } };
    const lists: ChatMessageIds = {
      byPage: { first: ["m-2"], [`${cursor.created_at}|${cursor.id}`]: ["m-1", "m-2"] },
    };
    const derived = selectChatMessagePages(store, lists, data);
    expect(derived.pages.map((p) => p.messages.map((m) => m.id))).toEqual([["m-2"], ["m-1"]]);
    expect(derived.pages[1]).toMatchObject({ limit: 50, has_more: false });
  });

  it("the derived selector keeps the page reference when rows did not move", () => {
    const rows = [msg("m-1")];
    const data: InfiniteData<ChatMessagesPage> = { pages: [page(rows)], pageParams: [null] };
    const store: EntityTable<ChatMessage> = { byId: { "m-1": rows[0]! } };
    expect(selectChatMessagePages(store, { byPage: { first: ["m-1"] } }, data)).toBe(data);
  });

  it("an HTTP refetch refreshes a stored row (decided card) without losing store-only fields", async () => {
    listChatMessagesPage.mockResolvedValueOnce(page([msg("m-1", { content: "old" })]));
    const qc = newClient();
    await fetchPages(qc);
    upsertChatMessageToCaches(qc, sid, msg("m-1", { attachments: [{ id: "a" }] as never }));
    listChatMessagesPage.mockResolvedValueOnce(page([msg("m-1", { content: "new" })]));
    await fetchPages(qc);
    expect(entities(qc)!.byId["m-1"]).toMatchObject({ content: "new", attachments: [{ id: "a" }] });
    expect(qc.getQueryData<InfiniteData<ChatMessagesPage>>(chatKeys.messagesPage(sid))!.pages[0]!
      .messages[0]!.content).toBe("new");
  });

  it("ignores a response whose request was aborted, so it cannot undo a newer write", async () => {
    const qc = newClient();
    let resolve!: (p: ChatMessagesPage) => void;
    listChatMessagesPage.mockReturnValue(new Promise<ChatMessagesPage>((r) => (resolve = r)));
    const controller = new AbortController();
    const options = chatMessagesPageOptions(sid);
    const inflight = (options.queryFn as (ctx: unknown) => Promise<ChatMessagesPage>)({
      client: qc,
      pageParam: null,
      signal: controller.signal,
    });
    controller.abort();
    resolve(page([msg("m-1", { content: "stale" })]));
    await inflight;
    expect(entities(qc)).toBeUndefined();
  });

  it("patches and removes through the store", async () => {
    listChatMessagesPage.mockResolvedValue(page([msg("m-1"), msg("m-2")]));
    listChatMessages.mockResolvedValue([msg("m-1"), msg("m-2")]);
    const qc = newClient();
    await fetchPages(qc);
    await qc.fetchQuery(chatMessagesOptions(sid));

    patchChatMessageInCaches(qc, sid, "m-2", (m) => ({ ...m, quick_actions: [] as never }));
    expect(entities(qc)!.byId["m-2"]!.quick_actions).toEqual([]);
    expect(
      qc.getQueryData<InfiniteData<ChatMessagesPage>>(chatKeys.messagesPage(sid))!.pages[0]!.messages[1]!
        .quick_actions,
    ).toEqual([]);

    removeChatMessageFromCaches(qc, sid, "m-1");
    expect(entities(qc)!.byId["m-1"]).toBeUndefined();
    expect(pageIds(qc)).toEqual([["m-2"]]);
    expect(flatIds(qc)).toEqual(["m-2"]);
  });

  it("adopts a cache that was populated before the store existed", () => {
    const qc = newClient();
    qc.setQueryData<InfiniteData<ChatMessagesPage>>(chatKeys.messagesPage(sid), {
      pages: [page([msg("m-1")])],
      pageParams: [null],
    });
    upsertChatMessageToCaches(qc, sid, msg("m-2"));
    expect(pageIds(qc)).toEqual([["m-1", "m-2"]]);
    expect(Object.keys(entities(qc)!.byId)).toEqual(["m-1", "m-2"]);
  });

  it("leaves no trace for a session this client never opened", () => {
    const qc = newClient();
    upsertChatMessageToCaches(qc, sid, msg("m-1"));
    expect(entities(qc)).toBeUndefined();
    expect(idLists(qc)).toBeUndefined();
  });
});

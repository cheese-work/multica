import type { ReactNode } from "react";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createChatStore, registerChatStore, useChatStore } from "@multica/core/chat";
import { chatKeys } from "@multica/core/chat/queries";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatForkResponse, ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";
import { useChatFork } from "./use-chat-fork";

const mocks = vi.hoisted(() => ({ fork: vi.fn() }));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...await importOriginal<typeof import("@multica/core/api")>(),
  api: { forkChatSession: mocks.fork },
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

const { ApiError } = await import("@multica/core/api");

const source = { id: "source-1", title: "Source" } as ChatSession;
const child = { id: "child-1", title: "", forked_from_session_id: "source-1" } as ChatSession;
const response = (withMessage: boolean): ChatForkResponse => ({
  session: child,
  previous_session_id: "source-1",
  url: "/acme/chat?session=child-1",
  message: withMessage ? { id: "m1" } as ChatForkResponse["message"] : null,
});

function setup(current: ChatSession | null = source) {
  const onOpenSession = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  client.setQueryData(chatKeys.sessions("ws-1"), [source]);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>{children}</I18nProvider>
    </QueryClientProvider>
  );
  const view = renderHook(({ session }) => useChatFork(session, onOpenSession), {
    wrapper,
    initialProps: { session: current },
  });
  return { client, onOpenSession, ...view };
}

describe("useChatFork", () => {
  beforeEach(() => {
    mocks.fork.mockReset();
    registerChatStore(createChatStore({ storage: localStorage }));
    useChatStore.setState({ activeSessionId: "source-1" });
  });

  it("posts the key and message, adds the child to the DM list and opens it", async () => {
    mocks.fork.mockResolvedValue(response(true));
    const { result, client, onOpenSession } = setup();
    let outcome: unknown;
    await act(async () => { outcome = await result.current.handleFork("take it from here"); });

    expect(outcome).toEqual({ ok: true });
    expect(mocks.fork).toHaveBeenCalledTimes(1);
    const [id, body] = mocks.fork.mock.calls[0]!;
    expect(id).toBe("source-1");
    expect(body.message).toBe("take it from here");
    expect(body.idempotency_key).toEqual(expect.any(String));
    expect(new TextEncoder().encode(body.idempotency_key).length).toBeLessThanOrEqual(128);
    expect(client.getQueryData<ChatSession[]>(chatKeys.sessions("ws-1"))?.map((s) => s.id)).toContain("child-1");
    expect(onOpenSession).toHaveBeenCalledWith(child);
  });

  it("sends the attachment ids and keeps the key only for the same attachment set", async () => {
    mocks.fork.mockRejectedValue(new Error("offline"));
    const { result } = setup();
    await act(async () => { await result.current.handleFork("look", ["att-1"]); });
    await act(async () => { await result.current.handleFork("look", ["att-1"]); });
    await act(async () => { await result.current.handleFork("look", ["att-1", "att-2"]); });

    const [first, retry, changed] = mocks.fork.mock.calls.map(([, body]) => body);
    expect(first.attachment_ids).toEqual(["att-1"]);
    expect(retry.idempotency_key).toBe(first.idempotency_key);
    expect(changed.attachment_ids).toEqual(["att-1", "att-2"]);
    expect(changed.idempotency_key).not.toBe(first.idempotency_key);
  });

  it("opens the idle child when no message was given", async () => {
    mocks.fork.mockResolvedValue(response(false));
    const { result, client, onOpenSession } = setup();
    await act(async () => { expect(await result.current.handleFork("")).toEqual({ ok: true }); });

    expect(mocks.fork.mock.calls[0]![1]).not.toHaveProperty("message");
    expect(client.getQueryData<ChatSession[]>(chatKeys.sessions("ws-1"))?.map((s) => s.id)).toContain("child-1");
    expect(onOpenSession).toHaveBeenCalledWith(expect.objectContaining({ id: "child-1" }));
  });

  it("does not hijack a DM the person moved to while a fork without a message was in flight", async () => {
    mocks.fork.mockImplementation(async () => {
      useChatStore.setState({ activeSessionId: "other-dm" });
      return response(false);
    });
    const { result, onOpenSession } = setup();
    await act(async () => { await result.current.handleFork(""); });
    expect(onOpenSession).not.toHaveBeenCalled();
  });

  it("does not hijack a DM the person moved to while the fork was in flight", async () => {
    mocks.fork.mockImplementation(async () => {
      useChatStore.setState({ activeSessionId: "other-dm" });
      return response(true);
    });
    const { result, onOpenSession } = setup();
    await act(async () => { await result.current.handleFork("hi"); });
    expect(onOpenSession).not.toHaveBeenCalled();
  });

  it("shows the server message for a 4xx", async () => {
    mocks.fork.mockRejectedValue(new ApiError("nothing to fork yet", 400, "Bad Request"));
    const { result, onOpenSession } = setup();
    await act(async () => {
      expect(await result.current.handleFork("")).toEqual({ ok: false, error: "nothing to fork yet" });
    });
    expect(onOpenSession).not.toHaveBeenCalled();
  });

  it.each([
    ["a network error", new TypeError("Failed to fetch")],
    ["a 5xx", new ApiError("pq: duplicate key internal detail", 503, "Service Unavailable")],
  ])("asks the person to check the Forked into links after %s", async (_name, error) => {
    mocks.fork.mockRejectedValue(error);
    const { result } = setup();
    await act(async () => {
      expect(await result.current.handleFork("hi")).toEqual({
        ok: false,
        error: "Fork could not be confirmed. Check the Forked into links before trying again.",
      });
    });
  });

  it("treats an unreadable success body as unconfirmed", async () => {
    mocks.fork.mockResolvedValue({ session: { id: "" }, previous_session_id: "", url: "" });
    const { result, onOpenSession } = setup();
    await act(async () => { expect((await result.current.handleFork("hi")).ok).toBe(false); });
    expect(onOpenSession).not.toHaveBeenCalled();
  });

  it("reuses the key for a retry of the same text and mints a new one for edited text", async () => {
    mocks.fork.mockRejectedValue(new TypeError("Failed to fetch"));
    const { result } = setup();
    await act(async () => { await result.current.handleFork("hi"); });
    await act(async () => { await result.current.handleFork("hi"); });
    await act(async () => { await result.current.handleFork("hi there"); });
    const keys = mocks.fork.mock.calls.map((call) => call[1].idempotency_key);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).not.toBe(keys[0]);
  });

  it("retires the key after a success so the same text forks again", async () => {
    mocks.fork.mockResolvedValue(response(false));
    const { result } = setup();
    await act(async () => { await result.current.handleFork("hi"); });
    await act(async () => { await result.current.handleFork("hi"); });
    const keys = mocks.fork.mock.calls.map((call) => call[1].idempotency_key);
    expect(keys[1]).not.toBe(keys[0]);
  });

  it("does not share a key between DMs", async () => {
    mocks.fork.mockRejectedValue(new TypeError("Failed to fetch"));
    const { result, rerender } = setup();
    await act(async () => { await result.current.handleFork("hi"); });
    rerender({ session: { id: "source-2", title: "Other" } as ChatSession });
    await act(async () => { await result.current.handleFork("hi"); });
    const keys = mocks.fork.mock.calls.map((call) => call[1].idempotency_key);
    expect(keys[1]).not.toBe(keys[0]);
  });
});

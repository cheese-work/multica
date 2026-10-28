import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { forwardRef, useImperativeHandle, type ReactElement } from "react";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { createAuthStore, registerAuthStore } from "@multica/core/auth";
import type { ApiClient } from "@multica/core/api";
import type { ChatMessage, User } from "@multica/core/types";
import { NavigationProvider } from "../../navigation";
import enChat from "../../locales/en/chat.json";
import enEditor from "../../locales/en/editor.json";

const scrollToIndex = vi.fn();

// Real Virtuoso renders no rows under jsdom; this stub keeps what the button
// reads: `data-item-index` on every row and the imperative scrollToIndex.
vi.mock("react-virtuoso", () => ({
  Virtuoso: forwardRef(function VirtuosoStub(
    { data, itemContent, firstItemIndex = 0 }: { data: unknown[]; itemContent: (i: number, item: unknown) => ReactElement; firstItemIndex?: number },
    ref,
  ) {
    useImperativeHandle(ref, () => ({ scrollToIndex }));
    return (
      <div>
        {data.map((item, i) => (
          <div key={i} data-item-index={firstItemIndex + i}>
            {itemContent(firstItemIndex + i, item)}
          </div>
        ))}
      </div>
    );
  }),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
vi.mock("@multica/core/workspace/hooks", () => ({ useActorName: () => ({ getActorName: () => "Terra" }) }));

import { ChatMessageList } from "./chat-message-list";

const VIEWER = "f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff";
const SESSION = "22222222-2222-4222-8222-222222222222";

function message(i: number, content: string): ChatMessage {
  return {
    id: `m${i}`, chat_session_id: SESSION, role: "assistant", content,
    task_id: null, created_at: `2026-10-13T00:0${i}:00Z`,
  } as ChatMessage;
}

// jsdom has no layout: the scroller spans 0..100px, rows 0-1 are on screen and
// the rest sit below.
function stubLayout() {
  vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
    const index = this.getAttribute("data-item-index");
    if (index === null) return { top: 0, bottom: 100 } as DOMRect;
    const top = Number(index) * 50;
    return { top, bottom: top + 40 } as DOMRect;
  });
}

function renderList(messages: ChatMessage[]) {
  return render(
    <I18nProvider locale="en" resources={{ en: { chat: enChat, editor: enEditor } }}>
      <WorkspaceSlugProvider slug="acme">
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/chat", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path) => path }}>
          <QueryClientProvider client={new QueryClient()}>
            <ChatMessageList messages={messages} pendingTask={null} availability="online" />
          </QueryClientProvider>
        </NavigationProvider>
      </WorkspaceSlugProvider>
    </I18nProvider>,
  );
}

function signIn() {
  const store = createAuthStore({
    api: {} as ApiClient,
    storage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
  });
  store.setState({ user: { id: VIEWER } as User });
  registerAuthStore(store);
}

describe("ChatMessageList next self-mention", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    scrollToIndex.mockReset();
  });

  it("jumps to the next DM message that mentions the viewer, with a count", async () => {
    signIn();
    stubLayout();
    renderList([
      message(0, "hello"),
      message(1, "hello again"),
      message(2, `ping [@Me](mention://member/${VIEWER})`),
      message(3, "plain"),
      message(4, "everyone [@all](mention://all/all)"),
    ]);
    const button = await screen.findByRole("button", { name: /next mention of me/i });
    expect(screen.getByTestId("next-self-mention-count")).toHaveTextContent("2");
    fireEvent.click(button);
    expect(scrollToIndex).toHaveBeenCalledWith(expect.objectContaining({ index: 2, align: "start" }));
  });

  it("shows no button when no message below mentions the viewer", async () => {
    signIn();
    stubLayout();
    renderList([message(0, "hello"), message(1, "again"), message(2, "plain")]);
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByRole("button", { name: /next mention of me/i })).toBeNull();
  });
});

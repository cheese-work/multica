import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatMessage } from "@multica/core/types";
import type { ReactElement } from "react";
import enChat from "../../locales/en/chat.json";
import {
  ScrollRestorationProvider,
  type ScrollRestorationAdapter,
} from "../../platform";
import { ChatMessageList } from "./chat-message-list";

// Reading-position restore for a DM: the list saves a view anchor
// ({ itemId, offsetInItemPx, atLiveEnd }) under `chat::<sessionId>` and a
// remount (back navigation, session switch) opens on that item instead of the
// newest message. The stub surfaces the props Virtuoso is mounted with and
// lets a test drive the callbacks the real list reports through.

interface StubProps {
  initialTopMostItemIndex?: unknown;
  rangeChanged?: (range: { startIndex: number; endIndex: number }) => void;
  atBottomStateChange?: (atBottom: boolean) => void;
}
let virtuosoProps: StubProps = {};

vi.mock("react-virtuoso", () => ({
  Virtuoso: (props: {
    ref?: { current: unknown } | ((handle: unknown) => void);
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
  } & StubProps) => {
    virtuosoProps = props;
    const handle = { scrollToIndex: () => {} };
    if (typeof props.ref === "function") props.ref(handle);
    else if (props.ref) props.ref.current = handle;
    return (
      <div data-testid="virtuoso-rows">
        {props.data.map((item, i) => (
          <div key={props.computeItemKey(i, item)}>{props.itemContent(i, item)}</div>
        ))}
      </div>
    );
  },
}));

const TEST_RESOURCES = { en: { chat: enChat } };

function message(id: string, i: number, sessionId: string): ChatMessage {
  return {
    id,
    chat_session_id: sessionId,
    role: i % 2 ? "assistant" : "user",
    content: `body ${id}`,
    task_id: null,
    created_at: `2026-10-0${1 + (i % 5)}T10:00:00Z`,
  };
}

const IDS = ["m1", "m2", "m3", "m4", "m5", "m6"];
const messagesFor = (sessionId: string, ids = IDS) =>
  ids.map((id, i) => message(id, i, sessionId));

let store: Map<string, string>;
const adapter: ScrollRestorationAdapter = {
  get: () => undefined,
  getViewState: (key) => store.get(key),
  setViewState: (key, value) => {
    if (value === undefined) store.delete(key);
    else store.set(key, value);
  },
};

function mount(sessionId: string, messages: ChatMessage[]) {
  const qc = new QueryClient();
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={qc}>
        <ScrollRestorationProvider adapter={adapter}>
          <ChatMessageList
            key={sessionId}
            sessionId={sessionId}
            messages={messages}
            pendingTask={null}
            availability={undefined}
          />
        </ScrollRestorationProvider>
      </QueryClientProvider>
    </I18nProvider>,
  );
}

const saveAnchor = (sessionId: string, anchor: object) =>
  store.set(`chat::${sessionId}`, JSON.stringify(anchor));
const savedAnchor = (sessionId: string) => {
  const raw = store.get(`chat::${sessionId}`);
  return raw === undefined ? undefined : JSON.parse(raw);
};

beforeEach(() => {
  store = new Map();
  virtuosoProps = {};
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("ChatMessageList reading-position restore", () => {
  it("opens on the saved anchor item instead of the newest message", () => {
    saveAnchor("s1", { itemId: "m3", offsetInItemPx: 20, atLiveEnd: false });
    const view = mount("s1", messagesFor("s1"));

    expect(virtuosoProps.initialTopMostItemIndex).toEqual({
      index: 2,
      align: "start",
      offset: -20,
    });
    // Shown straight away: there is no live end to wait for.
    const root = view.container.querySelector("[data-tab-scroll-root]");
    expect(root?.className).not.toContain("invisible");
  });

  it("starts at the newest message when the reader was at the live end", () => {
    saveAnchor("s1", { itemId: "m3", offsetInItemPx: 0, atLiveEnd: true });
    mount("s1", messagesFor("s1"));

    expect(virtuosoProps.initialTopMostItemIndex).toEqual({ index: "LAST", align: "end" });
  });

  it("starts at the newest message when nothing was saved", () => {
    mount("s1", messagesFor("s1"));

    expect(virtuosoProps.initialTopMostItemIndex).toEqual({ index: "LAST", align: "end" });
  });

  it("starts at the newest message when the anchor item is not loaded", () => {
    saveAnchor("s1", { itemId: "gone", offsetInItemPx: 0, atLiveEnd: false });
    mount("s1", messagesFor("s1"));

    expect(virtuosoProps.initialTopMostItemIndex).toEqual({ index: "LAST", align: "end" });
  });

  it("starts at the newest message when the saved entry is malformed", () => {
    store.set("chat::s1", "not json");
    mount("s1", messagesFor("s1"));

    expect(virtuosoProps.initialTopMostItemIndex).toEqual({ index: "LAST", align: "end" });
  });

  it("keeps a separate anchor per session", () => {
    saveAnchor("s1", { itemId: "m4", offsetInItemPx: 0, atLiveEnd: false });
    mount("s2", messagesFor("s2"));

    expect(virtuosoProps.initialTopMostItemIndex).toEqual({ index: "LAST", align: "end" });
  });

  it("records the top visible item with its offset while the reader scrolls", () => {
    saveAnchor("s1", { itemId: "m3", offsetInItemPx: 0, atLiveEnd: false });
    const view = mount("s1", messagesFor("s1"));
    const root = view.container.querySelector<HTMLElement>("[data-tab-scroll-root]")!;
    const row = view.container.querySelector<HTMLElement>('[data-message-id="m5"]')!;
    root.getBoundingClientRect = () => ({ top: 100, bottom: 700 }) as DOMRect;
    row.getBoundingClientRect = () => ({ top: 80, bottom: 180 }) as DOMRect;

    act(() => virtuosoProps.rangeChanged?.({ startIndex: 4, endIndex: 5 }));

    expect(savedAnchor("s1")).toEqual({ itemId: "m5", offsetInItemPx: 20, atLiveEnd: false });
  });

  it("keeps the offset current while the reader scrolls inside a tall reply", () => {
    saveAnchor("s1", { itemId: "m3", offsetInItemPx: 0, atLiveEnd: false });
    const view = mount("s1", messagesFor("s1"));
    const root = view.container.querySelector<HTMLElement>("[data-tab-scroll-root]")!;
    const row = view.container.querySelector<HTMLElement>('[data-message-id="m5"]')!;
    root.getBoundingClientRect = () => ({ top: 100, bottom: 700 }) as DOMRect;
    row.getBoundingClientRect = () => ({ top: 80, bottom: 1500 }) as DOMRect;
    act(() => virtuosoProps.rangeChanged?.({ startIndex: 4, endIndex: 5 }));
    expect(savedAnchor("s1")?.offsetInItemPx).toBe(20);

    // 700 px further down the same tall row: the range endpoints do not change,
    // so Virtuoso reports nothing; only the scroll event carries the new offset.
    row.getBoundingClientRect = () => ({ top: -620, bottom: 800 }) as DOMRect;
    act(() => {
      root.dispatchEvent(new Event("scroll"));
    });

    expect(savedAnchor("s1")).toEqual({ itemId: "m5", offsetInItemPx: 720, atLiveEnd: false });
  });

  it("records atLiveEnd when the reader returns to the bottom", () => {
    saveAnchor("s1", { itemId: "m3", offsetInItemPx: 0, atLiveEnd: false });
    mount("s1", messagesFor("s1"));

    act(() => virtuosoProps.rangeChanged?.({ startIndex: 4, endIndex: 5 }));
    act(() => virtuosoProps.atBottomStateChange?.(true));

    expect(savedAnchor("s1")?.atLiveEnd).toBe(true);
  });

  it("does not overwrite the saved anchor before the list has landed", () => {
    saveAnchor("s1", { itemId: "m3", offsetInItemPx: 0, atLiveEnd: false });
    store.set("chat::s2", JSON.stringify({ itemId: "m2", offsetInItemPx: 0, atLiveEnd: false }));
    // A fresh session with no anchor is still hidden, waiting for the live end:
    // the range Virtuoso reports on its way there is not the reader's position.
    mount("s3", messagesFor("s3"));

    act(() => virtuosoProps.rangeChanged?.({ startIndex: 0, endIndex: 1 }));

    expect(savedAnchor("s3")).toBeUndefined();
  });
});

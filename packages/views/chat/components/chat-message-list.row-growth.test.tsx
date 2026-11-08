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

// WebKit has no scroll anchoring, so a restored DM shifts scrollTop itself when
// a row above the viewport grows. Growth in the first second after the list
// lands is held back (Virtuoso is still placing rows) and applied once it
// settles; growth after that, or after the reader's first input, applies at once.

vi.mock("react-virtuoso", () => ({
  Virtuoso: (props: {
    ref?: { current: unknown } | ((handle: unknown) => void);
    data: unknown[];
    itemContent: (i: number, item: unknown) => ReactElement;
    computeItemKey: (i: number, item: unknown) => string;
  }) => {
    const handle = { scrollToIndex: () => {} };
    if (typeof props.ref === "function") props.ref(handle);
    else if (props.ref) props.ref.current = handle;
    return (
      <div>
        {props.data.map((item, i) => (
          <div key={props.computeItemKey(i, item)}>{props.itemContent(i, item)}</div>
        ))}
      </div>
    );
  },
}));

const TEST_RESOURCES = { en: { chat: enChat } };
const IDS = ["m1", "m2", "m3", "m4", "m5", "m6"];
const VIEWPORT_TOP = 100;

let observed: Element[] = [];
let callbacks: ((entries: unknown[]) => void)[] = [];
let store: Map<string, string>;
const adapter: ScrollRestorationAdapter = {
  get: () => undefined,
  getViewState: (key) => store.get(key),
  setViewState: (key, value) => {
    if (value === undefined) store.delete(key);
    else store.set(key, value);
  },
};

beforeEach(() => {
  vi.useFakeTimers();
  observed = [];
  callbacks = [];
  store = new Map([
    ["chat::s1", JSON.stringify({ itemId: "m3", offsetInItemPx: 0, atLiveEnd: false })],
  ]);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      constructor(cb: (entries: unknown[]) => void) {
        callbacks.push(cb);
      }
      observe(target: Element) {
        observed.push(target);
      }
      unobserve() {}
      disconnect() {}
    },
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

function setup() {
  const messages: ChatMessage[] = IDS.map((id, i) => ({
    id,
    chat_session_id: "s1",
    role: i % 2 ? "assistant" : "user",
    content: `body ${id}`,
    task_id: null,
    created_at: "2026-10-01T10:00:00Z",
  }));
  const view = render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={new QueryClient()}>
        <ScrollRestorationProvider adapter={adapter}>
          <ChatMessageList sessionId="s1" messages={messages} pendingTask={null} availability={undefined} />
        </ScrollRestorationProvider>
      </QueryClientProvider>
    </I18nProvider>,
  );
  const root = view.container.querySelector<HTMLElement>("[data-tab-scroll-root]")!;
  root.getBoundingClientRect = () => ({ top: VIEWPORT_TOP }) as DOMRect;
  Object.defineProperty(root, "scrollTop", { value: 500, writable: true });
  // m1 and m2 sit above the viewport top, the rest are in view.
  const rows = IDS.map(
    (id) => observed.find((el) => el.getAttribute("data-message-id") === id)!,
  );
  rows.forEach((row, i) => {
    row.getBoundingClientRect = () => ({ top: i < 2 ? -200 + i * 100 : VIEWPORT_TOP + i * 100 }) as DOMRect;
  });
  const resize = (i: number, height: number) =>
    act(() => {
      for (const cb of callbacks)
        cb([{ target: rows[i]!, borderBoxSize: [{ blockSize: height }] }]);
    });
  return { root, resize };
}

describe("ChatMessageList row growth above the viewport", () => {
  it("applies growth seen while the restored list was still landing once it settles", () => {
    const { root, resize } = setup();
    for (let i = 0; i < IDS.length; i++) resize(i, 100);
    vi.advanceTimersByTime(500);

    resize(0, 180); // an image above loads mid-landing
    expect(root.scrollTop).toBe(500); // held back: Virtuoso is still placing rows

    vi.advanceTimersByTime(1000);
    expect(root.scrollTop).toBe(580);
  });

  it("restarts the settle window while new rows keep arriving", () => {
    const { root, resize } = setup();
    resize(0, 100);
    vi.advanceTimersByTime(900);
    resize(1, 100); // another row lands just before the window closes
    resize(0, 180);
    vi.advanceTimersByTime(900);
    expect(root.scrollTop).toBe(500);

    vi.advanceTimersByTime(200);
    expect(root.scrollTop).toBe(580);
  });

  it("applies growth at once after the list has settled", () => {
    const { root, resize } = setup();
    for (let i = 0; i < IDS.length; i++) resize(i, 100);
    vi.advanceTimersByTime(1500);

    resize(1, 140);
    expect(root.scrollTop).toBe(540);
  });

  it("drops held-back growth when the reader acts first", () => {
    const { root, resize } = setup();
    for (let i = 0; i < IDS.length; i++) resize(i, 100);
    resize(0, 180);

    act(() => {
      root.dispatchEvent(new Event("wheel"));
    });
    vi.advanceTimersByTime(1500);

    expect(root.scrollTop).toBe(500);
  });

  it("ignores growth of a row at or below the viewport top", () => {
    const { root, resize } = setup();
    for (let i = 0; i < IDS.length; i++) resize(i, 100);
    resize(3, 180);
    vi.advanceTimersByTime(1500);

    expect(root.scrollTop).toBe(500);
  });
});

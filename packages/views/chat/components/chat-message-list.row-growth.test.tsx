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
// a row grows above the reader. The list keeps the row at the viewport top at
// its measured offset: growth above moves it back, growth below never does,
// and several rows growing in one batch correct once. Row geometry is modelled
// from the row heights, so a correction that is too big or too small shows.

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
const ROW = 100;
const START = 250; // scrollTop with m3 straddling the viewport top, 50 px above it

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
    ["chat::s1", JSON.stringify({ itemId: "m3", offsetInItemPx: 50, atLiveEnd: false })],
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
  delete (Range.prototype as { getBoundingClientRect?: unknown }).getBoundingClientRect;
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
  const heights = IDS.map(() => ROW);
  root.getBoundingClientRect = () => ({ top: VIEWPORT_TOP }) as DOMRect;
  Object.defineProperty(root, "scrollTop", { value: START, writable: true });
  const rows = IDS.map((id) => observed.find((el) => el.getAttribute("data-message-id") === id)!);
  const rowTop = (i: number) =>
    VIEWPORT_TOP - root.scrollTop + heights.slice(0, i).reduce((a, b) => a + b, 0);
  rows.forEach((row, i) => {
    row.getBoundingClientRect = () =>
      ({ top: rowTop(i), bottom: rowTop(i) + heights[i]!, height: heights[i]! }) as DOMRect;
  });
  // m3 holds two blocks: `above` is wholly above the viewport top, `marker` is
  // the visible text the reader is on. Row geometry stays in `heights`.
  // `marker` is a paragraph: plain `text` (what the reader sees) followed by a
  // `<strong>` that sits far below the viewport (what a descent must not pick).
  let aboveH = 30;
  let textH = heights[2]! - aboveH;
  const above = document.createElement("div");
  const marker = document.createElement("div");
  const text = document.createTextNode("visible text");
  const strong = document.createElement("strong");
  marker.append(text, strong);
  rows[2]!.append(above, marker);
  // jsdom has no Range geometry: the text node's box starts at the marker top.
  (Range.prototype as { getBoundingClientRect?: unknown }).getBoundingClientRect = function (this: Range) {
    const top = rowTop(2) + aboveH;
    return this.startContainer === text
      ? { top, bottom: top + textH, height: textH }
      : { top: 0, bottom: 0, height: 0 };
  };
  strong.getBoundingClientRect = () =>
    ({ top: rowTop(2) + aboveH + textH, bottom: rowTop(2) + aboveH + textH + 20, height: 20 }) as DOMRect;
  above.getBoundingClientRect = () =>
    ({ top: rowTop(2), bottom: rowTop(2) + aboveH, height: aboveH }) as DOMRect;
  marker.getBoundingClientRect = () =>
    ({ top: rowTop(2) + aboveH, bottom: rowTop(2) + heights[2]!, height: heights[2]! - aboveH }) as DOMRect;
  // Block `above` grows inside m3: the row grows with it, the marker moves down.
  const growAbove = (px: number) =>
    act(() => {
      aboveH += px;
      heights[2]! += px;
      for (const cb of callbacks)
        cb([{ target: rows[2]!, borderBoxSize: [{ blockSize: heights[2]! }] }]);
    });
  // Plain text below the reader grows: the later `<strong>` moves, the text not.
  const growText = (px: number) =>
    act(() => {
      textH += px;
      heights[2]! += px;
      for (const cb of callbacks)
        cb([{ target: rows[2]!, borderBoxSize: [{ blockSize: heights[2]! }] }]);
    });
  const markerOffset = () => marker.getBoundingClientRect().top - VIEWPORT_TOP;
  // Rows grow in place; the observers then report every resized row in one batch.
  const grow = (...by: [number, number][]) =>
    act(() => {
      for (const [i, px] of by) heights[i]! += px;
      for (const cb of callbacks)
        cb(by.map(([i]) => ({ target: rows[i]!, borderBoxSize: [{ blockSize: heights[i]! }] })));
    });
  // The first observation of every row, as the mounted list reports it.
  act(() => {
    for (const cb of callbacks)
      cb(rows.map((target, i) => ({ target, borderBoxSize: [{ blockSize: heights[i]! }] })));
  });
  // Distance of m3, the row at the viewport top, from the viewport top.
  const readingOffset = () => rowTop(2) - VIEWPORT_TOP;
  return { root, grow, growAbove, growText, markerOffset, readingOffset };
}

describe("ChatMessageList holds the reading point as rows resize", () => {
  it("moves back for a row wholly above the viewport that grows", () => {
    const { root, grow, readingOffset } = setup();
    grow([0, 80]);
    expect(readingOffset()).toBe(-50);
    expect(root.scrollTop).toBe(START + 80);
  });

  it("ignores a row wholly below the viewport top that grows", () => {
    const { root, grow, readingOffset } = setup();
    grow([4, 80]);
    expect(readingOffset()).toBe(-50);
    expect(root.scrollTop).toBe(START);
  });

  it("ignores growth below the reader inside a partly visible tall message", () => {
    const { root, grow, readingOffset } = setup();
    grow([2, 200]); // m3 straddles the viewport top and grows at its bottom
    expect(readingOffset()).toBe(-50);
    expect(root.scrollTop).toBe(START);
  });

  it("moves back for a block above the visible text that grows inside the same message", () => {
    const { root, growAbove, markerOffset } = setup();
    expect(markerOffset()).toBe(-20);
    growAbove(60);
    expect(markerOffset()).toBe(-20);
    expect(root.scrollTop).toBe(START + 60);
  });

  it("ignores growth of the visible text when a later inline element sits below the viewport", () => {
    const { root, growText, markerOffset } = setup();
    growText(400); // the `<strong>` moves down 400 px; the text the reader reads does not
    expect(markerOffset()).toBe(-20);
    expect(root.scrollTop).toBe(START);
  });

  it("corrects several rows resized in one batch once", () => {
    const { root, grow, readingOffset } = setup();
    grow([0, 80], [1, 40], [4, 60]);
    expect(readingOffset()).toBe(-50);
    expect(root.scrollTop).toBe(START + 120);
  });

  it("keeps holding across growth that arrives later", () => {
    const { root, grow, readingOffset } = setup();
    grow([1, 40]);
    grow([0, 80]);
    grow([3, 30]);
    expect(readingOffset()).toBe(-50);
    expect(root.scrollTop).toBe(START + 120);
  });

  it("lets the reader move away: no snap back to the restored message", () => {
    const { root, grow, readingOffset } = setup();
    act(() => {
      root.dispatchEvent(new Event("wheel"));
      root.scrollTop = 400; // reader drags down: m5 now straddles the top
      root.dispatchEvent(new Event("scroll"));
    });
    grow([0, 80]);
    expect(root.scrollTop).toBe(480); // m5's offset held, m3 is not chased
    expect(readingOffset()).toBe(-200); // m3 only moved with the growth
  });

  it("does not move a list that is following the live end", () => {
    store.clear();
    const { root, grow } = setup();
    grow([0, 80]);
    expect(root.scrollTop).toBe(START);
  });
});

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

// The window module's factory runs when the module is first imported.
const windowLoads = vi.hoisted(() => ({ count: 0 }));
const store = vi.hoisted(() => ({ floatingChatEnabled: true, isOpen: false }));

vi.mock("@multica/core/chat", () => ({
  useChatStore: (select: (s: typeof store) => unknown) => select(store),
}));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ chat: () => "/acme/chat" }),
}));
vi.mock("../navigation", () => ({ useNavigation: () => ({ pathname: "/acme/issues" }) }));
vi.mock("./components/chat-fab", () => ({ ChatFab: () => <div>fab</div> }));
vi.mock("./components/chat-window", () => {
  windowLoads.count += 1;
  return { ChatWindow: () => <div>window</div> };
});

import { FloatingChat } from "./floating-chat";

// jsdom has no idle callbacks; queue them so the test decides when the browser is idle.
const idle: Array<() => void> = [];

beforeEach(() => {
  idle.length = 0;
  vi.stubGlobal("requestIdleCallback", (cb: () => void) => idle.push(cb));
  vi.stubGlobal("cancelIdleCallback", () => {});
  store.floatingChatEnabled = true;
  store.isOpen = false;
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("FloatingChat", () => {
  it("shows the button without loading the chat window module", () => {
    render(<FloatingChat />);

    expect(screen.getByText("fab")).toBeTruthy();
    expect(screen.queryByText("window")).toBeNull();
    expect(windowLoads.count).toBe(0);
  });

  it("loads the chat window once the page is idle", async () => {
    render(<FloatingChat />);

    act(() => idle.forEach((run) => run()));

    expect(await screen.findByText("window")).toBeTruthy();
    expect(windowLoads.count).toBe(1);
  });
});

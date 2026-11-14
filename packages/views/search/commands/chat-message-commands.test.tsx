import type { ReactNode } from "react";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import { Command as CommandPrimitive } from "cmdk";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api/client";
import { RESOURCES } from "../../locales";
import { PaletteContext, type PaletteApi } from "../palette-context";
import { ChatMessageGroup, ChatMessageStatus, useChatMessageSearch } from "./chat-message-commands";

const { searchChats, sessions } = vi.hoisted(() => ({
  searchChats: vi.fn(),
  sessions: { current: [] as Array<{ id: string; status: string }> },
}));

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { searchChats },
}));
vi.mock("@multica/core/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => "ws-test",
}));
vi.mock("@multica/core/paths", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/paths")>();
  return { ...actual, useWorkspacePaths: () => actual.paths.workspace("ws-test") };
});
vi.mock("@multica/core/chat/queries", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/chat/queries")>();
  return {
    ...actual,
    chatSessionsOptions: (wsId: string) => ({
      queryKey: ["chat-sessions-fixture", wsId],
      queryFn: () => Promise.resolve(sessions.current),
    }),
  };
});

const hit = (over: Record<string, unknown> = {}) => ({
  session_id: "s-1",
  title: "sub2api budget",
  agent_id: "a-1",
  message_id: "m-1",
  snippet: "the november budget is capped",
  updated_at: "2026-11-13T07:15:00Z",
  archived: false,
  ...over,
});

const navigate = vi.fn();
const palette: PaletteApi = { close: vi.fn(), push: vi.fn(), navigate, openHostDialog: vi.fn() };

function Harness({ query }: { query: string }) {
  const search = useChatMessageSearch(query, true);
  return (
    <CommandPrimitive>
      <CommandPrimitive.List>
        <ChatMessageGroup search={search} query={query} />
        <ChatMessageStatus search={search} />
      </CommandPrimitive.List>
    </CommandPrimitive>
  );
}

function renderHarness(query: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The saved chat list is already cached when the connection drops.
  client.setQueryData(["chat-sessions-fixture", "ws-test"], sessions.current);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: RESOURCES.en }}>
        <PaletteContext.Provider value={palette}>{children}</PaletteContext.Provider>
      </I18nProvider>
    </QueryClientProvider>
  );
  return render(<Harness query={query} />, { wrapper });
}

beforeEach(() => {
  searchChats.mockReset();
  navigate.mockReset();
  sessions.current = Array.from({ length: 3 }, (_, i) => ({ id: `s-${i}`, status: "active" }));
});
afterEach(() => onlineManager.setOnline(true));

describe("⌘K messages group", () => {
  it("renders a content hit in the Messages group and opens the DM at that message", async () => {
    searchChats.mockResolvedValue({
      results: [hit(), hit({ session_id: "s-2", message_id: null, title: "budget notes" })],
      next_cursor: null,
    });
    renderHarness("budget");

    const row = await screen.findByText("sub2api budget");
    expect(screen.getByText("Messages")).toBeInTheDocument();
    // Title hits stay in the existing Chats group.
    expect(screen.queryByText("budget notes")).not.toBeInTheDocument();

    await userEvent.click(row);
    expect(navigate).toHaveBeenCalledWith("/ws-test/chat?session=s-1&message=m-1");
  });
});

describe("⌘K message search states", () => {
  it("shows the searching state while the request is in flight", async () => {
    searchChats.mockReturnValue(new Promise(() => {}));
    renderHarness("budget");
    expect(await screen.findByRole("status")).toHaveTextContent("Searching messages…");
  });

  it("shows the error copy and Retry refetches; Retry has a 2px focus ring", async () => {
    searchChats.mockRejectedValueOnce(new ApiError("boom", 503, "Service Unavailable"));
    searchChats.mockResolvedValueOnce({ results: [hit()], next_cursor: null });
    renderHarness("budget");

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't search message content");
    expect(alert).toHaveTextContent("took too long");
    const retry = screen.getByRole("button", { name: "Retry" });
    expect(retry.className).toContain("focus-visible:ring-2");

    await userEvent.click(retry);
    expect(await screen.findByText("sub2api budget")).toBeInTheDocument();
    expect(searchChats).toHaveBeenCalledTimes(2);
  });

  it("shows the offline state with the saved chat count when the request is paused", async () => {
    onlineManager.setOnline(false);
    searchChats.mockResolvedValue({ results: [], next_cursor: null });
    renderHarness("budget");
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("You're offline"));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("3 saved chats"));
    expect(searchChats).not.toHaveBeenCalled();
  });
});

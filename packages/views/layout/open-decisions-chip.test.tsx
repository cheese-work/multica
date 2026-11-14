// @vitest-environment jsdom

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { OpenDecisionRequest } from "@multica/core/types";
import enChat from "../locales/en/chat.json";
import { NavigationProvider } from "../navigation";

const listOpen = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useWorkspaceSlug: () => "test-ws",
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({ api: { listOpenDecisionRequests: listOpen } }));

import { OpenDecisionsChip } from "./open-decisions-chip";

const request = (id: string, overrides: Partial<OpenDecisionRequest> = {}): OpenDecisionRequest => ({
  id,
  issue_id: `issue-${id}`,
  issue_identifier: `CHE-${id}`,
  asking_agent_id: "agent-1",
  asking_agent_name: "Alpha",
  ask: "Pick one?",
  escalation_class: "reversible",
  created_at: "2026-11-11T10:00:00Z",
  chat_session_id: "session-1",
  message_id: null,
  source_comment_id: null,
  ...overrides,
});

function renderChip(pathname = "/test-ws") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname, searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
          <OpenDecisionsChip />
        </NavigationProvider>
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("OpenDecisionsChip", () => {
  it("shows nothing when no decision is open", async () => {
    listOpen.mockResolvedValue([]);
    renderChip();
    await Promise.resolve();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("counts open decisions and opens the oldest one at its card comment", async () => {
    listOpen.mockResolvedValue([
      request("2", { created_at: "2026-11-11T11:00:00Z" }),
      request("1", { source_comment_id: "comment-1" }),
    ]);
    renderChip();
    const link = await screen.findByRole("link", { name: /2 decisions need you/i });
    expect(link.getAttribute("href")).toBe("/test-ws/issues/CHE-1#comment-comment-1");
  });

  it("falls back to the issue when the card has no comment", async () => {
    listOpen.mockResolvedValue([request("1")]);
    renderChip();
    const link = await screen.findByRole("link", { name: /1 decision needs you/i });
    expect(link.getAttribute("href")).toBe("/test-ws/issues/CHE-1");
  });

  it("stays off the chat route, where the DM header shows the badge", async () => {
    listOpen.mockResolvedValue([request("1")]);
    renderChip("/test-ws/chat");
    await Promise.resolve();
    await Promise.resolve();
    expect(screen.queryByRole("link")).toBeNull();
  });
});

import type { ReactNode } from "react";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import type { OpenDecisionRequest } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const listOpen = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useWorkspaceSlug: () => "test-ws",
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({ api: { listOpenDecisionRequests: listOpen } }));
vi.mock("../../navigation", () => ({
  AppLink: ({ children, href, ...rest }: { children: ReactNode; href: string }) => (
    <a href={href} {...rest}>{children}</a>
  ),
}));

import { OpenDecisionsBadge } from "./open-decisions-badge";

const request = (id: string, overrides: Partial<OpenDecisionRequest> = {}): OpenDecisionRequest => ({
  id,
  issue_id: `issue-${id}`,
  issue_identifier: `CHE-${id}`,
  asking_agent_id: "agent-1",
  asking_agent_name: "Alpha",
  ask: `**Decision**\nPick option ${id}?`,
  escalation_class: "reversible",
  created_at: "2026-11-11T10:00:00Z",
  chat_session_id: `session-${id}`,
  message_id: null,
  source_comment_id: "c-1",
  ...overrides,
});

function renderBadge() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat } }}>{children}</I18nProvider>
    </QueryClientProvider>
  );
  return render(<OpenDecisionsBadge />, { wrapper });
}

describe("OpenDecisionsBadge", () => {
  beforeEach(() => listOpen.mockReset());

  it("renders nothing while no decision is open", async () => {
    listOpen.mockResolvedValue([]);
    const { container } = renderBadge();
    await Promise.resolve();
    expect(container).toBeEmptyDOMElement();
  });

  it("is a button, not a link, so a click cannot navigate away from the DM", async () => {
    listOpen.mockResolvedValue([request("1"), request("2")]);
    renderBadge();
    const badge = await screen.findByRole("button", { name: /2 decisions need you/i });
    expect(screen.queryByRole("link")).toBeNull();
    expect(badge).toBeInTheDocument();
  });

  it("opens a list of the decisions, each row linking to its DM message", async () => {
    listOpen.mockResolvedValue([
      request("1", { message_id: "m-1" }),
      request("2", { created_at: "2026-11-11T11:00:00Z" }),
    ]);
    renderBadge();
    fireEvent.click(await screen.findByRole("button", { name: /2 decisions need you/i }));
    const links = await screen.findAllByRole("link");
    expect(links.map((l) => l.getAttribute("href"))).toEqual([
      "/test-ws/chat?session=session-1&message=m-1",
      "/test-ws/chat?session=session-2",
    ]);
    expect(within(links[0]!).getByText("Pick option 1?")).toBeInTheDocument();
  });
});

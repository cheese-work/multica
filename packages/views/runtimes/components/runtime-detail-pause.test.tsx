// @vitest-environment jsdom

import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { AgentRuntime } from "@multica/core/types";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import enAgents from "../../locales/en/agents.json";

const listRuntimes = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (sel: (s: { user: { id: string } }) => unknown) =>
    sel({ user: { id: "user-1" } }),
}));
vi.mock("@multica/core/realtime", () => ({ useWSEvent: () => {} }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    runtimes: () => "/runtimes",
    runtimeSettings: () => "/runtimes/x",
  }),
}));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: {
    listRuntimes,
    listAgents: vi.fn().mockResolvedValue([]),
    getAgentTaskSnapshot: vi.fn().mockResolvedValue([]),
    listMembers: vi.fn().mockResolvedValue([]),
    listRuntimeProfiles: vi.fn().mockResolvedValue([]),
  },
}));
vi.mock("../../navigation", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../navigation")>()),
  AppLink: ({ children, href }: { children: React.ReactNode; href: string }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("./runtime-list", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./runtime-list")>()),
  RuntimeList: () => <div data-testid="runtime-list" />,
}));

import { RuntimeDetailPage } from "./runtime-detail-page";

const runtime: AgentRuntime = {
  id: "rt-1",
  workspace_id: "ws-1",
  daemon_id: "daemon-1",
  name: "Claude (x99)",
  runtime_mode: "local",
  provider: "claude",
  launch_header: "",
  status: "online",
  device_info: "x99",
  metadata: {},
  owner_id: "user-1",
  visibility: "private",
  last_seen_at: new Date().toISOString(),
  created_at: "",
  updated_at: "",
  pause: {
    reason: "incident",
    paused_by: null,
    paused_at: new Date().toISOString(),
    expires_at: new Date(Date.now() + 30 * 60_000).toISOString(),
  },
  daemon_running_tasks: 2,
};

describe("RuntimeDetailPage machine pause", () => {
  it("shows the Machine card with the live pause between the header and the runtimes", async () => {
    listRuntimes.mockResolvedValue([runtime]);
    render(
      <I18nProvider
        locale="en"
        resources={{ en: { common: enCommon, runtimes: enRuntimes, agents: enAgents } }}
      >
        <QueryClientProvider client={new QueryClient()}>
          <RuntimeDetailPage runtimeId="local:daemon-1" />
        </QueryClientProvider>
      </I18nProvider>,
    );

    const card = await screen.findByRole("region", { name: "Machine" });
    expect(within(card).getByText("Paused: incident")).toBeInTheDocument();
    expect(within(card).getByText(/Draining: 2 tasks left/)).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Resume" })).toBeEnabled();
    // Card sits above the runtime list.
    expect(
      card.compareDocumentPosition(screen.getByTestId("runtime-list")) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });
});

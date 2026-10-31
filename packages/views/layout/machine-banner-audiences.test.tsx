// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { useDismissedMachineEventsStore, useMachineBannerDetailsStore } from "@multica/core/runtimes";
import type { AffectedMachine, AgentRuntime } from "@multica/core/types";
import enRuntimes from "../locales/en/runtimes.json";
import enCommon from "../locales/en/common.json";

const listRuntimes = vi.hoisted(() => vi.fn());
const listAffected = vi.hoisted(() => vi.fn());
const listMembers = vi.hoisted(() => vi.fn());
const me = vi.hoisted(() => ({ id: "lead-1" }));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (sel: (s: { user: { id: string } }) => unknown) => sel({ user: me }),
}));
vi.mock("@multica/core/realtime", () => ({ useWSEvent: () => {} }));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: {
    listRuntimes,
    listAffectedMachines: listAffected,
    listMembers,
    resumeRuntimeMachine: vi.fn(),
  },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

import { MachineBannerHeaderSlot } from "./machine-banner-toggle";
import { MachinePauseBanner } from "./machine-pause-banner";
import { PageHeader } from "./page-header";

const MIN = 60_000;

function affected(overrides: Partial<AffectedMachine> = {}): AffectedMachine {
  return {
    machine_owner_id: "owner-1",
    daemon_id: "daemon-x99",
    device_name: "x99",
    state: "paused",
    reason: "incident",
    event_id: "paused:owner-1/daemon-x99:1",
    expires_at: new Date(Date.now() + 22 * MIN + 30_000).toISOString(),
    waiting_tasks: 2,
    ...overrides,
  };
}

function runtime(overrides: Partial<AgentRuntime> = {}): AgentRuntime {
  return {
    id: "rt-1",
    workspace_id: "ws-1",
    daemon_id: "daemon-x99",
    name: "Claude (x99)",
    runtime_mode: "local",
    provider: "claude",
    launch_header: "",
    status: "online",
    device_info: "x99",
    metadata: {},
    owner_id: "owner-1",
    visibility: "public",
    last_seen_at: new Date().toISOString(),
    created_at: "",
    updated_at: "",
    pause: null,
    ...overrides,
  };
}

function renderChrome() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return { client, ...render(
    <I18nProvider locale="en" resources={{ en: { runtimes: enRuntimes, common: enCommon } }}>
      <QueryClientProvider client={client}>
        <MachineBannerHeaderSlot>
          <MachinePauseBanner />
          <PageHeader>Issues</PageHeader>
        </MachineBannerHeaderSlot>
      </QueryClientProvider>
    </I18nProvider>,
  ) };
}

function givenServer(machines: AffectedMachine[], runtimes: AgentRuntime[] = [runtime()]) {
  listRuntimes.mockResolvedValue(runtimes);
  listAffected.mockResolvedValue(machines);
  listMembers.mockResolvedValue([
    { user_id: "owner-1", name: "Cheese", role: "member" },
    { user_id: "lead-1", name: "Dana", role: "member" },
  ]);
}

describe("machine banner audiences", () => {
  beforeEach(() => {
    me.id = "lead-1";
    listRuntimes.mockReset();
    listAffected.mockReset();
    listMembers.mockReset();
    useDismissedMachineEventsStore.setState({ dismissed: [] });
    useMachineBannerDetailsStore.setState({ open: false });
  });

  it("a project lead whose tasks wait sees the cause, the count and the machine, without Resume", async () => {
    givenServer([affected()]);
    renderChrome();
    const banner = await screen.findByRole("status");
    expect(banner).toHaveTextContent("x99 (Cheese's machine) is paused: incident.");
    expect(banner).toHaveTextContent("2 tasks wait for agents on it.");
    expect(banner).toHaveTextContent("Ends in 23 min.");
    expect(screen.queryByRole("button", { name: "Resume" })).not.toBeInTheDocument();
  });

  it("an offline machine reads as offline with its last-seen time", async () => {
    givenServer([affected({ state: "offline", reason: null, expires_at: null, event_id: "offline:1" })]);
    renderChrome();
    expect(await screen.findByRole("status")).toHaveTextContent("x99 (Cheese's machine) is offline.");
  });

  it("nothing renders while the affected query fails", async () => {
    givenServer([]);
    listAffected.mockRejectedValue(new Error("boom"));
    renderChrome();
    await waitFor(() => expect(listAffected).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /machine banner/ })).not.toBeInTheDocument();
  });

  it("the machine owner keeps Resume on their own machine", async () => {
    me.id = "owner-1";
    givenServer(
      [affected()],
      [
        runtime({
          pause: {
            reason: "incident",
            paused_by: "owner-1",
            paused_at: new Date(Date.now() - MIN).toISOString(),
            expires_at: new Date(Date.now() + 20 * MIN).toISOString(),
          },
          daemon_running_tasks: 1,
        }),
      ],
    );
    renderChrome();
    expect(await screen.findByRole("button", { name: "Resume" })).toBeInTheDocument();
    // The owner's machine appears once, not once from the runtime list and once from the query.
    expect(screen.getAllByRole("status")).toHaveLength(1);
  });

  it("several machines collapse into one summary; Details opens a row per machine", async () => {
    givenServer([
      affected(),
      affected({
        daemon_id: "daemon-mac",
        device_name: "mac-mini",
        state: "offline",
        reason: null,
        expires_at: null,
        event_id: "offline:2",
        waiting_tasks: 1,
      }),
    ]);
    renderChrome();
    const banner = await screen.findByRole("status");
    expect(banner).toHaveTextContent("2 machines need attention.");
    expect(banner).toHaveTextContent("3 tasks wait for agents on them.");
    fireEvent.click(screen.getByRole("button", { name: "Details" }));
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
  });

  it("close hides the banner, the header icon keeps the count, and the icon reopens it", async () => {
    givenServer([affected()]);
    renderChrome();
    fireEvent.click(await screen.findByRole("button", { name: "Hide machine banner" }));
    await waitFor(() => expect(screen.queryByRole("status")).not.toBeInTheDocument());

    const icon = screen.getByRole("button", { name: "Show machine banner: x99 is paused" });
    expect(icon).toHaveTextContent("1");
    expect(icon).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(icon);
    expect(await screen.findByRole("status")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Hide machine banner: x99 is paused" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });

  it("a new event after a dismiss shows the banner again with only that row tagged New", async () => {
    givenServer([affected()]);
    const { client } = renderChrome();
    fireEvent.click(await screen.findByRole("button", { name: "Hide machine banner" }));
    await waitFor(() => expect(screen.queryByRole("status")).not.toBeInTheDocument());

    listAffected.mockResolvedValue([
      affected(),
      affected({
        daemon_id: "daemon-mac",
        device_name: "mac-mini",
        state: "offline",
        reason: null,
        expires_at: null,
        event_id: "offline:9",
      }),
    ]);
    await client.invalidateQueries();
    const banner = await screen.findByRole("status");
    fireEvent.click(screen.getByRole("button", { name: "Details" }));
    const rows = screen.getAllByRole("listitem");
    expect(banner).toHaveTextContent("2 machines need attention.");
    expect(rows).toHaveLength(2);
    expect(rows.filter((row) => row.textContent?.includes("New"))).toHaveLength(1);
    expect(rows.find((row) => row.textContent?.includes("New"))).toHaveTextContent("mac-mini");
  });

  it("counts above 9 read 9+", async () => {
    givenServer(
      Array.from({ length: 11 }, (_, i) =>
        affected({ daemon_id: `d${i}`, device_name: `m${i}`, event_id: `paused:${i}` }),
      ),
    );
    renderChrome();
    expect(await screen.findByRole("button", { name: /^Hide machine banner: / })).toHaveTextContent("9+");
  });
});

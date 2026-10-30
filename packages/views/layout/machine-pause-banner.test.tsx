// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { AgentRuntime, RuntimePause } from "@multica/core/types";
import enRuntimes from "../locales/en/runtimes.json";
import enCommon from "../locales/en/common.json";

const listRuntimes = vi.hoisted(() => vi.fn());
const resumeMachine = vi.hoisted(() => vi.fn());
const me = vi.hoisted(() => ({ id: "user-1" }));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (sel: (s: { user: { id: string } }) => unknown) => sel({ user: me }),
}));
vi.mock("@multica/core/realtime", () => ({ useWSEvent: () => {} }));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { listRuntimes, resumeRuntimeMachine: resumeMachine },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

import { MachinePauseBanner } from "./machine-pause-banner";

const MIN = 60_000;

function pause(overrides: Partial<RuntimePause> = {}): RuntimePause {
  return {
    reason: "maintenance",
    paused_by: "user-1",
    paused_at: new Date(Date.now() - 12 * MIN).toISOString(),
    expires_at: new Date(Date.now() + 108 * MIN + 30_000).toISOString(),
    ...overrides,
  };
}

function runtime(overrides: Partial<AgentRuntime> = {}): AgentRuntime {
  return {
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
    pause: pause(),
    daemon_running_tasks: 3,
    ...overrides,
  };
}

function renderBanner(runtimes: AgentRuntime[]) {
  listRuntimes.mockResolvedValue(runtimes);
  return render(
    <I18nProvider locale="en" resources={{ en: { runtimes: enRuntimes, common: enCommon } }}>
      <QueryClientProvider client={new QueryClient({ defaultOptions: { mutations: { retry: false } } })}>
        <MachinePauseBanner />
      </QueryClientProvider>
    </I18nProvider>,
  );
}

describe("MachinePauseBanner", () => {
  beforeEach(() => {
    listRuntimes.mockReset();
    resumeMachine.mockReset();
    me.id = "user-1";
  });

  it("pausing: tells the machine owner what is paused, the tasks left and the time left", async () => {
    renderBanner([runtime()]);
    const banner = await screen.findByRole("status");
    expect(banner).toHaveTextContent("x99 is paused: maintenance.");
    expect(banner).toHaveTextContent("3 tasks left.");
    expect(banner).toHaveTextContent("Ends in 1 h 49 min.");
  });

  it("drained: says it is safe to shut down", async () => {
    renderBanner([runtime({ daemon_running_tasks: 0 })]);
    expect(await screen.findByRole("status")).toHaveTextContent("Safe to shut down.");
  });

  it("offline: marks the count as possibly stale instead of claiming a safe shutdown", async () => {
    renderBanner([runtime({ status: "offline", daemon_running_tasks: 0 })]);
    const banner = await screen.findByRole("status");
    expect(banner).toHaveTextContent("0 tasks left (offline; count may be stale).");
    expect(banner).not.toHaveTextContent("Safe to shut down");
  });

  it("ending soon: adds that the machine takes work again", async () => {
    renderBanner([
      runtime({ daemon_running_tasks: 0, pause: pause({ expires_at: new Date(Date.now() + 4 * MIN - 5_000).toISOString() }) }),
    ]);
    expect(await screen.findByRole("status")).toHaveTextContent("Ends in 4 min. x99 takes work again then.");
  });

  it("is hidden from a viewer who does not own the machine", async () => {
    me.id = "user-9";
    renderBanner([runtime()]);
    await waitFor(() => expect(listRuntimes).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("is hidden while the machine takes work", async () => {
    renderBanner([runtime({ pause: null })]);
    await waitFor(() => expect(listRuntimes).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("Resume lifts the pause and is busy while the request runs", async () => {
    resumeMachine.mockReturnValue(new Promise(() => {}));
    renderBanner([runtime()]);
    fireEvent.click(await screen.findByRole("button", { name: "Resume" }));
    await waitFor(() => expect(resumeMachine).toHaveBeenCalledWith("rt-1"));
    const busy = screen.getByRole("button", { name: "Resume" });
    await waitFor(() => expect(busy).toHaveAttribute("aria-busy", "true"));
    expect(busy).toBeDisabled();
  });
});

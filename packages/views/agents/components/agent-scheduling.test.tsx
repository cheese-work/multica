// @vitest-environment jsdom

import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enAgents from "../../locales/en/agents.json";

const TEST_RESOURCES = { en: { common: enCommon, agents: enAgents } };

const api = vi.hoisted(() => ({
  pauseAgent: vi.fn(async () => ({})),
  resumeAgent: vi.fn(async () => ({})),
  cancelAgentTasks: vi.fn(async () => ({ cancelled: 3 })),
  getAgentSchedulingImpact: vi.fn(async () => ({
    running_tasks: 2,
    queued_tasks: 1,
    open_issues: 4,
    enabled_wakeups: 5,
  })),
}));

const agents = vi.hoisted(() => ({
  list: [] as Array<{ id: string; scheduling_paused_at: string | null }>,
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

vi.mock("@multica/core/workspace/queries", () => ({
  workspaceKeys: { agents: (ws: string) => ["workspaces", ws, "agents"] },
  agentListOptions: (ws: string) => ({
    queryKey: ["workspaces", ws, "agents"],
    queryFn: async () => agents.list,
  }),
}));
vi.mock("@multica/core/api", () => ({ api }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import {
  AgentPausedBadge,
  AgentPausedBadgeById,
  AgentPauseNote,
  AgentSchedulingDialog,
  canScheduleAgent,
} from "./agent-scheduling";

const agent = {
  id: "a-1",
  name: "Tofu",
  scheduling_paused_at: null,
} as never;

function renderWithProviders(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={qc}>{ui}</QueryClientProvider>
    </I18nProvider>,
  );
}

beforeEach(() => vi.clearAllMocks());

describe("AgentSchedulingDialog (pause)", () => {
  it("shows the blast radius before asking", async () => {
    renderWithProviders(<AgentSchedulingDialog agent={agent} action="pause" onClose={() => {}} />);
    expect(await screen.findByText(/2 running/i)).toBeTruthy();
    expect(screen.getByText(/1 queued/i)).toBeTruthy();
    expect(screen.getByText(/4 open issues/i)).toBeTruthy();
    expect(screen.getByText(/5 enabled wakeups/i)).toBeTruthy();
  });

  it("lets running runs finish by default", async () => {
    const onClose = vi.fn();
    renderWithProviders(<AgentSchedulingDialog agent={agent} action="pause" onClose={onClose} />);
    await screen.findByText(/2 running/i);
    fireEvent.click(screen.getByRole("button", { name: /^pause agent$/i }));
    await waitFor(() => expect(api.pauseAgent).toHaveBeenCalledWith("a-1", undefined));
    expect(api.cancelAgentTasks).not.toHaveBeenCalled();
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("cancels active runs after pausing when the box is ticked", async () => {
    renderWithProviders(<AgentSchedulingDialog agent={agent} action="pause" onClose={() => {}} />);
    await screen.findByText(/2 running/i);
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: /^pause agent$/i }));
    await waitFor(() => expect(api.cancelAgentTasks).toHaveBeenCalledWith("a-1"));
    expect(api.pauseAgent.mock.invocationCallOrder[0]).toBeLessThan(
      api.cancelAgentTasks.mock.invocationCallOrder[0]!,
    );
  });
});

describe("AgentSchedulingDialog (pause reason)", () => {
  it("sends the typed reason with the pause", async () => {
    renderWithProviders(<AgentSchedulingDialog agent={agent} action="pause" onClose={() => {}} />);
    await screen.findByText(/2 running/i);
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "budget freeze" } });
    fireEvent.click(screen.getByRole("button", { name: /^pause agent$/i }));
    await waitFor(() => expect(api.pauseAgent).toHaveBeenCalledWith("a-1", "budget freeze"));
  });

  it("still refreshes the list when the cancel step fails after the pause", async () => {
    api.cancelAgentTasks.mockRejectedValueOnce(new Error("cancel failed"));
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(qc, "invalidateQueries");
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={qc}>
          <AgentSchedulingDialog agent={agent} action="pause" onClose={() => {}} />
        </QueryClientProvider>
      </I18nProvider>,
    );
    await screen.findByText(/2 running/i);
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: /^pause agent$/i }));
    await waitFor(() => expect(api.cancelAgentTasks).toHaveBeenCalled());
    await waitFor(() => expect(spy).toHaveBeenCalled());
  });
});

describe("AgentPauseNote (who, when, why)", () => {
  it("names who paused, when and why; renders nothing for a running agent", () => {
    const paused = {
      scheduling_paused_at: new Date().toISOString(),
      scheduling_paused_by: "u-1",
      scheduling_paused_reason: "budget freeze",
    } as never;
    const { container, rerender } = renderWithProviders(<AgentPauseNote agent={paused} members={[{ user_id: "u-1", name: "Cheese" }]} />);
    expect(screen.getByText(/Paused by Cheese/)).toBeTruthy();
    expect(screen.getByText(/budget freeze/)).toBeTruthy();
    rerender(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <AgentPauseNote agent={{ scheduling_paused_at: null } as never} members={[]} />
        </QueryClientProvider>
      </I18nProvider>,
    );
    expect(container.textContent).toBe("");
  });
});

describe("AgentSchedulingDialog (resume)", () => {
  it("resumes the agent", async () => {
    renderWithProviders(<AgentSchedulingDialog agent={agent} action="resume" onClose={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /^resume agent$/i }));
    await waitFor(() => expect(api.resumeAgent).toHaveBeenCalledWith("a-1"));
  });
});

describe("AgentPausedBadge", () => {
  it("renders only while paused", () => {
    const { rerender, container } = renderWithProviders(<AgentPausedBadge agent={agent} />);
    expect(container.textContent).toBe("");
    const paused = { ...(agent as object), scheduling_paused_at: "2026-11-12T00:00:00Z" } as never;
    rerender(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <QueryClientProvider client={new QueryClient()}>
          <AgentPausedBadge agent={paused} />
        </QueryClientProvider>
      </I18nProvider>,
    );
    expect(screen.getByText("Paused")).toBeTruthy();
  });
});

describe("AgentPausedBadgeById (issue assignee, squad members)", () => {
  it("shows Paused for a paused agent and nothing for a running or unknown one", async () => {
    agents.list = [
      { id: "paused", scheduling_paused_at: "2026-11-12T00:00:00Z" },
      { id: "running", scheduling_paused_at: null },
    ];
    const { container, rerender } = renderWithProviders(<AgentPausedBadgeById agentId="paused" />);
    expect(await screen.findByText("Paused")).toBeTruthy();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    for (const id of ["running", "missing"]) {
      rerender(
        <I18nProvider locale="en" resources={TEST_RESOURCES}>
          <QueryClientProvider client={qc}>
            <AgentPausedBadgeById agentId={id} />
          </QueryClientProvider>
        </I18nProvider>,
      );
      await waitFor(() => expect(container.textContent).toBe(""));
    }
  });
});

describe("canScheduleAgent", () => {
  it("allows workspace owner and admin only, and never an archived agent", () => {
    expect(canScheduleAgent("owner", false)).toBe(true);
    expect(canScheduleAgent("admin", false)).toBe(true);
    expect(canScheduleAgent("member", false)).toBe(false);
    expect(canScheduleAgent(undefined, false)).toBe(false);
    expect(canScheduleAgent("owner", true)).toBe(false);
  });
});

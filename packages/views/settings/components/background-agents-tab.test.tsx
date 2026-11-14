import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import { BackgroundAgentsTab } from "./background-agents-tab";

const mockToastError = vi.hoisted(() => vi.fn());
const role = vi.hoisted(() => ({ current: "owner" as "owner" | "admin" | "member" }));

vi.mock("@multica/core/api", () => ({
  api: { getBackgroundAgents: vi.fn(), updateBackgroundAgents: vi.fn(), listAgents: vi.fn() },
  clientErrorMessage: (e: unknown) => (e instanceof Error ? e.message : undefined),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("@multica/core/permissions", () => ({
  useCurrentMember: () => ({ role: role.current }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: (wsId: string) => ({
    queryKey: ["agents", wsId],
    queryFn: () => api.listAgents({ workspace_id: wsId, include_archived: true }),
  }),
  memberListOptions: () => ({ queryKey: ["members", "ws"], queryFn: async () => [] }),
}));
vi.mock("@multica/core/paths", () => ({ useCurrentWorkspace: () => ({ id: "ws", name: "Acme" }) }));
vi.mock("../../navigation", () => ({
  useNavigation: () => ({ pathname: "/acme/settings", searchParams: new URLSearchParams("tab=background-agents") }),
  AppLink: ({ href, children }: { href: string; children: React.ReactNode }) => <a href={href}>{children}</a>,
}));
vi.mock("sonner", () => ({ toast: { error: mockToastError, success: vi.fn() } }));

const read = vi.mocked(api.getBackgroundAgents);
const write = vi.mocked(api.updateBackgroundAgents);
const agents = vi.mocked(api.listAgents);

const agent = (id: string, name: string, extra: Record<string, unknown> = {}) =>
  ({ id, name, archived_at: null, visibility: "workspace", ...extra }) as never;

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <BackgroundAgentsTab />
    </QueryClientProvider>,
  );
}

const stored = {
  quick_actions: { agent_id: "a-haiku", fallback_agent_id: null },
  compact: { agent_id: "a-luna" },
  routing: { agent_id: "a-luna", statuses: ["todo", "blocked"], overrides: { todo: "a-haiku" } },
};

async function choose(name: string, option: string) {
  await userEvent.click(await screen.findByRole("combobox", { name }));
  await userEvent.click(await screen.findByRole("option", { name: option }));
}

beforeEach(() => {
  role.current = "owner";
  mockToastError.mockReset();
  agents.mockReset().mockResolvedValue([
    agent("a-haiku", "Haiku router"),
    agent("a-luna", "Luna"),
    agent("a-old", "Retired", { archived_at: "2026-11-01T00:00:00Z" }),
    agent("a-dana", "Dana notes bot", { visibility: "private" }),
  ]);
  read.mockReset().mockResolvedValue(structuredClone(stored) as never);
  write.mockReset().mockImplementation(async (_ws, value) => value as never);
});

describe("BackgroundAgentsTab", () => {
  it("shows one row per job with its no-agent note", async () => {
    mount();
    expect(await screen.findByText("No agent: uses the title cut (first 60 characters).")).toBeInTheDocument();
    expect(screen.getByText("No agent: no buttons.")).toBeInTheDocument();
    expect(screen.getByText("No agent: each step keeps its own agent.")).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: "Quick actions: Agent" })).toHaveTextContent("Haiku router"),
    );
  });

  it("a member sees the picks read-only with a link to Personal background agents", async () => {
    role.current = "member";
    mount();
    expect(await screen.findByText("Haiku router")).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Personal → Background agents" })).toHaveAttribute(
      "href",
      expect.stringContaining("tab=personal-background-agents"),
    );
  });

  it("an owner change sends one PUT with both slots and keeps every other job", async () => {
    mount();
    await choose("DM title: Agent", "Luna");

    await waitFor(() => expect(write).toHaveBeenCalledTimes(1));
    expect(write).toHaveBeenCalledWith("ws", {
      dm_title: { agent_id: "a-luna", fallback_agent_id: null },
      quick_actions: { agent_id: "a-haiku", fallback_agent_id: null },
      compact: { agent_id: "a-luna", fallback_agent_id: null },
      routing: {
        agent_id: "a-luna",
        fallback_agent_id: null,
        statuses: ["todo", "blocked"],
        overrides: { todo: "a-haiku" },
      },
    });
  });

  it("keeps the primary slot when only the fallback changes", async () => {
    mount();
    await choose("Quick actions: Fallback agent", "Luna");

    await waitFor(() => expect(write).toHaveBeenCalledTimes(1));
    expect(write.mock.calls[0]![1]).toMatchObject({
      quick_actions: { agent_id: "a-haiku", fallback_agent_id: "a-luna" },
    });
  });

  it("disables the fallback until a primary agent is picked", async () => {
    mount();
    const fallback = await screen.findByRole("combobox", { name: "DM title: Fallback agent" });
    expect(fallback).toBeDisabled();
    expect(screen.getAllByText("Pick an agent first.").length).toBeGreaterThan(0);
  });

  it("clearing the primary agent clears the fallback in the same PUT", async () => {
    read.mockResolvedValue({ quick_actions: { agent_id: "a-haiku", fallback_agent_id: "a-luna" } } as never);
    mount();
    await choose("Quick actions: Agent", "No agent");

    await waitFor(() => expect(write).toHaveBeenCalledTimes(1));
    expect(write.mock.calls[0]![1]).toMatchObject({
      quick_actions: { agent_id: null, fallback_agent_id: null },
    });
  });

  it("does not offer archived agents and greys out agents the whole workspace cannot run", async () => {
    mount();
    await userEvent.click(await screen.findByRole("combobox", { name: "DM title: Agent" }));
    expect(await screen.findByRole("option", { name: "Haiku router" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /Retired/ })).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: /Dana notes bot/ })).toHaveAttribute("aria-disabled", "true");
  });

  it("reports a failed save", async () => {
    write.mockRejectedValue(new Error("agent not found"));
    mount();
    await choose("DM title: Agent", "Luna");

    await waitFor(() => expect(mockToastError).toHaveBeenCalledWith("agent not found"));
  });
});

import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { ApiError } from "@multica/core/api/client";
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
  read.mockReset().mockResolvedValue({ value: structuredClone(stored), etag: '"v1"' } as never);
  write.mockReset().mockImplementation(async (_ws, value) => ({ value, etag: '"v2"' }) as never);
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
    }, '"v1"');
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
    read.mockResolvedValue({ value: { quick_actions: { agent_id: "a-haiku", fallback_agent_id: "a-luna" } }, etag: '"v1"' } as never);
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

  it.each(["owner", "member"] as const)("%s sees the load error and Retry when the response is malformed", async (who) => {
    role.current = who;
    read.mockResolvedValueOnce(null);
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't load background agents.");
    expect(screen.queryByText("No agent")).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("No agent: no buttons.")).toBeInTheDocument();
    expect(read).toHaveBeenCalledTimes(2);
  });

  it("shows an empty saved value ({}) as No agent, not as an error", async () => {
    read.mockResolvedValue({ value: {}, etag: '"v0"' } as never);
    mount();
    expect(await screen.findByText("No agent: no buttons.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Quick actions: Agent" })).toHaveTextContent("No agent");
  });

  describe("another owner saved first (409)", () => {
    const theirs = {
      quick_actions: { agent_id: "a-luna", fallback_agent_id: null },
      compact: { agent_id: "a-luna" },
    };
    const conflict = () =>
      new ApiError("conflict", 409, "Conflict", { code: "background_agents_conflict", current: theirs, etag: '"v9"' });

    async function conflictOnDmTitle() {
      write.mockReset().mockRejectedValueOnce(conflict());
      mount();
      await choose("DM title: Agent", "Haiku router");
      return await screen.findByRole("alert");
    }

    it("sends If-Match, saves nothing, and shows both versions instead of overwriting", async () => {
      const alert = await conflictOnDmTitle();
      expect(write).toHaveBeenCalledTimes(1);
      expect(write.mock.calls[0]![2]).toBe('"v1"');
      expect(alert).toHaveTextContent("Another owner saved a change while you were editing.");
      expect(alert).toHaveTextContent("Your change was not saved.");
      expect(screen.getByText("Your change (not saved)")).toBeInTheDocument();
      expect(screen.getByText("Saved now")).toBeInTheDocument();
      // The page already shows their value, not the stale one.
      expect(screen.getByRole("combobox", { name: "Quick actions: Agent" })).toHaveTextContent("Luna");
      expect(mockToastError).not.toHaveBeenCalled();
    });

    it("Keep their version drops my change and sends nothing", async () => {
      await conflictOnDmTitle();
      await userEvent.click(screen.getByRole("button", { name: "Keep their version" }));
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
      expect(screen.getByRole("combobox", { name: "DM title: Agent" })).toHaveTextContent("No agent");
      expect(write).toHaveBeenCalledTimes(1);
    });

    it("Apply my change again saves on top of their value with the new ETag", async () => {
      await conflictOnDmTitle();
      write.mockImplementationOnce(async (_ws, value) => ({ value, etag: '"v10"' }) as never);
      await userEvent.click(screen.getByRole("button", { name: "Apply my change again" }));

      await waitFor(() => expect(write).toHaveBeenCalledTimes(2));
      expect(write.mock.calls[1]![1]).toMatchObject({
        dm_title: { agent_id: "a-haiku", fallback_agent_id: null },
        quick_actions: { agent_id: "a-luna", fallback_agent_id: null },
      });
      expect(write.mock.calls[1]![2]).toBe('"v9"');
      await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    });

    it("keeps editing off while the conflict is open", async () => {
      await conflictOnDmTitle();
      expect(screen.getByRole("combobox", { name: "DM title: Agent" })).toBeDisabled();
    });
  });
});

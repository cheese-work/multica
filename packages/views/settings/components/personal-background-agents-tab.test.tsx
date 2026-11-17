import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import { PersonalBackgroundAgentsTab } from "./personal-background-agents-tab";

const mockToastError = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: {
    getBackgroundAgents: vi.fn(),
    getMyBackgroundAgents: vi.fn(),
    updateMyBackgroundAgents: vi.fn(),
    listAgents: vi.fn(),
  },
  clientErrorMessage: (e: unknown) => (e instanceof Error ? e.message : undefined),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: (wsId: string) => ({
    queryKey: ["agents", wsId],
    queryFn: () => api.listAgents({ workspace_id: wsId, include_archived: true }),
  }),
}));
vi.mock("../../navigation", () => ({
  useNavigation: () => ({ pathname: "/acme/settings", searchParams: new URLSearchParams("tab=personal-background-agents") }),
  AppLink: ({ href, children }: { href: string; children: React.ReactNode }) => <a href={href}>{children}</a>,
}));
vi.mock("sonner", () => ({ toast: { error: mockToastError, success: vi.fn() } }));

const workspaceRead = vi.mocked(api.getBackgroundAgents);
const mine = vi.mocked(api.getMyBackgroundAgents);
const write = vi.mocked(api.updateMyBackgroundAgents);
const agents = vi.mocked(api.listAgents);

const agent = (id: string, name: string, extra: Record<string, unknown> = {}) =>
  ({ id, name, archived_at: null, visibility: "workspace", ...extra }) as never;

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <PersonalBackgroundAgentsTab />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockToastError.mockReset();
  agents.mockReset().mockResolvedValue([
    agent("a-haiku", "Haiku router"),
    agent("a-luna", "Luna"),
    agent("a-notes", "My notes bot", { visibility: "private" }),
    agent("a-old", "Retired", { archived_at: "2026-11-01T00:00:00Z" }),
  ]);
  workspaceRead.mockReset().mockResolvedValue({
    value: { quick_actions: { agent_id: "a-haiku", fallback_agent_id: "" } },
    etag: '"w1"',
  } as never);
  mine.mockReset().mockResolvedValue({ dm_title: { agent_id: "a-luna", fallback_agent_id: "" } } as never);
  write.mockReset().mockResolvedValue({} as never);
});

describe("PersonalBackgroundAgentsTab", () => {
  it("an unset row shows the workspace default", async () => {
    mount();
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: "Quick actions: Agent" })).toHaveTextContent(
        "Using workspace: Haiku router",
      ),
    );
    expect(screen.getByRole("combobox", { name: "DM title: Agent" })).toHaveTextContent("Luna");
  });

  it("an unset row with no workspace agent says so", async () => {
    workspaceRead.mockResolvedValue({ value: {}, etag: null } as never);
    mount();
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: "Quick actions: Agent" })).toHaveTextContent(
        "Using workspace: No agent",
      ),
    );
  });

  it("clearing a personal pick sends null for the agent and its fallback", async () => {
    mount();
    await userEvent.click(await screen.findByRole("combobox", { name: "DM title: Agent" }));
    await userEvent.click(await screen.findByRole("option", { name: "Use workspace pick" }));

    await waitFor(() => expect(write).toHaveBeenCalledTimes(1));
    expect(write).toHaveBeenCalledWith("ws", { dm_title: { agent_id: null, fallback_agent_id: null } });
  });

  it("picking an agent sends only that slot", async () => {
    mount();
    await userEvent.click(await screen.findByRole("combobox", { name: "Quick actions: Agent" }));
    await userEvent.click(await screen.findByRole("option", { name: /My notes bot/ }));

    await waitFor(() => expect(write).toHaveBeenCalledWith("ws", { quick_actions: { agent_id: "a-notes" } }));
  });

  it("an archived personal pick warns, does not fall through, and one button returns to the workspace pick", async () => {
    mine.mockResolvedValue({ quick_actions: { agent_id: "a-old", fallback_agent_id: "" } } as never);
    mount();
    expect(await screen.findByText(/Retired is archived/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Use the workspace pick (Haiku router)" }));

    await waitFor(() =>
      expect(write).toHaveBeenCalledWith("ws", { quick_actions: { agent_id: null, fallback_agent_id: null } }),
    );
  });

  it("disables the fallback until a personal agent is picked", async () => {
    mount();
    expect(await screen.findByRole("combobox", { name: "Quick actions: Fallback agent" })).toBeDisabled();
    expect(screen.getByRole("combobox", { name: "DM title: Fallback agent" })).toBeEnabled();
  });

  it("does not render Routing", async () => {
    mount();
    await screen.findByRole("combobox", { name: "DM title: Agent" });
    expect(screen.queryByText("Routing")).not.toBeInTheDocument();
  });

  it("shows the load error with Retry when the personal value cannot be read", async () => {
    mine.mockResolvedValueOnce(null);
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't load your background agents.");
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("combobox", { name: "DM title: Agent" })).toBeInTheDocument();
  });

  it("reports a failed save and re-reads", async () => {
    write.mockRejectedValue(new Error("agent not eligible"));
    mount();
    await userEvent.click(await screen.findByRole("combobox", { name: "Quick actions: Agent" }));
    await userEvent.click(await screen.findByRole("option", { name: "Luna" }));

    await waitFor(() => expect(mockToastError).toHaveBeenCalledWith("agent not eligible"));
    await waitFor(() => expect(mine.mock.calls.length).toBeGreaterThan(1));
  });
});

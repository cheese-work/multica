import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import { RoutingAgentRows } from "./routing-agent-rows";

const mockToastError = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: { getRoutingAgent: vi.fn(), updateRoutingAgent: vi.fn(), listAgents: vi.fn() },
}));
vi.mock("sonner", () => ({ toast: { error: mockToastError, success: vi.fn() } }));

const read = vi.mocked(api.getRoutingAgent);
const write = vi.mocked(api.updateRoutingAgent);
const agents = vi.mocked(api.listAgents);

const agent = (id: string, name: string, archived = false) =>
  ({ id, name, archived_at: archived ? "2026-11-01T00:00:00Z" : null }) as never;

function mount(canEdit = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <RoutingAgentRows wsId="ws" canEdit={canEdit} />
    </QueryClientProvider>,
  );
}

const routing = { name: "Routing agent" };
const fallback = { name: "Fallback agent" };

async function choose(trigger: { name: string }, option: string) {
  await userEvent.click(await screen.findByRole("combobox", trigger));
  await userEvent.click(await screen.findByRole("option", { name: option }));
}

beforeEach(() => {
  mockToastError.mockReset();
  agents.mockReset().mockResolvedValue([
    agent("a-haiku", "Haiku router"),
    agent("a-luna", "Luna"),
    agent("a-old", "Retired", true),
  ]);
  read.mockReset().mockResolvedValue({
    agent_id: "a-luna",
    fallback_agent_id: "",
    statuses: ["todo", "blocked"],
    overrides: { "todo": "a-haiku" },
  });
  write.mockReset().mockImplementation(async (_ws, config) => config);
});

describe("RoutingAgentRows", () => {
  it("shows the configured routing agent", async () => {
    mount();
    const trigger = await screen.findByRole("combobox", routing);
    await waitFor(() => expect(trigger).toHaveTextContent("Luna"));
  });

  it("saves a new routing agent and keeps statuses and overrides it does not edit", async () => {
    mount();
    await choose(routing, "Haiku router");

    await waitFor(() => expect(write).toHaveBeenCalledTimes(1));
    expect(write).toHaveBeenCalledWith("ws", {
      agent_id: "a-haiku",
      fallback_agent_id: "",
      statuses: ["todo", "blocked"],
      overrides: { todo: "a-haiku" },
    });
  });

  it("sets a fallback agent", async () => {
    mount();
    await choose(fallback, "Haiku router");

    await waitFor(() => expect(write).toHaveBeenCalledTimes(1));
    expect(write.mock.calls[0]![1]).toMatchObject({ agent_id: "a-luna", fallback_agent_id: "a-haiku" });
  });

  it("does not offer archived agents", async () => {
    mount();
    await userEvent.click(await screen.findByRole("combobox", routing));
    expect(await screen.findByRole("option", { name: "Haiku router" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "Retired" })).not.toBeInTheDocument();
  });

  it("reports a failed save and sends nothing else", async () => {
    write.mockRejectedValue(new Error("agent not found"));
    mount();
    await choose(routing, "Haiku router");

    await waitFor(() => expect(mockToastError).toHaveBeenCalledWith("agent not found"));
  });

  it("is read-only for a member who cannot edit", async () => {
    mount(false);
    expect(await screen.findByText("Luna")).toBeInTheDocument();
    expect(screen.queryByRole("combobox", routing)).not.toBeInTheDocument();
  });
});

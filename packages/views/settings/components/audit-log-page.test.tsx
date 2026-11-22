import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { api } from "@multica/core/api";
import type { AuditEvent, AuditEventsPage } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { AuditLogPage } from "./audit-log-page";

vi.mock("@multica/core/api", () => ({ api: { listAuditEvents: vi.fn() } }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: unknown) => unknown) => selector({ user: { id: "u" } }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({
    queryKey: ["members", "ws"],
    queryFn: async () => [{ user_id: "u1", name: "Ada", role: "admin" }],
  }),
  agentListOptions: () => ({
    queryKey: ["agents", "ws"],
    queryFn: async () => [{ id: "a1", name: "Tofu" }],
  }),
}));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    agentDetail: (id: string) => `/acme/agents/${id}`,
    runtimeDetail: (id: string) => `/acme/runtimes/${id}`,
    skillDetail: (id: string) => `/acme/skills/${id}`,
    squadDetail: (id: string) => `/acme/squads/${id}`,
    projectDetail: (id: string) => `/acme/projects/${id}`,
    autopilotDetail: (id: string) => `/acme/autopilots/${id}`,
    settings: () => "/acme/settings",
  }),
}));
vi.mock("../../navigation", () => ({
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={href} {...rest}>{children}</a>
  ),
}));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "Asia/Ho_Chi_Minh" }));

const list = vi.mocked(api.listAuditEvents);

const agentUpdate: AuditEvent = {
  id: "e1",
  at: "2026-11-09T04:18:00Z",
  actor_type: "agent",
  actor_id: "a1",
  actor_name: "Tofu",
  task_id: "t1",
  kind: "agent.update",
  target_type: "agent",
  target_id: "a2",
  target_name: "Sonnet",
  before: { max_concurrent_tasks: 6, description: "changed" },
  after: { max_concurrent_tasks: 2, description: "changed" },
  external_requested: [],
};
const olderMember: AuditEvent = {
  ...agentUpdate,
  id: "e2",
  at: "2026-11-09T03:00:00Z",
  actor_type: "member",
  actor_id: "u1",
  actor_name: "Ada",
  task_id: null,
  kind: "runtime.delete",
  target_type: "runtime",
  target_id: "r1",
  target_name: null,
  before: { name: "box-1" },
  after: {},
};

const page = (events: AuditEvent[], next_cursor: string | null = null): AuditEventsPage => ({ events, next_cursor });

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <AuditLogPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  list.mockReset().mockResolvedValue(page([agentUpdate, olderMember]));
});

it("shows local time, actor name and type, kind, and the target as a link", async () => {
  mount();
  const row = (await screen.findByText("agent.update")).closest("tr")!;
  // 04:18 UTC is 11:18 in Ho Chi Minh City (ICT).
  expect(within(row).getByText(/11:18/)).toBeVisible();
  expect(within(row).getByText("Tofu")).toBeVisible();
  expect(within(row).getByText("Agent")).toBeVisible();
  expect(within(row).getByRole("link", { name: "Sonnet" })).toHaveAttribute("href", "/acme/agents/a2");
});

it("shows an allowed field as before to after", async () => {
  mount();
  const row = (await screen.findByText("agent.update")).closest("tr")!;
  const diff = within(row).getByText("max_concurrent_tasks").closest("li")!;
  expect(within(diff).getByText("6")).toBeVisible();
  expect(within(diff).getByText("2")).toBeVisible();
});

it("shows a redacted field as \"changed\", never a value", async () => {
  mount();
  const row = (await screen.findByText("agent.update")).closest("tr")!;
  const diff = within(row).getByText("description").closest("li")!;
  expect(within(diff).getByText("changed")).toBeVisible();
});

it("shows a deleted target as a name-less, link-less cell", async () => {
  mount();
  const row = (await screen.findByText("runtime.delete")).closest("tr")!;
  expect(within(row).getByText("(deleted)")).toBeVisible();
  expect(within(row).queryByRole("link")).toBeNull();
  expect(within(row).getByText("Member")).toBeVisible();
});

it("requests the next cursor on Load more and appends the rows", async () => {
  list.mockResolvedValueOnce(page([agentUpdate], "c1")).mockResolvedValueOnce(page([olderMember]));
  mount();
  await screen.findByText("agent.update");
  expect(list).toHaveBeenLastCalledWith("ws", expect.not.objectContaining({ cursor: expect.anything() }));
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  await screen.findByText("runtime.delete");
  expect(list).toHaveBeenLastCalledWith("ws", expect.objectContaining({ cursor: "c1" }));
  expect(screen.getByText("agent.update")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
});

it("sends the kind prefix, the actor and the time range as query filters", async () => {
  mount();
  await screen.findByText("agent.update");
  fireEvent.change(screen.getByLabelText("Event kind"), { target: { value: "agent." } });
  await waitFor(() => expect(list).toHaveBeenLastCalledWith("ws", expect.objectContaining({ kind: "agent." })));
  fireEvent.change(await screen.findByLabelText("Actor"), { target: { value: "u1" } });
  await waitFor(() => expect(list).toHaveBeenLastCalledWith("ws", expect.objectContaining({ kind: "agent.", actor_id: "u1" })));
  fireEvent.change(screen.getByLabelText("From"), { target: { value: "2026-11-09T11:00" } });
  fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-11-09T12:00" } });
  await waitFor(() =>
    expect(list).toHaveBeenLastCalledWith(
      "ws",
      expect.objectContaining({
        kind: "agent.",
        actor_id: "u1",
        since: new Date("2026-11-09T11:00").toISOString(),
        until: new Date("2026-11-09T12:00").toISOString(),
      }),
    ),
  );
});

it("starts again from the first page when a filter changes", async () => {
  list.mockResolvedValueOnce(page([agentUpdate], "c1"));
  mount();
  await screen.findByText("agent.update");
  fireEvent.change(screen.getByLabelText("Event kind"), { target: { value: "runtime." } });
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  expect(list.mock.calls[1]![1]).not.toHaveProperty("cursor");
});

it("shows the empty state", async () => {
  list.mockResolvedValue(page([]));
  mount();
  expect(await screen.findByText("No audit events match.")).toBeVisible();
});

it("shows a loading state, then the rows", async () => {
  mount();
  expect(screen.getByText("Loading audit events…")).toBeVisible();
  await screen.findByText("agent.update");
  expect(screen.queryByText("Loading audit events…")).toBeNull();
});

it("shows an error with a Retry that reloads", async () => {
  list.mockRejectedValueOnce(new Error("boom"));
  mount();
  expect(await screen.findByRole("alert")).toHaveTextContent("Could not load the audit log.");
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByText("agent.update");
  expect(screen.queryByRole("alert")).toBeNull();
});

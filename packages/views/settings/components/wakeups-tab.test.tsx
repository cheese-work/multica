import { beforeEach, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import { WakeupsTab } from "./wakeups-tab";

vi.mock("@multica/core/api", () => ({
  api: { listWorkspaceSystemWakeups: vi.fn(), updateWorkspaceSystemWakeup: vi.fn(), updateWorkspace: vi.fn() },
}));
let settings: Record<string, unknown> = {};
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "ws", settings }),
}));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "UTC" }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: unknown) => unknown) => selector({ user: { id: "u" } }),
}));
let role = "admin";
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members", "ws"], queryFn: async () => [{ user_id: "u", name: "Ada", role }] }),
  workspaceKeys: { list: () => ["workspaces"] },
}));

const list = vi.mocked(api.listWorkspaceSystemWakeups);
const update = vi.mocked(api.updateWorkspaceSystemWakeup);
const updateWorkspace = vi.mocked(api.updateWorkspace);

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <WakeupsTab />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  role = "admin";
  settings = { github_enabled: true };
  updateWorkspace.mockReset().mockResolvedValue({ id: "ws" } as never);
  list.mockReset().mockResolvedValue([
    { rule: "child_done", enabled: true, instruction: "", builtin_instruction: "Advance the next stage.", customized: 2 },
  ]);
  update.mockReset().mockResolvedValue(undefined);
});

it("turns the sub-issue rule's default off and saves a workspace instruction", async () => {
  mount();
  const toggle = await screen.findByRole("switch", { name: "On by default" });
  expect(screen.getByText("2 open issues have their own setting and ignore this default.")).toBeVisible();
  fireEvent.click(toggle);
  await waitFor(() => expect(update).toHaveBeenCalledWith("child_done", { enabled: false }));

  const input = screen.getByRole("textbox", { name: "Default instruction" });
  expect(screen.getByText("Advance the next stage.")).toBeVisible();
  expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  fireEvent.change(input, { target: { value: "  Wrap up in the parent.  " } });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(update).toHaveBeenCalledWith("child_done", { instruction: "Wrap up in the parent." }));
});

it("shows the defaults read-only to members who cannot change them", async () => {
  role = "member";
  mount();
  expect(await screen.findByText("Only owners and admins can change these defaults.")).toBeVisible();
  expect(screen.getByRole("switch", { name: "On by default" })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByRole("textbox", { name: "Default instruction" })).toBeDisabled();
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
});

it("lets an admin pause every wakeup and keeps the rest of the settings", async () => {
  mount();
  const toggle = await screen.findByRole("switch", { name: "Pause all wakeups" });
  // The switch stays disabled until the member list says the viewer is an admin.
  await waitFor(() => expect(toggle).not.toHaveAttribute("aria-disabled", "true"));
  fireEvent.click(toggle);
  await waitFor(() =>
    expect(updateWorkspace).toHaveBeenCalledWith("ws", { settings: { github_enabled: true, wakeups_paused: true } }),
  );
});

it("names who paused the workspace and shows the per-rule switch as overridden", async () => {
  settings = { wakeups_paused: true, wakeups_paused_by: "u", wakeups_paused_at: "2026-10-15T08:00:00Z" };
  mount();
  expect(await screen.findByText(/Wakeups paused by Ada at /)).toBeVisible();
  expect(screen.getByRole("switch", { name: "Pause all wakeups" })).toBeChecked();
  expect(screen.getByRole("switch", { name: "On by default" })).toHaveAttribute("aria-disabled", "true");
  expect(screen.getByText("Overridden while wakeups are paused.")).toBeVisible();
  expect(screen.getByTestId("wakeups-paused-banner")).toBeVisible();
  expect(screen.getByTestId("wakeups-pause-switch")).toBe(screen.getByRole("switch", { name: "Pause all wakeups" }));
  expect(screen.getByTestId("wakeups-child-done-switch")).toBe(screen.getByRole("switch", { name: "On by default" }));
  fireEvent.click(screen.getByRole("switch", { name: "Pause all wakeups" }));
  await waitFor(() =>
    expect(updateWorkspace).toHaveBeenCalledWith("ws", expect.objectContaining({ settings: expect.objectContaining({ wakeups_paused: false }) })),
  );
});

it("shows no pause switch to members who cannot change it", async () => {
  role = "member";
  mount();
  await screen.findByRole("switch", { name: "On by default" });
  expect(screen.getByRole("switch", { name: "Pause all wakeups" })).toHaveAttribute("aria-disabled", "true");
});

it("confirms a saved pause and a saved resume (approved rev-3 A: Saved and Resumed states)", async () => {
  // The saved workspace comes back with the switch as the server stamps it.
  updateWorkspace.mockImplementation((async (_id: string, body: { settings: Record<string, unknown> }) => {
    settings = body.settings;
    return { id: "ws" };
  }) as never);
  mount();
  const toggle = await screen.findByRole("switch", { name: "Pause all wakeups" });
  await waitFor(() => expect(toggle).not.toHaveAttribute("aria-disabled", "true"));
  fireEvent.click(toggle);
  expect(await screen.findByText("Wakeups paused.")).toBeVisible();
  fireEvent.click(screen.getByRole("switch", { name: "Pause all wakeups" }));
  expect(await screen.findByText("Wakeups resumed.")).toBeVisible();
  expect(screen.queryByText("Wakeups paused.")).toBeNull();
});

it("offers Retry when the defaults cannot be loaded (approved rev-3 A: load-error state)", async () => {
  list.mockReset().mockRejectedValueOnce(new Error("boom")).mockResolvedValue([
    { rule: "child_done", enabled: true, instruction: "", builtin_instruction: "", customized: 0 },
  ]);
  mount();
  expect(await screen.findByRole("alert")).toHaveTextContent("Could not load wakeup defaults.");
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  expect(await screen.findByRole("switch", { name: "On by default" })).toBeVisible();
  expect(screen.queryByRole("alert")).toBeNull();
});

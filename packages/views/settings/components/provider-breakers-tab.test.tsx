import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import { ProviderBreakersTab } from "./provider-breakers-tab";

vi.mock("@multica/core/api", () => ({
  api: { listProviderBreakers: vi.fn(), resetProviderBreaker: vi.fn() },
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: unknown) => unknown) => selector({ user: { id: "u" } }),
}));
let role = "admin";
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members", "ws"], queryFn: async () => [{ user_id: "u", name: "Ada", role }] }),
}));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "Asia/Ho_Chi_Minh" }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const list = vi.mocked(api.listProviderBreakers);
const reset = vi.mocked(api.resetProviderBreaker);

const open = {
  provider: "openai",
  state: "open" as const,
  open_reason: "repeated provider 5xx failures",
  failure_count: 3,
  opened_at: "2026-11-07T02:00:00Z",
  next_probe_at: "2026-11-07T02:30:00Z",
  last_success_at: "2026-11-07T01:55:00Z",
  trip_evidence: ["t1", "t2", "t3"],
};
const closed = { ...open, provider: "anthropic", state: "closed" as const, open_reason: "", failure_count: 0, opened_at: null, next_probe_at: null };

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <ProviderBreakersTab />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  role = "admin";
  list.mockReset().mockResolvedValue([open, closed]);
  reset.mockReset().mockResolvedValue(undefined);
});

it("shows an admin the open breaker's state and a Reset button", async () => {
  mount();
  const row = (await screen.findByText("openai")).closest("tr")!;
  expect(within(row).getByText("Open")).toBeVisible();
  expect(within(row).getByText("repeated provider 5xx failures")).toBeVisible();
  expect(within(row).getByText("3")).toBeVisible();
  // 02:00 UTC is 09:00 in Ho Chi Minh City (ICT).
  expect(within(row).getByText(/09:00/)).toBeVisible();
  expect(within(row).getByRole("button", { name: "Reset openai breaker" })).toBeVisible();
});

it("hides Reset from a member and from a closed breaker", async () => {
  role = "member";
  mount();
  await screen.findByText("openai");
  await waitFor(() => expect(screen.queryByRole("button", { name: /^Reset/ })).toBeNull());
});

it("hides Reset on a closed breaker for an admin", async () => {
  mount();
  await screen.findByText("anthropic");
  await screen.findByRole("button", { name: "Reset openai breaker" });
  expect(screen.queryByRole("button", { name: "Reset anthropic breaker" })).toBeNull();
});

it("resets only after the admin confirms", async () => {
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Reset openai breaker" }));
  expect(reset).not.toHaveBeenCalled();
  const dialog = await screen.findByRole("alertdialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Reset breaker" }));
  await waitFor(() => expect(reset).toHaveBeenCalledWith("ws", "openai"));
  await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
});

it("keeps the dialog open when the reset fails", async () => {
  reset.mockRejectedValue(new Error("forbidden"));
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Reset openai breaker" }));
  const dialog = await screen.findByRole("alertdialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Reset breaker" }));
  await waitFor(() => expect(reset).toHaveBeenCalled());
  expect(screen.getByRole("alertdialog")).toBeVisible();
});

it("shows the empty state", async () => {
  list.mockResolvedValue([]);
  mount();
  expect(await screen.findByText("No breaker has tripped in this workspace.")).toBeVisible();
});

it("offers a retry when loading fails", async () => {
  list.mockRejectedValueOnce(new Error("boom"));
  mount();
  expect(await screen.findByText("Could not load provider breakers.")).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByText("openai")).toBeVisible();
});

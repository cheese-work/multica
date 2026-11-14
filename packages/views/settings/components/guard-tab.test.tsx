import { beforeEach, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import { GuardTab } from "./guard-tab";

vi.mock("@multica/core/api", () => ({
  api: { getGuardStats: vi.fn(), listGuardDecisions: vi.fn() },
}));
let settings: Record<string, unknown> = {};
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "ws", name: "Cheese", settings }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "Asia/Bangkok" }));

const stats = vi.mocked(api.getGuardStats);
const decisions = vi.mocked(api.listGuardDecisions);

const day = (d: string, fixed: number, refused: number) => ({ day: d, fixed, refused, errors: 0 });
const POLICY = {
  version: 42,
  plugins: [
    { name: "pr-title-tag", match: { tool: "gh", subcommand: ["pr"] }, action: "fix", builtin: "gh-pr", message: "guard: title tag added" },
    { name: "ci-wait", match: { tool: "gh" }, action: "deny", builtin: "ci-wait", fail_closed: true },
    { name: "wakeup-create", match: { tool: "multica", subcommand: ["issue", "wakeup", "create"] }, action: "shadow", builtin: "multica-wakeup" },
  ],
  review: [
    { repo: "cheese-work/multica", reviewers: ["a", "b"] },
    { repo: "tevo-studio/*", github: "skip" },
  ],
};

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <GuardTab />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  settings = { guard_policy: POLICY };
  stats.mockReset().mockResolvedValue({
    days: 7,
    tz: "Asia/Bangkok",
    since: "2026-11-08",
    plugins: [
      { plugin: "pr-title-tag", fixed: 42, refused: 3, errors: 0, shadow_hits: 0, passes: 0, daily: [day("2026-11-08", 30, 1), day("2026-11-09", 12, 2)] },
      { plugin: "ci-wait", fixed: 0, refused: 18, errors: 1, shadow_hits: 0, passes: 0, daily: [] },
    ],
  });
  decisions.mockReset().mockResolvedValue([
    { id: "d1", task_id: "t", time: "2026-11-13T15:41:00Z", plugin: "pr-title-tag", action: "fixed", tool: "gh", subcommand: "pr create", line: "title tag [squash] added", issue: "CHE-5189", note: "", runtime: "claude" },
  ]);
});

it("lists plugins with their counts and shows the first one's detail", async () => {
  mount();
  const list = await screen.findByRole("list", { name: "Guard plugins" });
  const first = within(list).getAllByRole("listitem")[0]!;
  expect(within(first).getByText("pr-title-tag")).toBeVisible();
  expect(within(first).getByText("Fixed")).toBeVisible();
  expect(within(first).getByText("42")).toBeVisible();
  expect(screen.getByRole("heading", { level: 2, name: "pr-title-tag" })).toBeVisible();
  expect(screen.getByText("guard: title tag added")).toBeVisible();
  expect(screen.getByText("Policy version 42")).toBeVisible();
  expect(stats).toHaveBeenCalledWith(7);
});

it("shows the recent decisions of the selected plugin in a named, focusable region", async () => {
  mount();
  const region = await screen.findByRole("region", { name: "Recent decisions" });
  expect(region).toHaveAttribute("tabindex", "0");
  expect(await within(region).findByText("CHE-5189")).toBeVisible();
  expect(within(region).getByText("title tag [squash] added")).toBeVisible();
  expect(decisions).toHaveBeenCalledWith({ plugin: "pr-title-tag", limit: 20 });
});

it("switches the detail when another plugin is picked", async () => {
  mount();
  fireEvent.click(await screen.findByRole("button", { name: /ci-wait/ }));
  expect(screen.getByRole("heading", { level: 2, name: "ci-wait" })).toBeVisible();
  expect(screen.getByText("Refuses the call, and logs it.")).toBeVisible();
  await waitFor(() => expect(decisions).toHaveBeenLastCalledWith({ plugin: "ci-wait", limit: 20 }));
});

it("describes the chart for assistive tech with the same totals as the counts", async () => {
  mount();
  const chart = await screen.findByRole("img", { name: /Fixed per day 30, 12; refused per day 1, 2/ });
  expect(chart).toBeVisible();
});

it("drills in at phone width and the back link restores the selected row", async () => {
  mount();
  const row = await screen.findByRole("button", { name: /ci-wait/ });
  fireEvent.click(row);
  const detail = screen.getByRole("region", { name: "Plugin details" });
  expect(detail).toHaveAttribute("data-open", "true");
  fireEvent.click(within(detail).getByRole("button", { name: "All plugins" }));
  expect(detail).toHaveAttribute("data-open", "false");
  expect(screen.getByRole("button", { name: /ci-wait/ })).toHaveFocus();
  expect(screen.getByRole("button", { name: /ci-wait/ })).toHaveAttribute("aria-current", "true");
});

it("shows the review policy rows and the CLI footer", async () => {
  mount();
  expect(await screen.findByText("cheese-work/multica")).toBeVisible();
  expect(screen.getByText("2 reviewers · GitHub review required")).toBeVisible();
  expect(screen.getByText("Multica review only")).toBeVisible();
  expect(screen.getByText("multica guard stats")).toBeVisible();
});

it("explains how to edit instead of editing in the page", async () => {
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit with passkey" }));
  expect(screen.getByText(/multica guard plugin set --file/)).toBeVisible();
});

it("shows an error with retry when the counts cannot load", async () => {
  stats.mockRejectedValueOnce(new Error("502"));
  mount();
  const alert = await screen.findByRole("alert");
  fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
  expect(await screen.findByRole("list", { name: "Guard plugins" })).toBeVisible();
});

it("shows the empty state when no plugin is stored and no hit was seen", async () => {
  settings = {};
  stats.mockResolvedValue({ days: 7, tz: "UTC", since: "2026-11-08", plugins: [] });
  mount();
  expect(await screen.findByText("No guard plugins yet")).toBeVisible();
  expect(screen.getByText("Built-in defaults")).toBeVisible();
});

it("shows an empty table row when a plugin has no decisions", async () => {
  decisions.mockResolvedValue([]);
  mount();
  expect(await screen.findByText("No decisions in the last 7 days.")).toBeVisible();
});

import { beforeEach, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api, ApiError } from "@multica/core/api";
import { signPasskeyChallenge } from "@multica/core/auth/webauthn";
import { renderWithI18n } from "../../test/i18n";
import { useGuardEditDraftStore } from "@multica/core/settings/draft-stores";
import { GuardTab } from "./guard-tab";

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { getGuardStats: vi.fn(), listGuardDecisions: vi.fn(), getGuardPolicy: vi.fn(), challengeGuardPolicy: vi.fn(), putGuardPolicy: vi.fn() },
}));
vi.mock("@multica/core/auth/webauthn", () => ({ signPasskeyChallenge: vi.fn() }));
let role: "owner" | "member" = "owner";
vi.mock("@multica/core/permissions", () => ({ useCurrentMember: () => ({ role }) }));
let settings: Record<string, unknown> = {};
let wsId = "ws";
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: wsId, name: "Cheese", settings }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => wsId }));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "Asia/Bangkok" }));

const stats = vi.mocked(api.getGuardStats);
const decisions = vi.mocked(api.listGuardDecisions);
const getPolicy = vi.mocked(api.getGuardPolicy);
const challenge = vi.mocked(api.challengeGuardPolicy);
const put = vi.mocked(api.putGuardPolicy);
const sign = vi.mocked(signPasskeyChallenge);

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

let client: QueryClient;
function mount() {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <GuardTab />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  useGuardEditDraftStore.setState({ drafts: {} });
  role = "owner";
  wsId = "ws";
  settings = { guard_policy: POLICY };
  getPolicy.mockReset().mockResolvedValue({ policy: POLICY, sha256: "1111aaaa" + "0".repeat(56) });
  challenge.mockReset().mockResolvedValue({ challenge_id: "c1", options: { publicKey: {} }, expires_at: "2026-11-15T00:00:00Z" });
  sign.mockReset().mockResolvedValue({ challenge_id: "c1", credential: { id: "cred" } });
  put.mockReset().mockImplementation(async (_ws, input) => ({ policy: input.policy as Record<string, unknown>, sha256: "2222bbbb" + "0".repeat(56) }));
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

it("points a member at the CLI instead of an editor", async () => {
  role = "member";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit with passkey" }));
  expect(screen.getByText(/multica guard plugin set --file/)).toBeVisible();
  expect(screen.getByText("Only owners edit workspace-level rules here.")).toBeVisible();
  expect(screen.queryByRole("combobox", { name: "Action" })).toBeNull();
});

async function openEditor(name = "ci-wait", view = mount()) {
  fireEvent.click(within(await screen.findByRole("list", { name: "Guard plugins" })).getByText(name));
  fireEvent.click(screen.getByRole("button", { name: "Edit with passkey" }));
  const action = await screen.findByRole("combobox", { name: "Action" });
  await waitFor(() => expect(action).toBeEnabled()); // the form unlocks once the base policy is loaded
  return { action, view };
}

const NEW_POLICY = {
  ...POLICY,
  plugins: POLICY.plugins.map((p) => (p.name === "ci-wait" ? { name: p.name, match: p.match, action: "pass", builtin: p.builtin, fail_closed: true } : p)),
};

it("lets an owner change a rule: challenge names the base, the passkey signs, the write follows", async () => {
  const { action } = await openEditor();
  fireEvent.change(action, { target: { value: "pass" } });
  fireEvent.click(screen.getByRole("button", { name: "Save with passkey" }));
  expect(await screen.findByText("Saved. Policy 2222bbbb is live.")).toBeVisible();
  const base = "1111aaaa" + "0".repeat(56);
  expect(challenge).toHaveBeenCalledWith("ws", { policy: NEW_POLICY, base_sha256: base });
  expect(sign).toHaveBeenCalledTimes(1);
  expect(put).toHaveBeenCalledWith("ws", { policy: NEW_POLICY, base_sha256: base, assertion: { challenge_id: "c1", credential: { id: "cred" } } });
});

it("keeps the edit open with one line when the passkey prompt is cancelled", async () => {
  sign.mockRejectedValueOnce(new Error("cancelled"));
  const { action } = await openEditor();
  fireEvent.change(action, { target: { value: "pass" } });
  fireEvent.click(screen.getByRole("button", { name: "Save with passkey" }));
  expect(await screen.findByText("Passkey cancelled or timed out. Nothing was saved.")).toBeVisible();
  expect(put).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: "Save with passkey" })).toBeEnabled();
});

const STALE_SHA = "3333cccc" + "0".repeat(56);
function conflict() {
  const current = { ...POLICY, plugins: [...POLICY.plugins, { name: "extra", match: { tool: "x" }, action: "pass" }] };
  const body = { error: "stale_base: changed", code: "stale_base", current_sha256: STALE_SHA, policy: current };
  return { current, error: new ApiError("stale_base: changed", 409, "Conflict", body) };
}

it("shows the Reapply screen on a 409 and re-applies the change on the current policy", async () => {
  const { current, error } = conflict();
  challenge.mockRejectedValueOnce(error);
  const { action } = await openEditor();
  fireEvent.change(action, { target: { value: "pass" } });
  fireEvent.click(screen.getByRole("button", { name: "Save with passkey" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Not saved. The guard policy changed while you edited: 1111aaaa → 3333cccc.");
  expect(screen.getByText("ci-wait: Deny → Pass, workspace scope.")).toBeVisible();
  expect(put).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Re-apply on 3333cccc with passkey" }));
  expect(await screen.findByText("Saved. Policy 2222bbbb is live.")).toBeVisible();
  const reapplied = { ...current, plugins: current.plugins.map((p) => (p.name === "ci-wait" ? NEW_POLICY.plugins[1] : p)) };
  expect(challenge).toHaveBeenLastCalledWith("ws", { policy: reapplied, base_sha256: STALE_SHA });
  expect(put).toHaveBeenCalledWith("ws", expect.objectContaining({ policy: reapplied, base_sha256: STALE_SHA }));
});

it("drops the pending change on Discard and reloads the current values on Reload", async () => {
  const { error } = conflict();
  challenge.mockRejectedValue(error);
  const { action } = await openEditor();
  fireEvent.change(action, { target: { value: "pass" } });
  fireEvent.click(screen.getByRole("button", { name: "Save with passkey" }));
  fireEvent.click(await screen.findByRole("button", { name: "Reload current" }));
  expect(screen.getByRole("combobox", { name: "Action" })).toHaveValue("deny");
  expect(screen.queryByRole("alert")).toBeNull();
  fireEvent.change(screen.getByRole("combobox", { name: "Action" }), { target: { value: "pass" } });
  fireEvent.click(screen.getByRole("button", { name: "Save with passkey" }));
  fireEvent.click(await screen.findByRole("button", { name: "Discard" }));
  expect(screen.queryByRole("combobox", { name: "Action" })).toBeNull();
  expect(put).not.toHaveBeenCalled();
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

it("lists a layer's plugin next to the workspace one with the same name, and picks each on its own", async () => {
  settings = {
    guard_policy: {
      version: 3,
      plugins: [{ name: "ci-wait", match: { tool: "gh" }, action: "deny" }],
      layers: [{ scope: "repo", repo: "cheese-work/multica", locked: true, plugins: [{ name: "ci-wait", match: { tool: "gh" }, action: "pass" }] }],
    },
  };
  stats.mockResolvedValue({ days: 7, tz: "Asia/Bangkok", since: "2026-11-08", plugins: [{ plugin: "ci-wait", fixed: 0, refused: 18, errors: 0, shadow_hits: 0, passes: 0, daily: [] }] });
  mount();
  const list = await screen.findByRole("list", { name: "Guard plugins" });
  const items = within(list).getAllByRole("listitem");
  expect(items).toHaveLength(2);
  expect(within(items[0]!).getByText("Workspace")).toBeVisible();
  expect(within(items[1]!).getByText("Repository cheese-work/multica")).toBeVisible();
  expect(within(items[1]!).getByText("Locked")).toBeVisible();
  fireEvent.click(within(items[1]!).getByRole("button"));
  expect(within(items[1]!).getByRole("button")).toHaveAttribute("aria-current", "true");
  expect(within(items[0]!).getByRole("button")).not.toHaveAttribute("aria-current");
  expect(screen.getByText("Lets the call through.")).toBeVisible();
  expect(screen.getAllByText("Counts are shared by every plugin with this name.").length).toBeGreaterThan(0);
});

it("shows the review policy when the policy has review rules and no plugins", async () => {
  settings = { guard_policy: { version: 5, review: [{ repo: "cheese-work/multica", reviewers: ["a", "b"] }] } };
  stats.mockResolvedValue({ days: 7, tz: "Asia/Bangkok", since: "2026-11-08", plugins: [] });
  mount();
  expect(await screen.findByText("Review policy · 1 repositories")).toBeVisible();
  expect(screen.getByText("cheese-work/multica")).toBeVisible();
  expect(screen.getByText("No guard plugins yet")).toBeVisible();
});

it("shows review rules from a layer", async () => {
  settings = { guard_policy: { version: 5, layers: [{ scope: "org", org: "tevo-studio", review: [{ repo: "tevo-studio/*", github: "skip" }] }] } };
  stats.mockResolvedValue({ days: 7, tz: "Asia/Bangkok", since: "2026-11-08", plugins: [] });
  mount();
  expect(await screen.findByText("tevo-studio/*")).toBeVisible();
  expect(screen.getByText(/^Organization tevo-studio/)).toBeVisible();
});

const OTHER_OWNER_SHA = "4444dddd" + "0".repeat(56);
const OTHER_OWNER_POLICY = { ...POLICY, plugins: POLICY.plugins.map((p) => (p.name === "ci-wait" ? { ...p, action: "shadow" } : p)) };

it("keeps the signed base while a draft is open: a refresh does not turn another owner's change into a silent overwrite", async () => {
  const { action } = await openEditor();
  fireEvent.change(action, { target: { value: "pass" } });
  getPolicy.mockResolvedValue({ policy: OTHER_OWNER_POLICY, sha256: OTHER_OWNER_SHA });
  await act(async () => {
    await client.invalidateQueries({ queryKey: ["guard", "ws", "policy"] });
  });
  await waitFor(() => expect(getPolicy).toHaveBeenCalledTimes(2));
  fireEvent.click(screen.getByRole("button", { name: "Save with passkey" }));
  await waitFor(() => expect(challenge).toHaveBeenCalledTimes(1));
  expect(challenge).toHaveBeenCalledWith("ws", { policy: NEW_POLICY, base_sha256: "1111aaaa" + "0".repeat(56) });
});

it("keeps the unsent edit and its base when the editor closes and opens again", async () => {
  const { action, view } = await openEditor();
  fireEvent.change(action, { target: { value: "pass" } });
  fireEvent.change(screen.getByRole("textbox", { name: "Message to agent" }), { target: { value: "kept" } });
  view.unmount();
  getPolicy.mockResolvedValue({ policy: OTHER_OWNER_POLICY, sha256: OTHER_OWNER_SHA });
  const again = await openEditor("ci-wait", mount());
  expect(again.action).toHaveValue("pass");
  expect(screen.getByRole("textbox", { name: "Message to agent" })).toHaveValue("kept");
  fireEvent.click(screen.getByRole("button", { name: "Save with passkey" }));
  await waitFor(() => expect(challenge).toHaveBeenCalledTimes(1));
  expect(challenge.mock.calls[0]?.[1]).toMatchObject({ base_sha256: "1111aaaa" + "0".repeat(56) });
});

it("clears the unsent edit after a save and after Cancel", async () => {
  const first = await openEditor();
  fireEvent.change(first.action, { target: { value: "pass" } });
  fireEvent.click(screen.getByRole("button", { name: "Save with passkey" }));
  await screen.findByText("Saved. Policy 2222bbbb is live.");
  expect(useGuardEditDraftStore.getState().drafts).toEqual({});
  first.view.unmount();
  const second = await openEditor("ci-wait", mount());
  fireEvent.change(second.action, { target: { value: "shadow" } });
  expect(Object.keys(useGuardEditDraftStore.getState().drafts)).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(useGuardEditDraftStore.getState().drafts).toEqual({});
});

it("starts the form from the same fetched policy version it freezes, not from a stale workspace row", async () => {
  // The workspace settings (the row) still say Deny; the fetched version already says Watch.
  getPolicy.mockResolvedValue({ policy: OTHER_OWNER_POLICY, sha256: OTHER_OWNER_SHA });
  const { action } = await openEditor();
  expect(action).toHaveValue("shadow");
  fireEvent.change(screen.getByRole("textbox", { name: "Message to agent" }), { target: { value: "only the message" } });
  fireEvent.click(screen.getByRole("button", { name: "Save with passkey" }));
  await waitFor(() => expect(challenge).toHaveBeenCalledTimes(1));
  const [, input] = challenge.mock.calls[0] ?? [];
  expect(input?.base_sha256).toBe(OTHER_OWNER_SHA);
  const saved = (input?.policy as { plugins: { name: string; action: string; message?: string }[] }).plugins.find((p) => p.name === "ci-wait");
  expect(saved).toMatchObject({ action: "shadow", message: "only the message" });
});

it("keeps the unsent edit of one workspace apart from another workspace's edit of the same plugin", async () => {
  const a = await openEditor();
  fireEvent.change(a.action, { target: { value: "pass" } });
  a.view.unmount();
  wsId = "ws-b";
  const b = await openEditor("ci-wait", mount());
  expect(b.action).toHaveValue("deny"); // not the other workspace's unsent "pass"
  fireEvent.change(b.action, { target: { value: "shadow" } });
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(Object.keys(useGuardEditDraftStore.getState().drafts)).toHaveLength(1); // ws-a's draft survives
  b.view.unmount();
  wsId = "ws";
  const back = await openEditor("ci-wait", mount());
  expect(back.action).toHaveValue("pass");
});

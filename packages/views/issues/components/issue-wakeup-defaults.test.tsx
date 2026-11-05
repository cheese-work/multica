import { beforeEach, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api, ApiError } from "@multica/core/api";
import type { WakeupDefinition, WakeupEffectiveRule } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { NavigationProvider } from "../../navigation";
import { IssueWakeupDefaults } from "./issue-wakeup-defaults";

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: {
    listWakeupDefinitions: vi.fn(),
    getEffectiveWakeupRule: vi.fn(),
    saveWakeupDefinition: vi.fn(),
    deleteWakeupDefinition: vi.fn(),
  },
}));

// The shared wakeup wording reads the workspace and the viewer's time zone.
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("@multica/core/paths", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/paths")>();
  return { ...actual, useWorkspacePaths: () => actual.paths.workspace("acme") };
});
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "UTC" }));
vi.mock("./wakeup-condition-names", () => ({
  useConditionNames: () => ({ status: (key: string) => key, label: () => undefined, property: () => undefined, actor: (_type: string, id: string) => id }),
}));

const list = vi.mocked(api.listWakeupDefinitions);
const effectiveRule = vi.mocked(api.getEffectiveWakeupRule);
const save = vi.mocked(api.saveWakeupDefinition);
const remove = vi.mocked(api.deleteWakeupDefinition);

const ISSUE = { kind: "issue", id: "issue-1" };
const open = { definition_writes: true, trigger_kinds: ["event"], fields: ["enabled", "instruction"] };
const closed = { definition_writes: false, trigger_kinds: [], fields: [] };

const inherited: WakeupEffectiveRule = {
  rule_key: "pr_merged", scope: "issue", applicable: true, inapplicable_reason: "", enabled: true,
  config: { v: 1, enabled: true, instruction: "Check the merge" },
  sources: { enabled: "workspace", instruction: "project" }, overrides: [], aggregate_caps: [],
  fingerprint: "f", execution: null, already_satisfied: null, redacted: false, capabilities: open,
};
const overrideDefinition: WakeupDefinition = {
  scope: "issue", scope_id: "issue-1", rule_key: "pr_merged", root: false, revision: "5",
  config: { v: 1, instruction: "Only look at docs" }, updated_at: null, redacted: false,
};
const overridden: WakeupEffectiveRule = {
  ...inherited, config: { v: 1, enabled: true, instruction: "Only look at docs" },
  sources: { enabled: "workspace", instruction: "issue" }, overrides: ["instruction"],
};

/** Every rule resolves like `inherited` unless a test gives `pr_merged` its own answer. */
function resolve(byKey: Record<string, WakeupEffectiveRule>) {
  effectiveRule.mockImplementation(async (_scope, key) => byKey[key] ?? { ...inherited, rule_key: key });
}

let projectId: string | undefined;
function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <NavigationProvider
        value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/issues/issue-1", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path: string) => path }}
      >
        <IssueWakeupDefaults workspaceId="ws" issueId="issue-1" projectId={projectId} />
      </NavigationProvider>
    </QueryClientProvider>,
  );
}
let mounted = false;
/** The row for one rule; the first call mounts the component after the test has set its mocks. */
function ruleItem(name: string) {
  if (!mounted) {
    mounted = true;
    mount();
  }
  return screen.findByRole("listitem", { name });
}

beforeEach(() => {
  mounted = false;
  projectId = undefined;
  list.mockReset().mockResolvedValue({ definitions: [], capabilities: open });
  effectiveRule.mockReset();
  resolve({});
  save.mockReset().mockResolvedValue(overrideDefinition as never);
  remove.mockReset().mockResolvedValue(undefined);
});

it("shows nothing while the server keeps definition writes closed", async () => {
  list.mockResolvedValue({ definitions: [], capabilities: closed });
  const { container } = mount();
  await waitFor(() => expect(list).toHaveBeenCalledWith(ISSUE));
  expect(container).toBeEmptyDOMElement();
});

it("shows an inherited rule with where it comes from, and offers Override", async () => {
  const item = await ruleItem("When the linked PR merges");
  expect(within(item).getByText("On")).toBeInTheDocument();
  expect(within(item).getByText("Inherited from workspace")).toBeInTheDocument();
  expect(within(item).getByText("Check the merge")).toBeInTheDocument();
  expect(within(item).getByRole("button", { name: "Override When the linked PR merges" })).toBeInTheDocument();
  expect(within(item).queryByRole("button", { name: /Reset/ })).not.toBeInTheDocument();
});

it("shows an override as set on this issue, and offers Reset", async () => {
  list.mockResolvedValue({ definitions: [overrideDefinition], capabilities: open });
  resolve({ pr_merged: overridden });
  const item = await ruleItem("When the linked PR merges");
  expect(within(item).getByText("Overridden on this issue")).toBeInTheDocument();
  expect(within(item).getByText("Only look at docs")).toBeInTheDocument();
  expect(within(item).getByRole("button", { name: "Reset When the linked PR merges" })).toBeInTheDocument();
  expect(within(item).queryByRole("button", { name: /^Override/ })).not.toBeInTheDocument();
});

it("Override sends only the fields the person changed, at revision 0", async () => {
  const user = userEvent.setup();
  const item = await ruleItem("When the linked PR merges");
  await user.click(within(item).getByRole("button", { name: "Override When the linked PR merges" }));
  const instruction = within(item).getByRole("textbox", { name: "What to do when woken" });
  expect(instruction).toHaveValue("Check the merge");
  await user.clear(instruction);
  await user.type(instruction, "Only look at docs");
  await user.click(within(item).getByRole("button", { name: "Save override" }));
  await waitFor(() =>
    expect(save).toHaveBeenCalledWith(ISSUE, "pr_merged", { revision: "0", config: { v: 1, instruction: "Only look at docs" } }),
  );
});

it("Override can turn the rule off for this issue alone", async () => {
  const user = userEvent.setup();
  const item = await ruleItem("When the linked PR merges");
  await user.click(within(item).getByRole("button", { name: "Override When the linked PR merges" }));
  await user.click(within(item).getByRole("switch", { name: "On for this issue" }));
  await user.click(within(item).getByRole("button", { name: "Save override" }));
  await waitFor(() => expect(save).toHaveBeenCalledWith(ISSUE, "pr_merged", { revision: "0", config: { v: 1, enabled: false } }));
});

it("editing an existing override keeps its other fields and its revision", async () => {
  const user = userEvent.setup();
  list.mockResolvedValue({
    definitions: [{ ...overrideDefinition, config: { v: 1, rate_limit: 3, instruction: "Only look at docs" } }],
    capabilities: open,
  });
  resolve({ pr_merged: overridden });
  const item = await ruleItem("When the linked PR merges");
  await user.click(within(item).getByRole("button", { name: "Edit override When the linked PR merges" }));
  const instruction = within(item).getByRole("textbox", { name: "What to do when woken" });
  await user.clear(instruction);
  await user.type(instruction, "Docs and tests");
  await user.click(within(item).getByRole("button", { name: "Save override" }));
  await waitFor(() =>
    expect(save).toHaveBeenCalledWith(ISSUE, "pr_merged", { revision: "5", config: { v: 1, rate_limit: 3, instruction: "Docs and tests" } }),
  );
});

it("Reset deletes the override at the revision it was read at", async () => {
  const user = userEvent.setup();
  list.mockResolvedValue({ definitions: [overrideDefinition], capabilities: open });
  resolve({ pr_merged: overridden });
  const item = await ruleItem("When the linked PR merges");
  await user.click(within(item).getByRole("button", { name: "Reset When the linked PR merges" }));
  await waitFor(() => expect(remove).toHaveBeenCalledWith(ISSUE, "pr_merged", "5"));
});

it("a failed Reset says so and keeps the override", async () => {
  const user = userEvent.setup();
  remove.mockRejectedValue(new ApiError("boom", 500, "Server Error"));
  list.mockResolvedValue({ definitions: [overrideDefinition], capabilities: open });
  resolve({ pr_merged: overridden });
  const item = await ruleItem("When the linked PR merges");
  await user.click(within(item).getByRole("button", { name: "Reset When the linked PR merges" }));
  expect(await within(item).findByRole("alert")).toHaveTextContent("Could not reset the rule.");
  expect(within(item).getByText("Overridden on this issue")).toBeInTheDocument();
});

it("tells a disabled rule apart from a paused one", async () => {
  resolve({
    pr_merged: { ...inherited, enabled: false, config: { v: 1, enabled: false, instruction: "Check the merge" } },
    pr_checks_failed: {
      ...inherited, rule_key: "pr_checks_failed",
      execution: { instance_id: "i1", enabled: true, paused_reason: "loop" },
    },
  });
  const disabled = await ruleItem("When the linked PR merges");
  expect(within(disabled).getByText("Disabled")).toBeInTheDocument();
  expect(within(disabled).queryByText(/Paused/)).not.toBeInTheDocument();
  const paused = await ruleItem("When PR checks fail");
  expect(within(paused).getByText(/Paused: it kept waking rules that woke it again/)).toBeInTheDocument();
  expect(within(paused).queryByText("Disabled")).not.toBeInTheDocument();
  expect(within(paused).queryByText("On")).not.toBeInTheDocument();
});

it("keeps the draft and says why when someone else changed the override first (409)", async () => {
  const user = userEvent.setup();
  save.mockRejectedValue(new ApiError("stale", 409, "Conflict"));
  const item = await ruleItem("When the linked PR merges");
  await user.click(within(item).getByRole("button", { name: "Override When the linked PR merges" }));
  const instruction = within(item).getByRole("textbox", { name: "What to do when woken" });
  await user.clear(instruction);
  await user.type(instruction, "My unsaved words");
  await user.click(within(item).getByRole("button", { name: "Save override" }));
  expect(await within(item).findByRole("alert")).toHaveTextContent("Someone changed this rule first. Your edits are kept; reload to see theirs.");
  expect(within(item).getByRole("textbox", { name: "What to do when woken" })).toHaveValue("My unsaved words");
});

it("is reachable and operable by keyboard alone", async () => {
  const user = userEvent.setup();
  const item = await ruleItem("When the linked PR merges");
  within(item).getByRole("button", { name: "Override When the linked PR merges" }).focus();
  await user.keyboard("{Enter}");
  const instruction = within(item).getByRole("textbox", { name: "What to do when woken" });
  expect(instruction).toHaveFocus();
  await user.tab();
  expect(within(item).getByRole("switch", { name: "On for this issue" })).toHaveFocus();
  await user.keyboard(" ");
  expect(within(item).getByRole("switch", { name: "On for this issue" })).not.toBeChecked();
  await user.tab();
  expect(within(item).getByRole("button", { name: "Save override" })).toHaveFocus();
  await user.tab();
  await user.keyboard("{Enter}");
  expect(within(item).queryByRole("textbox", { name: "What to do when woken" })).not.toBeInTheDocument();
});

it("groups the rules under one labelled list", async () => {
  await ruleItem("When the linked PR merges");
  const group = screen.getByRole("list", { name: "Default wakeups" });
  expect(within(group).getAllByRole("listitem")).toHaveLength(3);
});

it("links a project-sourced default to the project's wakeup rules", async () => {
  projectId = "proj-1";
  resolve({ pr_merged: { ...inherited, sources: { enabled: "project", instruction: "project" } } });
  const item = await ruleItem("When the linked PR merges");
  expect(within(item).getByRole("link", { name: "Edit in project rules" })).toHaveAttribute("href", "/acme/projects/proj-1?focus=wakeups");
});

it("offers no project link for a workspace default, an override, or an issue without a project", async () => {
  projectId = "proj-1";
  resolve({ pr_merged: inherited, pr_checks_failed: { ...inherited, rule_key: "pr_checks_failed", sources: { enabled: "project" } } });
  list.mockResolvedValue({ definitions: [{ ...overrideDefinition, rule_key: "pr_checks_failed" }], capabilities: open });
  const workspaceRule = await ruleItem("When the linked PR merges");
  expect(within(workspaceRule).queryByRole("link")).not.toBeInTheDocument();
  expect(within(await screen.findByRole("listitem", { name: /checks/i })).queryByRole("link")).not.toBeInTheDocument();
});

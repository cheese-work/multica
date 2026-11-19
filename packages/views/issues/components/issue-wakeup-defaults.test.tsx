import { beforeEach, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api, ApiError } from "@multica/core/api";
import type { WakeupDefinition, WakeupEffectiveRule } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { NavigationProvider } from "../../navigation";
import { useWakeupDefaults } from "./issue-wakeup-defaults";

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
/** The wiring `WakeupsSection` does: the button in a header, the panel under it, one key handler. */
function Harness() {
  const defaults = useWakeupDefaults({ workspaceId: "ws", issueId: "issue-1", projectId });
  if (!defaults.available) return null;
  return (
    <section onKeyDown={defaults.onKeyDown}>
      <header>{defaults.button}</header>
      {defaults.panel}
    </section>
  );
}
function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <NavigationProvider
        value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/acme/issues/issue-1", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (path: string) => path }}
      >
        <Harness />
      </NavigationProvider>
    </QueryClientProvider>,
  );
}
let mounted = false;
/** The row for one rule; the first call mounts the component and opens the panel. */
async function ruleItem(name: string) {
  if (!mounted) {
    mounted = true;
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /^Defaults/ }));
  }
  return screen.findByRole("listitem", { name });
}
/** One rule's more menu, opened: the second of the three clicks to an override. */
async function openMenu(item: HTMLElement, rule = "When the linked PR merges") {
  await userEvent.click(within(item).getByRole("button", { name: `More options for ${rule}` }));
}
async function pick(item: HTMLElement, label: string | RegExp, rule?: string) {
  await openMenu(item, rule);
  await userEvent.click(await screen.findByRole("menuitem", { name: label }));
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

it("shows one header button that toggles an inline panel and says so with aria-expanded", async () => {
  const user = userEvent.setup();
  mount();
  const button = await screen.findByRole("button", { name: "Defaults · 3 on" });
  const panelId = button.getAttribute("aria-controls");
  expect(panelId).toBeTruthy();
  expect(button).toHaveAttribute("aria-expanded", "false");
  expect(document.getElementById(panelId!)).not.toBeVisible();
  await user.click(button);
  expect(button).toHaveAttribute("aria-expanded", "true");
  expect(document.getElementById(panelId!)).toBeVisible();
  // Inline: no dialog, popover or sheet.
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  await user.click(button);
  expect(button).toHaveAttribute("aria-expanded", "false");
  expect(document.getElementById(panelId!)).not.toBeVisible();
});

it("Esc closes the panel and returns focus to the button", async () => {
  const user = userEvent.setup();
  const item = await ruleItem("When the linked PR merges");
  const button = screen.getByRole("button", { name: "Defaults · 3 on" });
  within(item).getByRole("button", { name: "More options for When the linked PR merges" }).focus();
  await user.keyboard("{Escape}");
  expect(button).toHaveAttribute("aria-expanded", "false");
  expect(button).toHaveFocus();
  // From the button itself too.
  await user.click(button);
  await user.keyboard("{Escape}");
  expect(button).toHaveAttribute("aria-expanded", "false");
  expect(button).toHaveFocus();
});

it("Esc closes an open menu first and leaves the panel open", async () => {
  const user = userEvent.setup();
  const item = await ruleItem("When the linked PR merges");
  await openMenu(item);
  await screen.findByRole("menu");
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("menu")).not.toBeInTheDocument());
  expect(screen.getByRole("button", { name: "Defaults · 3 on" })).toHaveAttribute("aria-expanded", "true");
});

it("Esc inside the override form keeps the panel and the draft", async () => {
  const user = userEvent.setup();
  const item = await ruleItem("When the linked PR merges");
  await pick(item, "Override");
  const instruction = within(item).getByRole("textbox", { name: "What to do when woken" });
  await user.type(instruction, " today");
  await user.keyboard("{Escape}");
  expect(screen.getByRole("button", { name: "Defaults · 3 on" })).toHaveAttribute("aria-expanded", "true");
  expect(within(item).getByRole("textbox", { name: "What to do when woken" })).toHaveValue("Check the merge today");
});

it("keeps the draft when the panel closes and opens again", async () => {
  const user = userEvent.setup();
  const item = await ruleItem("When the linked PR merges");
  await pick(item, "Override");
  await user.type(within(item).getByRole("textbox", { name: "What to do when woken" }), " today");
  const button = screen.getByRole("button", { name: "Defaults · 3 on" });
  await user.click(button);
  await user.click(button);
  expect(within(await screen.findByRole("listitem", { name: "When the linked PR merges" })).getByRole("textbox", { name: "What to do when woken" })).toHaveValue("Check the merge today");
});

it("lists one line per rule with its state, and no per-row Override button or inherited caption", async () => {
  const item = await ruleItem("When the linked PR merges");
  expect(within(item).getByText("On")).toBeInTheDocument();
  expect(within(item).queryByText(/Inherited from/)).not.toBeInTheDocument();
  expect(within(item).queryByText("Check the merge")).not.toBeInTheDocument();
  expect(within(item).queryByRole("button", { name: /^Override/ })).not.toBeInTheDocument();
  expect(screen.getAllByText("Inherited from the default unless noted")).toHaveLength(1);
});

it("Override takes three clicks from closed: open, more, Override", async () => {
  const user = userEvent.setup();
  mount();
  let clicks = 0;
  const click = async (el: HTMLElement) => {
    clicks += 1;
    await user.click(el);
  };
  await click(await screen.findByRole("button", { name: /^Defaults/ }));
  const item = await screen.findByRole("listitem", { name: "When the linked PR merges" });
  await click(within(item).getByRole("button", { name: "More options for When the linked PR merges" }));
  await click(await screen.findByRole("menuitem", { name: "Override" }));
  expect(clicks).toBe(3);
  expect(within(item).getByRole("textbox", { name: "What to do when woken" })).toHaveFocus();
});

it("the menu of an inherited rule offers Override and a disabled Reset to default", async () => {
  const item = await ruleItem("When the linked PR merges");
  await openMenu(item);
  expect(await screen.findByRole("menuitem", { name: "Override" })).toBeInTheDocument();
  expect(screen.getByRole("menuitem", { name: "Reset to default" })).toHaveAttribute("aria-disabled", "true");
  expect(screen.queryByRole("menuitem", { name: "Edit override" })).not.toBeInTheDocument();
});

it("the menu of an overridden rule offers Edit override and Reset to default, with an Edited tag", async () => {
  list.mockResolvedValue({ definitions: [overrideDefinition], capabilities: open });
  resolve({ pr_merged: overridden });
  const item = await ruleItem("When the linked PR merges");
  expect(within(item).getByText("Edited")).toBeInTheDocument();
  await openMenu(item);
  expect(await screen.findByRole("menuitem", { name: "Edit override" })).toBeInTheDocument();
  expect(screen.getByRole("menuitem", { name: "Reset to default" })).not.toHaveAttribute("aria-disabled", "true");
  expect(screen.queryByRole("menuitem", { name: "Override" })).not.toBeInTheDocument();
});

it("Override sends only the fields the person changed, at revision 0", async () => {
  const user = userEvent.setup();
  const item = await ruleItem("When the linked PR merges");
  await pick(item, "Override");
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
  await pick(item, "Override");
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
  await pick(item, "Edit override");
  const instruction = within(item).getByRole("textbox", { name: "What to do when woken" });
  await user.clear(instruction);
  await user.type(instruction, "Docs and tests");
  await user.click(within(item).getByRole("button", { name: "Save override" }));
  await waitFor(() =>
    expect(save).toHaveBeenCalledWith(ISSUE, "pr_merged", { revision: "5", config: { v: 1, rate_limit: 3, instruction: "Docs and tests" } }),
  );
});

it("closing the override form returns focus to the rule's more button", async () => {
  const user = userEvent.setup();
  const item = await ruleItem("When the linked PR merges");
  await pick(item, "Override");
  await user.click(within(item).getByRole("button", { name: "Cancel" }));
  expect(within(item).getByRole("button", { name: "More options for When the linked PR merges" })).toHaveFocus();
});

it("Reset to default deletes the override at the revision it was read at", async () => {
  list.mockResolvedValue({ definitions: [overrideDefinition], capabilities: open });
  resolve({ pr_merged: overridden });
  const item = await ruleItem("When the linked PR merges");
  await pick(item, "Reset to default");
  await waitFor(() => expect(remove).toHaveBeenCalledWith(ISSUE, "pr_merged", "5"));
});

it("a failed Reset says so and keeps the override", async () => {
  remove.mockRejectedValue(new ApiError("boom", 500, "Server Error"));
  list.mockResolvedValue({ definitions: [overrideDefinition], capabilities: open });
  resolve({ pr_merged: overridden });
  const item = await ruleItem("When the linked PR merges");
  await pick(item, "Reset to default");
  expect(await within(item).findByRole("alert")).toHaveTextContent("Could not reset the rule.");
  expect(within(item).getByText("Edited")).toBeInTheDocument();
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

it("a paused rule turns the button warm, with an icon and the word paused", async () => {
  resolve({
    pr_checks_failed: { ...inherited, rule_key: "pr_checks_failed", execution: { instance_id: "i1", enabled: true, paused_reason: "loop" } },
  });
  mount();
  const button = await screen.findByRole("button", { name: "Defaults · 1 paused" });
  expect(button).toHaveTextContent("paused");
  expect(button).toHaveClass("text-warning");
  expect(button.querySelector('svg[aria-hidden="true"]')).not.toBeNull();
});

it("an edited default adds an edited suffix to the button, with an icon and the word", async () => {
  list.mockResolvedValue({ definitions: [overrideDefinition], capabilities: open });
  resolve({ pr_merged: overridden });
  mount();
  const button = await screen.findByRole("button", { name: "Defaults · 3 on, 1 edited" });
  expect(button.querySelectorAll('svg[aria-hidden="true"]').length).toBeGreaterThanOrEqual(2);
});

it("keeps the draft and says why when someone else changed the override first (409)", async () => {
  const user = userEvent.setup();
  save.mockRejectedValue(new ApiError("stale", 409, "Conflict"));
  const item = await ruleItem("When the linked PR merges");
  await pick(item, "Override");
  const instruction = within(item).getByRole("textbox", { name: "What to do when woken" });
  await user.clear(instruction);
  await user.type(instruction, "My unsaved words");
  await user.click(within(item).getByRole("button", { name: "Save override" }));
  expect(await within(item).findByRole("alert")).toHaveTextContent("Someone changed this rule first. Your edits are kept; reload to see theirs.");
  expect(within(item).getByRole("textbox", { name: "What to do when woken" })).toHaveValue("My unsaved words");
});

it("is reachable and operable by keyboard alone", async () => {
  const user = userEvent.setup();
  mount();
  const button = await screen.findByRole("button", { name: /^Defaults/ });
  button.focus();
  await user.keyboard("{Enter}");
  const item = await screen.findByRole("listitem", { name: "When the linked PR merges" });
  await user.tab();
  expect(within(item).getByRole("button", { name: "More options for When the linked PR merges" })).toHaveFocus();
  await user.keyboard("{Enter}");
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

it("keeps 40 px targets at phone width for the header button and each more button", async () => {
  const item = await ruleItem("When the linked PR merges");
  expect(screen.getByRole("button", { name: /^Defaults/ })).toHaveClass("min-h-10");
  expect(within(item).getByRole("button", { name: /^More options/ })).toHaveClass("size-10");
});

it("groups the rules under one labelled list", async () => {
  await ruleItem("When the linked PR merges");
  const group = screen.getByRole("list", { name: "Default wakeups" });
  expect(within(group).getAllByRole("listitem")).toHaveLength(3);
});

it("offers Open project for a project-sourced default, linking to the project's wakeup rules", async () => {
  projectId = "proj-1";
  resolve({ pr_merged: { ...inherited, sources: { enabled: "project", instruction: "project" } } });
  const item = await ruleItem("When the linked PR merges");
  expect(within(item).getByText("Project")).toBeInTheDocument();
  await openMenu(item);
  expect(await screen.findByRole("menuitem", { name: "Open project" })).toHaveAttribute("href", "/acme/projects/proj-1?focus=wakeups");
});

it("offers no Open project for a workspace default, an override, or an issue without a project", async () => {
  projectId = "proj-1";
  resolve({ pr_merged: inherited, pr_checks_failed: { ...inherited, rule_key: "pr_checks_failed", sources: { enabled: "project" } } });
  list.mockResolvedValue({ definitions: [{ ...overrideDefinition, rule_key: "pr_checks_failed" }], capabilities: open });
  const workspaceRule = await ruleItem("When the linked PR merges");
  await openMenu(workspaceRule);
  await screen.findByRole("menuitem", { name: "Override" });
  expect(screen.queryByRole("menuitem", { name: "Open project" })).not.toBeInTheDocument();
  await userEvent.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("menu")).not.toBeInTheDocument());
  const overriddenRule = await screen.findByRole("listitem", { name: /checks/i });
  await openMenu(overriddenRule, "When PR checks fail");
  await screen.findByRole("menuitem", { name: "Edit override" });
  expect(screen.queryByRole("menuitem", { name: "Open project" })).not.toBeInTheDocument();
});

it("shows a redacted rule with no controls", async () => {
  resolve({ pr_merged: { ...inherited, redacted: true } });
  const item = await ruleItem("When the linked PR merges");
  expect(within(item).queryByRole("button")).not.toBeInTheDocument();
});

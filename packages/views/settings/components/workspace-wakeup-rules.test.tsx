import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { useWakeupRuleDraftStore } from "@multica/core/settings/draft-stores";
import { beforeEach, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api, ApiError } from "@multica/core/api";
import type { WakeupDefinition } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { WorkspaceWakeupRules } from "./workspace-wakeup-rules";

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: {
    listWakeupDefinitions: vi.fn(),
    previewWakeupDefinition: vi.fn(),
    createWakeupDefinition: vi.fn(),
    deleteWakeupDefinition: vi.fn(),
    saveWakeupDefinition: vi.fn(),
  },
}));
const properties = vi.hoisted(() => ({ current: [] as { id: string; type: string }[], value: "" }));
vi.mock("@multica/core/properties/queries", () => ({
  propertyListOptions: () => ({ queryKey: ["properties", "ws"], queryFn: async () => properties.current }),
}));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "UTC" }));
vi.mock("../../issues/components/wakeup-condition-names", () => ({
  useConditionNames: () => ({ status: (key: string) => key, label: () => undefined, property: () => undefined, actor: (_type: string, id: string) => id }),
}));
// The shared fields have their own tests; here a stub fills the draft they own.
vi.mock("../../issues/components/wakeup-fields", () => ({
  WakeupFields: ({ draft, update, scope }: { draft: { instruction: string; agentId: string; waitDays: number }; update: (patch: object) => void; scope: { issueId?: string; definition?: boolean } }) => (
    <>
      <button
        type="button"
        data-scope={scope.issueId ? "issue" : scope.definition ? "definition" : "plain"}
        onClick={() => update({ condition: "custom", events: ["issue.updated"], agentId: "agent-1", instruction: "Look at it" })}
      >
        Fill fields
      </button>
      <button
        type="button"
        onClick={() =>
          update({ condition: "field", field: "property", fieldTarget: "p1", fieldValue: properties.value, agentId: "agent-1", instruction: "Look at it" })
        }
      >
        Fill property
      </button>
      <output>{`draft:${draft.agentId}|${draft.instruction}|${draft.waitDays}`}</output>
    </>
  ),
}));

const list = vi.mocked(api.listWakeupDefinitions);
const preview = vi.mocked(api.previewWakeupDefinition);
const create = vi.mocked(api.createWakeupDefinition);
const remove = vi.mocked(api.deleteWakeupDefinition);
const save = vi.mocked(api.saveWakeupDefinition);

const open = { definition_writes: true, trigger_kinds: ["event"], fields: ["name"] };
const closed = { definition_writes: false, trigger_kinds: [], fields: [] };
const existing: WakeupDefinition = {
  scope: "workspace", scope_id: "ws", rule_key: "6f1a2b3c-4d5e-4f60-8a7b-9c0d1e2f3a4b", root: true, revision: "3",
  config: { v: 1, name: "Triage", instruction: "Triage it", trigger: { kind: "event" } }, updated_at: null, redacted: false,
};
// A rule the editor can write back: an event trigger with events, an agent, a wait from the offered choices.
const editable: WakeupDefinition = {
  ...existing, rule_key: "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", revision: "7",
  config: {
    v: 1, name: "Watch PRs", instruction: "Check the PR", trigger: { kind: "event", events: ["comment.created"] },
    target: { type: "agent", id: "agent-2" }, mode: "once", expiry: { after_seconds: 3 * 86400 }, rate_limit: 4, filters: { labels: ["a"] },
  },
};
const effective = {
  rule_key: "x", scope: "workspace", applicable: true, inapplicable_reason: "", enabled: true, config: { v: 1 }, sources: {},
  overrides: [], aggregate_caps: [{ scope: "workspace", scope_id: "ws", limit: 12 }], fingerprint: "f", execution: null,
  already_satisfied: { satisfied: 4, examined: 50, truncated: true }, redacted: false, capabilities: open,
};

function mount(canManage = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <WorkspaceWakeupRules workspaceId="ws" canManage={canManage} />
    </QueryClientProvider>,
  );
}
async function fillAndName() {
  fireEvent.click(await screen.findByRole("button", { name: "Fill fields" }));
  fireEvent.change(screen.getByRole("textbox", { name: "Rule name" }), { target: { value: "Watch updates" } });
}

beforeEach(() => {
  properties.current = [];
  list.mockReset().mockResolvedValue({ definitions: [existing], capabilities: open });
  preview.mockReset().mockResolvedValue(effective as never);
  create.mockReset().mockResolvedValue(existing as never);
  remove.mockReset().mockResolvedValue(undefined);
  save.mockReset().mockResolvedValue(editable as never);
});

it("shows nothing while the server keeps definition writes closed", async () => {
  list.mockResolvedValue({ definitions: [], capabilities: closed });
  const { container } = mount();
  await waitFor(() => expect(list).toHaveBeenCalledWith({ kind: "workspace" }));
  expect(container).toBeEmptyDOMElement();
});

it("shows nothing against an older server that has no definition endpoint", async () => {
  list.mockRejectedValue(new ApiError("not found", 404, "Not Found"));
  const { container } = mount();
  await waitFor(() => expect(list).toHaveBeenCalled());
  expect(container).toBeEmptyDOMElement();
});

it("lists the workspace rules and the editor once writes are open", async () => {
  mount();
  expect(await screen.findByText("Triage")).toBeVisible();
  expect(screen.getByRole("button", { name: "Fill fields" })).toHaveAttribute("data-scope", "definition");
});

it("previews the draft with the issues it would meet at once and the cap that applies", async () => {
  mount();
  await fillAndName();
  fireEvent.click(screen.getByRole("button", { name: "Preview" }));
  expect(await screen.findByText("4 of the first 50 issues already match; enabling the rule would fire for them at once.")).toBeVisible();
  expect(screen.getByText("Limit: 12 wakeups per hour for the workspace.")).toBeVisible();
  const [scope, body] = preview.mock.calls[0]!;
  expect(scope).toEqual({ kind: "workspace" });
  expect(body.config).toMatchObject({ name: "Watch updates", trigger: { kind: "event", events: ["issue.updated"] }, target: { type: "agent", id: "agent-1" } });
  expect(create).not.toHaveBeenCalled();
});

it("saves through one call and refreshes the list", async () => {
  mount();
  await fillAndName();
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  expect(create.mock.calls[0]![0]).toEqual({ kind: "workspace" });
  expect(create.mock.calls[0]![1].config).toMatchObject({ name: "Watch updates", instruction: "Look at it" });
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
});

it("keeps the draft and shows why the server refused", async () => {
  create.mockRejectedValue(new ApiError("closed", 403, "Forbidden", { code: "wakeup_definition_writes_closed" }));
  mount();
  await fillAndName();
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Wakeup rules are not open for editing yet.");
  expect(screen.getByRole("textbox", { name: "Rule name" })).toHaveValue("Watch updates");
});

it("asks for a name before it calls the server", async () => {
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Fill fields" }));
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Name the rule (up to 120 characters).");
  expect(create).not.toHaveBeenCalled();
});

it("deletes a rule at the revision it was read at", async () => {
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Delete Triage" }));
  await waitFor(() => expect(remove).toHaveBeenCalledWith({ kind: "workspace" }, existing.rule_key, "3"));
});

it("is read-only for members who cannot manage the workspace", async () => {
  mount(false);
  expect(await screen.findByText("Triage")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Create rule" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Delete Triage" })).toBeNull();
});

async function openEditor() {
  fireEvent.click(await screen.findByRole("button", { name: "Edit Watch PRs" }));
  return within(screen.getByRole("form", { name: "Edit workspace rule" }));
}

it("opens a saved rule with its values in the shared fields", async () => {
  list.mockResolvedValue({ definitions: [editable], capabilities: open });
  mount();
  const form = await openEditor();
  expect(form.getByRole("textbox", { name: "Rule name" })).toHaveValue("Watch PRs");
  expect(form.getByText("draft:agent-2|Check the PR|3")).toBeVisible();
});

it("saves an edit at the revision it was read at and keeps the fields the editor does not show", async () => {
  list.mockResolvedValue({ definitions: [editable], capabilities: open });
  mount();
  const form = await openEditor();
  fireEvent.change(form.getByRole("textbox", { name: "Rule name" }), { target: { value: "Watch PR comments" } });
  fireEvent.click(form.getByRole("button", { name: "Save rule" }));
  await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
  const [scope, ruleKey, body] = [save.mock.calls[0]![0], save.mock.calls[0]![1], save.mock.calls[0]![2]];
  expect(scope).toEqual({ kind: "workspace" });
  expect(ruleKey).toBe(editable.rule_key);
  expect(body.revision).toBe("7");
  expect(body.config).toMatchObject({
    name: "Watch PR comments", instruction: "Check the PR", rate_limit: 4, filters: { labels: ["a"] },
    trigger: { kind: "event", events: ["comment.created"] }, expiry: { after_seconds: 3 * 86400 },
  });
  expect(create).not.toHaveBeenCalled();
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(screen.queryByRole("form", { name: "Edit workspace rule" })).toBeNull());
});

it("previews the same configuration it would save, with the fields the editor does not show", async () => {
  list.mockResolvedValue({ definitions: [editable], capabilities: open });
  mount();
  const form = await openEditor();
  fireEvent.click(form.getByRole("button", { name: "Preview" }));
  await waitFor(() => expect(preview).toHaveBeenCalledTimes(1));
  expect(preview.mock.calls[0]![1].rule_key).toBe(editable.rule_key);
  fireEvent.click(form.getByRole("button", { name: "Save rule" }));
  await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
  expect(preview.mock.calls[0]![1].config).toEqual(save.mock.calls[0]![2].config);
  expect(preview.mock.calls[0]![1].config).toMatchObject({ rate_limit: 4, filters: { labels: ["a"] } });
});

it("keeps the revision the edit opened at when the list refetches after someone else saved", async () => {
  save.mockRejectedValueOnce(new ApiError("stale", 409, "Conflict"));
  list
    .mockResolvedValueOnce({ definitions: [editable], capabilities: open })
    .mockResolvedValue({
      definitions: [{ ...editable, revision: "8", config: { ...editable.config, instruction: "Their change" } }],
      capabilities: open,
    });
  mount();
  const form = await openEditor();
  fireEvent.click(form.getByRole("button", { name: "Save rule" }));
  expect(await form.findByRole("alert")).toHaveTextContent("Someone else changed this rule.");
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  fireEvent.click(form.getByRole("button", { name: "Save rule" }));
  await waitFor(() => expect(save).toHaveBeenCalledTimes(2));
  // The retry carries the revision the draft was read at, so the server refuses it again
  // instead of writing the old draft over the newer rule.
  expect(save.mock.calls[1]![2].revision).toBe("7");
  expect(save.mock.calls[1]![2].config.instruction).toBe("Check the PR");
});

it("keeps the edit open and says so when someone else saved the rule first", async () => {
  save.mockRejectedValue(new ApiError("stale", 409, "Conflict"));
  list.mockResolvedValue({ definitions: [editable], capabilities: open });
  mount();
  const form = await openEditor();
  fireEvent.click(form.getByRole("button", { name: "Save rule" }));
  expect(await form.findByRole("alert")).toHaveTextContent("Someone else changed this rule.");
  expect(form.getByRole("textbox", { name: "Rule name" })).toHaveValue("Watch PRs");
});

it("leaves a rule the editor cannot write back unedited", async () => {
  mount();
  expect(await screen.findByText("Triage")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Edit Triage" })).toBeNull();
  expect(screen.getByText(/cannot change/)).toBeVisible();
  expect(screen.getByRole("button", { name: "Delete Triage" })).toBeVisible();
});

it("offers no edit to members who cannot manage the workspace", async () => {
  list.mockResolvedValue({ definitions: [editable], capabilities: open });
  mount(false);
  expect(await screen.findByText("Watch PRs")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Edit Watch PRs" })).toBeNull();
});

it.each([
  ["number", "5", 5],
  ["checkbox", "true", true],
])("previews and saves a %s property condition with the typed value", async (type, raw, typed) => {
  properties.current = [{ id: "p1", type }];
  properties.value = raw;
  mount();
  const fill = await screen.findByRole("button", { name: "Fill property" });
  fireEvent.click(fill);
  fireEvent.change(screen.getByRole("textbox", { name: "Rule name" }), { target: { value: "By property" } });
  fireEvent.click(screen.getByRole("button", { name: "Preview" }));
  await waitFor(() => expect(preview).toHaveBeenCalledTimes(1));
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  const trigger = (config: { trigger?: unknown }) => config.trigger;
  expect(trigger(preview.mock.calls[0]![1].config)).toMatchObject({ condition: { value: typed } });
  expect(trigger(create.mock.calls[0]![1].config)).toEqual(trigger(preview.mock.calls[0]![1].config));
});

it("restores the unsent new rule name and fields after a remount (CHE-3527)", async () => {
  const first = mount();
  await fillAndName();
  first.unmount();

  mount();
  expect(await screen.findByRole("textbox", { name: "Rule name" })).toHaveValue("Watch updates");
  expect(screen.getByText("draft:agent-1|Look at it|7")).toBeInTheDocument();
});

it("locks the form while a rule is being created, so a late edit cannot be lost (CHE-3527)", async () => {
  let finish!: () => void;
  create.mockReturnValue(new Promise((r) => (finish = () => r(existing as never))) as never);
  mount();
  await fillAndName();
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));

  await waitFor(() => expect(screen.getByRole("textbox", { name: "Rule name" })).toBeDisabled());
  expect(screen.getByRole("button", { name: "Fill fields" })).toBeDisabled();
  finish();
  await waitFor(() => expect(screen.getByRole("textbox", { name: "Rule name" })).toHaveValue(""));
});

it("shows a draft that reaches the store after the form mounted (CHE-3527)", async () => {
  mount();
  expect(await screen.findByRole("textbox", { name: "Rule name" })).toHaveValue("");
  act(() => useWakeupRuleDraftStore.getState().setDraft("workspace:ws:new", { name: "Late draft" }));
  expect(screen.getByRole("textbox", { name: "Rule name" })).toHaveValue("Late draft");
});

beforeEach(() => resetAllRegisteredDrafts());

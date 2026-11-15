import { beforeEach, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api, ApiError } from "@multica/core/api";
import type { WakeupDefinition } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { NavigationProvider } from "../../navigation";
import { ProjectWakeupsSection } from "./project-wakeups-section";

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
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "UTC" }));
// The shared fields have their own tests; here a stub fills the draft they own.
vi.mock("../../issues/components/wakeup-fields", () => ({
  WakeupFields: ({ draft, update, scope }: { draft: { instruction: string; agentId: string }; update: (patch: object) => void; scope: { definition?: boolean } }) => (
    <>
      <button
        type="button"
        data-scope={scope.definition ? "definition" : "plain"}
        onClick={() => update({ condition: "custom", events: ["issue.updated"], agentId: "agent-1", instruction: "Look at it" })}
      >
        Fill fields
      </button>
      <output>{`draft:${draft.agentId}|${draft.instruction}`}</output>
    </>
  ),
}));

const list = vi.mocked(api.listWakeupDefinitions);
const preview = vi.mocked(api.previewWakeupDefinition);
const create = vi.mocked(api.createWakeupDefinition);
const remove = vi.mocked(api.deleteWakeupDefinition);
const save = vi.mocked(api.saveWakeupDefinition);

const PROJECT = { kind: "project", id: "proj-1" };
const open = { definition_writes: true, trigger_kinds: ["event"], fields: ["name"] };
const closed = { definition_writes: false, trigger_kinds: [], fields: [] };
const editable: WakeupDefinition = {
  scope: "project", scope_id: "proj-1", rule_key: "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", root: true, revision: "7",
  config: {
    v: 1, name: "Watch PRs", instruction: "Check the PR", trigger: { kind: "event", events: ["comment.created"] },
    target: { type: "agent", id: "agent-2" }, mode: "once", expiry: { after_seconds: 3 * 86400 }, rate_limit: 4,
  },
  updated_at: null, redacted: false,
};
const effective = {
  rule_key: "x", scope: "project", applicable: true, inapplicable_reason: "", enabled: true, config: { v: 1 }, sources: {},
  overrides: [], aggregate_caps: [
    { scope: "workspace", scope_id: "ws", limit: 12 },
    { scope: "project", scope_id: "proj-1", limit: 5 },
  ], fingerprint: "f", execution: null, already_satisfied: null, redacted: false, capabilities: open,
};

function mount(canManage = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <ProjectWakeupsSection projectId="proj-1" canManage={canManage} />
    </QueryClientProvider>,
  );
}
async function expand() {
  fireEvent.click(await screen.findByRole("button", { name: "Wakeups" }));
}

beforeEach(() => {
  list.mockReset().mockResolvedValue({ definitions: [editable], capabilities: open });
  preview.mockReset().mockResolvedValue(effective as never);
  create.mockReset().mockResolvedValue(editable as never);
  remove.mockReset().mockResolvedValue(undefined);
  save.mockReset().mockResolvedValue(editable as never);
});

it("shows nothing while the server keeps definition writes closed", async () => {
  list.mockResolvedValue({ definitions: [], capabilities: closed });
  const { container } = mount();
  await waitFor(() => expect(list).toHaveBeenCalledWith(PROJECT));
  expect(container).toBeEmptyDOMElement();
});

it("shows nothing against an older server that has no definition endpoint", async () => {
  list.mockRejectedValue(new ApiError("not found", 404, "Not Found"));
  const { container } = mount();
  await waitFor(() => expect(list).toHaveBeenCalled());
  expect(container).toBeEmptyDOMElement();
});

it("lists this project's rules under a collapsed Wakeups heading", async () => {
  mount();
  const toggle = await screen.findByRole("button", { name: "Wakeups" });
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  expect(await screen.findByText("Watch PRs")).not.toBeVisible();
  fireEvent.click(toggle);
  expect(toggle).toHaveAttribute("aria-expanded", "true");
  expect(screen.getByText("Watch PRs")).toBeVisible();
  expect(screen.getByText("Project rules")).toBeVisible();
});

it("creates a rule in the project scope through the shared fields", async () => {
  list.mockResolvedValue({ definitions: [], capabilities: open });
  mount();
  await expand();
  expect(await screen.findByText("No project rules yet.")).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Fill fields" }));
  expect(screen.getByRole("button", { name: "Fill fields" })).toHaveAttribute("data-scope", "definition");
  fireEvent.change(screen.getByRole("textbox", { name: "Rule name" }), { target: { value: "Watch updates" } });
  fireEvent.click(screen.getByRole("button", { name: "Create rule" }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  expect(create.mock.calls[0]![0]).toEqual(PROJECT);
  expect(create.mock.calls[0]![1].config).toMatchObject({ name: "Watch updates", target: { type: "agent", id: "agent-1" } });
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
});

it("previews in the project scope and shows the project's cap, not the workspace's", async () => {
  list.mockResolvedValue({ definitions: [], capabilities: open });
  mount();
  await expand();
  fireEvent.click(await screen.findByRole("button", { name: "Fill fields" }));
  fireEvent.change(screen.getByRole("textbox", { name: "Rule name" }), { target: { value: "Watch updates" } });
  fireEvent.click(screen.getByRole("button", { name: "Preview" }));
  expect(await screen.findByText("Limit: 5 wakeups per hour for the project.")).toBeVisible();
  expect(screen.queryByText(/12 wakeups/)).toBeNull();
  expect(preview.mock.calls[0]![0]).toEqual(PROJECT);
});

async function openEditor() {
  await expand();
  fireEvent.click(await screen.findByRole("button", { name: "Edit Watch PRs" }));
  return within(screen.getByRole("form", { name: "Edit project rule" }));
}

it("saves an edit in the project scope at the revision it was read at and keeps the fields the editor does not show", async () => {
  mount();
  const form = await openEditor();
  expect(form.getByText("draft:agent-2|Check the PR")).toBeVisible();
  fireEvent.change(form.getByRole("textbox", { name: "Rule name" }), { target: { value: "Watch PR comments" } });
  fireEvent.click(form.getByRole("button", { name: "Save rule" }));
  await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
  expect(save.mock.calls[0]![0]).toEqual(PROJECT);
  expect(save.mock.calls[0]![1]).toBe(editable.rule_key);
  expect(save.mock.calls[0]![2].revision).toBe("7");
  expect(save.mock.calls[0]![2].config).toMatchObject({ name: "Watch PR comments", rate_limit: 4 });
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(screen.queryByRole("form", { name: "Edit project rule" })).toBeNull());
});

it("keeps the draft and the opened revision when someone else saved the rule first (409)", async () => {
  save.mockRejectedValueOnce(new ApiError("stale", 409, "Conflict"));
  list
    .mockResolvedValueOnce({ definitions: [editable], capabilities: open })
    .mockResolvedValue({
      definitions: [{ ...editable, revision: "8", config: { ...editable.config, instruction: "Their change" } }],
      capabilities: open,
    });
  mount();
  const form = await openEditor();
  fireEvent.change(form.getByRole("textbox", { name: "Rule name" }), { target: { value: "My draft name" } });
  fireEvent.click(form.getByRole("button", { name: "Save rule" }));
  expect(await form.findByRole("alert")).toHaveTextContent("Someone else changed this rule.");
  expect(form.getByRole("textbox", { name: "Rule name" })).toHaveValue("My draft name");
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  fireEvent.click(form.getByRole("button", { name: "Save rule" }));
  await waitFor(() => expect(save).toHaveBeenCalledTimes(2));
  // The retry still carries revision 7, so the server refuses it again instead of
  // writing the old draft over their newer rule.
  expect(save.mock.calls[1]![2].revision).toBe("7");
  expect(save.mock.calls[1]![2].config.name).toBe("My draft name");
});

it("keeps the open draft when the rule is deleted by someone else before the save (409, rule gone from the list)", async () => {
  save.mockRejectedValue(new ApiError("stale", 409, "Conflict"));
  list
    .mockResolvedValueOnce({ definitions: [editable], capabilities: open })
    .mockResolvedValue({ definitions: [], capabilities: open });
  mount();
  const form = await openEditor();
  fireEvent.change(form.getByRole("textbox", { name: "Rule name" }), { target: { value: "My draft name" } });
  fireEvent.click(form.getByRole("button", { name: "Save rule" }));
  expect(await form.findByRole("alert")).toHaveTextContent("Someone else changed this rule.");
  await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  // The list no longer has the rule; the editor and its draft stay until the user cancels.
  expect(screen.getByRole("form", { name: "Edit project rule" })).toBeVisible();
  expect(form.getByRole("textbox", { name: "Rule name" })).toHaveValue("My draft name");
  fireEvent.click(form.getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("form", { name: "Edit project rule" })).toBeNull());
  expect(screen.getByText("No project rules yet.")).toBeVisible();
});

it("deletes a rule at the revision it was read at", async () => {
  mount();
  await expand();
  fireEvent.click(await screen.findByRole("button", { name: "Delete Watch PRs" }));
  await waitFor(() => expect(remove).toHaveBeenCalledWith(PROJECT, editable.rule_key, "7"));
});

it("is read-only for members who cannot manage the project", async () => {
  mount(false);
  await expand();
  expect(await screen.findByText("Watch PRs")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Create rule" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Edit Watch PRs" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Delete Watch PRs" })).toBeNull();
});

it("opens the collapsed rules when the page is reached by the wakeups deep link, and drops the param", async () => {
  const replace = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithI18n(
    <QueryClientProvider client={client}>
      <NavigationProvider
        value={{ push: vi.fn(), replace, back: vi.fn(), pathname: "/acme/projects/proj-1", searchParams: new URLSearchParams("focus=wakeups"), hash: "", getShareableUrl: (path: string) => path }}
      >
        <ProjectWakeupsSection projectId="proj-1" canManage />
      </NavigationProvider>
    </QueryClientProvider>,
  );
  const toggle = await screen.findByRole("button", { name: "Wakeups" });
  await waitFor(() => expect(toggle).toHaveAttribute("aria-expanded", "true"));
  expect(replace).toHaveBeenCalledWith("/acme/projects/proj-1");
});

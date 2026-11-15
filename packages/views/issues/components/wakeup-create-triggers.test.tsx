import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useWakeupFormDraftStore } from "@multica/core/issues/stores";
import { configureShortcutPlatform } from "@multica/core/shortcuts";
import { renderWithI18n } from "../../test/i18n";
import { WakeupCreate } from "./wakeup-create";

// Pins the payload of every trigger kind the issue form can build, so the form
// can be split into shared fields without changing what it sends.
const create = vi.fn();
vi.mock("./wakeup-condition-names", () => ({
  useConditionNames: () => ({ status: (key: string) => key, label: () => undefined, property: () => undefined, actor: (_type: string, id: string) => id }),
}));
vi.mock("@multica/core/issues", () => ({
  useCreateIssueWakeup: () => ({ mutateAsync: create, isPending: false }),
  childIssuesOptions: () => ({ queryKey: ["children"] }),
  issueDetailOptions: () => ({ queryKey: ["issue-detail"] }),
}));
const data: Record<string, unknown> = {
  members: [{ user_id: "user-j", name: "Jiayuan" }],
  squads: [{ id: "squad-1", name: "Dev team" }],
  labels: [{ id: "label-1", name: "bug" }],
  properties: [
    { id: "prop-num", name: "Points", type: "number", archived: false, config: {} },
    { id: "prop-sel", name: "Size", type: "select", archived: false, config: { options: [{ id: "opt-l", name: "Large" }] } },
  ],
  children: [{ stage: 1 }, { stage: 2 }, { stage: null }],
  "issue-detail": { id: "issue", assignee_type: "agent", assignee_id: "emacs" },
};
vi.mock("@tanstack/react-query", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-query")>()),
  useQuery: ({ queryKey }: { queryKey: unknown[] }) => {
    const key = JSON.stringify(queryKey);
    const hit = Object.keys(data).find((name) => key.includes(name));
    return {
      data: hit
        ? data[hit]
        : [
            { id: "emacs", name: "Emacs", archived_at: null, runtime_id: "rt" },
            { id: "grok", name: "Grok", archived_at: null, runtime_id: "rt" },
          ],
    };
  },
}));
vi.mock("@multica/core/agents", () => ({ isAgentRuntimeBound: () => true }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => ({ activeStatuses: [{ key: "in_review" }, { key: "blocked" }] }),
}));
vi.mock("../utils/status-label", () => ({ useStatusLabel: () => (key: string) => key }));
vi.mock("../../modals/issue-picker-modal", () => ({
  IssuePickerModal: ({ open, onSelect }: { open: boolean; onSelect: (issue: { id: string; identifier: string }) => void }) =>
    open ? <button onClick={() => onSelect({ id: "other-1", identifier: "CHE-9" })}>pick CHE-9</button> : null,
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "Asia/Shanghai" }));

async function renderForm() {
  renderWithI18n(<WakeupCreate workspaceId="ws" issueId="issue" defaultAgentId="emacs" />);
  fireEvent.click(screen.getByRole("button", { name: "New wakeup" }));
  await screen.findByRole("form", { name: "New wakeup" });
}

async function chooseCondition(label: string) {
  fireEvent.click(screen.getByRole("button", { name: /Choose a condition/ }));
  fireEvent.click(await screen.findByRole("menuitem", { name: new RegExp(label) }));
}

async function chooseOption(trigger: RegExp, option: string | RegExp) {
  fireEvent.click(screen.getByRole("button", { name: trigger }));
  fireEvent.click(await screen.findByRole("menuitemradio", { name: option }));
}

async function pickFromPicker(trigger: RegExp, option: string) {
  fireEvent.click(screen.getByRole("button", { name: trigger }));
  fireEvent.click(await screen.findByText(option));
}

function submit(instruction = "Continue") {
  fireEvent.change(screen.getByLabelText("What to do when woken"), { target: { value: instruction } });
  fireEvent.click(screen.getByRole("button", { name: /Create/ }));
}

const sent = () => create.mock.calls[0]![0];

beforeEach(() => {
  useWakeupFormDraftStore.setState({ drafts: {} });
  create.mockReset().mockResolvedValue(undefined);
  configureShortcutPlatform("macos");
});
afterEach(() => configureShortcutPlatform(null));

describe("issue wakeup form payloads", () => {
  it("at a preset time", async () => {
    await renderForm();
    await chooseCondition("At a time");
    await chooseOption(/At a time: In 1 hour/, "In 10 minutes");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent()).toMatchObject({ agent_id: "emacs", instruction: "Continue", kind: "at", mode: "once" });
    expect(new Date(sent().at).getTime()).toBeGreaterThan(Date.now());
  });

  it("at a custom time that is not in the future is refused", async () => {
    await renderForm();
    await chooseCondition("At a time");
    await chooseOption(/At a time: In 1 hour/, "Pick a time");
    fireEvent.change(screen.getByLabelText(/^Time \(/), { target: { value: "2020-01-01T09:00" } });
    submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("Choose a future time.");
    expect(create).not.toHaveBeenCalled();
  });

  it("recurring hourly", async () => {
    await renderForm();
    await chooseCondition("Check regularly");
    await chooseOption(/Check regularly: Every day/, "Every hour");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent()).toMatchObject({ kind: "every", interval_seconds: 3600, mode: "continuous" });
    expect(sent().expires_at).toBeTruthy();
  });

  it("someone replies, narrowed to a member", async () => {
    await renderForm();
    await chooseCondition("Someone replies");
    await pickFromPicker(/Anyone/, "Jiayuan");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent()).toMatchObject({
      kind: "event",
      event_types: ["comment.created"],
      filter_actor_type: "member",
      filter_actor_id: "user-j",
    });
  });

  it("an agent's run ends, for any agent and for one", async () => {
    await renderForm();
    await chooseCondition("An agent's run ends");
    await pickFromPicker(/Any agent/, "Grok");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent()).toMatchObject({
      event_types: ["task.completed", "task.failed", "task.cancelled"],
      filter_agent_id: "grok",
    });
  });

  it("custom events keep catalog order and need at least one", async () => {
    await renderForm();
    await chooseCondition("Custom events");
    submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("Choose at least one event.");
    fireEvent.click(screen.getByRole("button", { name: /0 events/ }));
fireEvent.click(await screen.findByRole("menuitemcheckbox", { name: "When a comment is edited" }));
    fireEvent.click(await screen.findByRole("menuitemcheckbox", { name: "When an agent starts a run" }));
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    fireEvent.click(screen.getByRole("button", { name: /Create/ }));
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent().event_types).toEqual(["task.started", "comment.updated"]);
  });

  it("field: status", async () => {
    await renderForm();
    await chooseCondition("A field changes");
    await chooseOption(/Choose a status/, "in_review");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent().condition).toEqual({ type: "issue_field", field: "status", value: "in_review" });
  });

  it("field: assignee, including a squad", async () => {
    await renderForm();
    await chooseCondition("A field changes");
    await chooseOption(/Field: Status/, "Assignee");
    await pickFromPicker(/Choose an assignee/, "Dev team");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent().condition).toEqual({ type: "issue_field", field: "assignee", assignee_type: "squad", assignee_id: "squad-1" });
  });

  it("field: label", async () => {
    await renderForm();
    await chooseCondition("A field changes");
    await chooseOption(/Field: Status/, "Label");
    await chooseOption(/Choose a label/, "bug");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent().condition).toEqual({ type: "issue_field", field: "label", label_id: "label-1" });
  });

  it("field: a number property is sent as a number", async () => {
    await renderForm();
    await chooseCondition("A field changes");
    await chooseOption(/Field: Status/, "Property");
    await chooseOption(/Choose a property/, "Points");
    fireEvent.change(screen.getByLabelText("Points"), { target: { value: "5" } });
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent().condition).toEqual({ type: "issue_field", field: "property", property_id: "prop-num", value: 5 });
  });

  it("field: a select property is sent as its option id", async () => {
    await renderForm();
    await chooseCondition("A field changes");
    await chooseOption(/Field: Status/, "Property");
    await chooseOption(/Choose a property/, "Size");
    await chooseOption(/^Size:/, "Large");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent().condition).toEqual({ type: "issue_field", field: "property", property_id: "prop-sel", value: "opt-l" });
  });

  it("sub-issues finish, for one stage", async () => {
    await renderForm();
    await chooseCondition("Sub-issues finish");
    await chooseOption(/Sub-issues finish: All sub-issues/, "Stage 2");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent().condition).toEqual({ type: "children_done", stage: 2 });
  });

  it("another issue's status", async () => {
    await renderForm();
    await chooseCondition("Another issue");
    submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("Choose an issue.");
    fireEvent.click(screen.getByRole("button", { name: /Choose an issue/ }));
    fireEvent.click(await screen.findByText("pick CHE-9"));
    await chooseOption(/Another issue's status: is done/, "moves to Blocked");
    fireEvent.click(screen.getByRole("button", { name: /Create/ }));
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent().condition).toEqual({ type: "other_issue", issue_id: "other-1", state: "blocked" });
  });

  it("a timeout that ends without a run", async () => {
    await renderForm();
    await chooseCondition("Someone replies");
    await chooseOption(/Then:/, "End without a run");
    await chooseOption(/Wait up to/, "30 days");
    submit();
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(sent()).toMatchObject({ on_timeout: "end", expires_in_seconds: 30 * 86400 });
  });
});

describe("issue wakeup form keyboard", () => {
  it("opens the condition menu and chooses with the keyboard", async () => {
    const user = userEvent.setup();
    await renderForm();
    screen.getByRole("button", { name: /Choose a condition/ }).focus();
    await user.keyboard("{Enter}");
    await screen.findByRole("menuitem", { name: /At a time/ });
    await user.keyboard("{ArrowDown}{Enter}");
    await waitFor(() => expect(screen.queryByRole("menuitem", { name: /At a time/ })).toBeNull());
    expect(screen.queryByRole("button", { name: /Choose a condition/ })).toBeNull();
  });

  it("does not submit on Cmd+Enter while composing text", async () => {
    await renderForm();
    await chooseCondition("At a time");
    const input = screen.getByLabelText("What to do when woken");
    fireEvent.change(input, { target: { value: "Continue" } });
    fireEvent.keyDown(input, { key: "Enter", metaKey: true, isComposing: true });
    expect(create).not.toHaveBeenCalled();
  });

  it("shows an error as an alert and clears it on the next edit", async () => {
    await renderForm();
    fireEvent.click(screen.getByRole("button", { name: /Create/ }));
    expect(screen.getByRole("alert")).toHaveTextContent("Choose a condition.");
    fireEvent.change(screen.getByLabelText("What to do when woken"), { target: { value: "x" } });
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

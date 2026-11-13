import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import { WakeupFields } from "./wakeup-fields";
import { emptyWakeupDraft, type WakeupCondition, type WakeupDraft, type WakeupScope } from "./wakeup-draft";

vi.mock("@tanstack/react-query", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-query")>()),
  useQuery: () => ({ data: [] }),
}));
vi.mock("@multica/core/issues", () => ({ childIssuesOptions: () => ({ queryKey: ["children"] }) }));
vi.mock("@multica/core/issue-statuses/hooks", () => ({ useIssueStatuses: () => ({ activeStatuses: [] }) }));
vi.mock("../utils/status-label", () => ({ useStatusLabel: () => (key: string) => key }));
vi.mock("./wakeup-condition-names", () => ({
  useConditionNames: () => ({ status: (key: string) => key, label: () => undefined, property: () => undefined, actor: (_type: string, id: string) => id }),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
vi.mock("../../modals/issue-picker-modal", () => ({ IssuePickerModal: () => null }));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "UTC" }));

const issueScope: WakeupScope = { workspaceId: "ws", issueId: "issue" };

function Harness({ scope, initial }: { scope: WakeupScope; initial?: Partial<WakeupDraft> }) {
  const [draft, setDraft] = useState({ ...emptyWakeupDraft("emacs", "UTC"), ...initial });
  return (
    <WakeupFields
      draft={draft}
      update={(patch) => setDraft((prev) => ({ ...prev, ...patch }))}
      scope={scope}
      assigneeAgentId={null}
    />
  );
}

// What each condition shows once chosen: the control that carries its parameter.
const PARAMS: [WakeupCondition, RegExp][] = [
  ["at", /At a time: In 1 hour/],
  ["recurring", /Check regularly: Every day/],
  ["reply", /Anyone/],
  ["field", /Field: Status/],
  ["run_end", /Any agent/],
  ["children", /Sub-issues finish: All sub-issues/],
  ["pull_request", /A linked PR's CI finishes: CI finishes/],
  ["other_issue", /Choose an issue/],
  ["custom", /0 events/],
];

describe("WakeupFields", () => {
  it.each(PARAMS)("shows the parameter control for %s", (condition, control) => {
    renderWithI18n(<Harness scope={issueScope} initial={{ condition }} />);
    expect(screen.getByRole("button", { name: control })).toBeVisible();
  });

  it("asks for a condition first", () => {
    renderWithI18n(<Harness scope={issueScope} />);
    expect(screen.getByRole("button", { name: /Choose a condition/ })).toBeVisible();
  });

  it("offers the condition menu the scope allows", async () => {
    renderWithI18n(<Harness scope={{ workspaceId: "ws" }} />);
    fireEvent.click(screen.getByRole("button", { name: /Choose a condition/ }));
    expect(await screen.findByRole("menuitem", { name: /Sub-issues finish/ })).toBeVisible();
    expect(screen.queryByRole("menuitem", { name: /Another issue/ })).toBeNull();
  });

  it("shows the end date for a recurring check and the wait options for an event", () => {
    const { unmount } = renderWithI18n(<Harness scope={issueScope} initial={{ condition: "recurring" }} />);
    expect(screen.getByLabelText("Until")).toBeVisible();
    expect(screen.queryByRole("radiogroup")).toBeNull();
    unmount();
    renderWithI18n(<Harness scope={issueScope} initial={{ condition: "reply" }} />);
    expect(screen.getByRole("radiogroup", { name: "Triggers" })).toBeVisible();
    expect(screen.queryByLabelText("Until")).toBeNull();
  });

  it("leaves out the timeout wake for a workspace definition", () => {
    const { unmount } = renderWithI18n(<Harness scope={issueScope} initial={{ condition: "reply" }} />);
    expect(screen.getByRole("button", { name: /Wake the agent to handle it|Wake .* to handle it/ })).toBeVisible();
    unmount();
    renderWithI18n(<Harness scope={{ workspaceId: "ws", definition: true }} initial={{ condition: "reply" }} />);
    expect(screen.getByRole("radiogroup", { name: "Triggers" })).toBeVisible();
    expect(screen.queryByRole("button", { name: /to handle it/ })).toBeNull();
  });

  it("explains that a reply wait for the assignee joins the run a comment starts", () => {
    renderWithI18n(
      <WakeupFields
        draft={{ ...emptyWakeupDraft("emacs", "UTC"), condition: "reply" }}
        update={() => {}}
        scope={issueScope}
        assigneeAgentId="emacs"
      />,
    );
    expect(screen.getByText(/is the assignee and already runs when a member comments/)).toBeVisible();
  });
});

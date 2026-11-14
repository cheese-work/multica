import { fireEvent, render, screen, cleanup } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import { useIssueViewStore } from "@multica/core/issues/stores/view-store";
import type { Issue } from "@multica/core/types";
import { Circle } from "@multica/ui/icons";
import enIssues from "../../../locales/en/issues.json";
import { IssueHierarchy } from "./hierarchy-minimap";
import { ISSUE_MODES, type IssueMode } from "./issue-modes";

// CHE-4358 (D5, premise 5): the sidebar card and the dialog share one N-mode switch, driven by a registry.
vi.mock("@multica/core/paths", () => ({ useWorkspacePaths: () => ({ issueDetail: (id: string) => `/issues/${id}` }) }));
vi.mock("../../../navigation", () => ({
  useNavigation: () => ({ push: vi.fn() }),
  AppLink: ({ href, children, ...props }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) => <a href={href} {...props}>{children}</a>,
}));
vi.mock("./use-hierarchy", () => ({
  useHierarchy: () => ({
    hierarchy: null, loading: true, error: false, catalog: { categoryOf: () => "unstarted", labelOf: (s: string) => s },
    wakeCount: null, mentionCount: null, mentionsLoading: false, refetch: vi.fn(),
  }),
}));
vi.mock("../swimlane2/swimlane2-issue-tree", () => ({ Swimlane2IssueTree: () => <div>swimlane tree stub</div> }));

const fake: IssueMode = {
  id: "fake3",
  label: () => "Fake three",
  icon: Circle,
  Card: () => <div>fake card</div>,
  Dialog: () => <div>fake dialog body</div>,
};
const modes = [...ISSUE_MODES, fake];
const issue = { id: "id-2", identifier: "CHE-2", title: "x", status: "todo", parent_issue_id: null } as Issue;
const ui = () => <I18nProvider locale="en" resources={{ en: { issues: enIssues } }}><IssueHierarchy issue={issue} modes={modes} /></I18nProvider>;
const switchButton = (name: string) => screen.getByRole("button", { name });

beforeEach(() => useIssueViewStore.setState({ sidebarMode: "hierarchy" }));
afterEach(cleanup);

describe("sidebar mode switch", () => {
  it("lists every registry entry and starts on hierarchy", () => {
    render(ui());
    expect(switchButton("Hierarchy")).toHaveAttribute("aria-pressed", "true");
    expect(switchButton("Swimlane 2.0")).toHaveAttribute("aria-pressed", "false");
    expect(switchButton("Fake three")).toHaveAttribute("aria-pressed", "false");
  });

  it("a fake third mode shows in the switch, renders, and persists across navigation", () => {
    const first = render(ui());
    fireEvent.click(switchButton("Fake three"));
    expect(screen.getByText("fake card")).toBeInTheDocument();
    expect(useIssueViewStore.getState().sidebarMode).toBe("fake3");
    first.unmount(); // navigate away, come back on another issue page
    render(ui());
    expect(screen.getByText("fake card")).toBeInTheDocument();
    expect(switchButton("Fake three")).toHaveAttribute("aria-pressed", "true");
  });

  it("the dialog opens in the persisted mode and its switch changes the same mode", () => {
    useIssueViewStore.setState({ sidebarMode: "fake3" });
    render(ui());
    fireEvent.click(screen.getByRole("button", { name: "Expand hierarchy" }));
    expect(screen.getByText("fake dialog body")).toBeInTheDocument();
    const dialog = screen.getByTestId("hierarchy-dialog");
    fireEvent.click(dialog.querySelector('[aria-label="View"] button[aria-pressed="false"]')!);
    expect(useIssueViewStore.getState().sidebarMode).not.toBe("fake3");
    expect(screen.queryByText("fake dialog body")).toBeNull();
  });

  it("an unknown or removed id falls back to hierarchy", () => {
    useIssueViewStore.setState({ sidebarMode: "removed-mode" });
    render(ui());
    expect(switchButton("Hierarchy")).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByText("fake card")).toBeNull();
  });

  it("swimlane 2.0 mode renders the issue tree view", () => {
    render(ui());
    fireEvent.click(switchButton("Swimlane 2.0"));
    expect(screen.getByText("swimlane tree stub")).toBeInTheDocument();
  });
});

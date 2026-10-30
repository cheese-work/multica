import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import enIssues from "../../locales/en/issues.json";
import { IssueRelationsButton } from "./issue-relations-button";

const prefetch = vi.hoisted(() => vi.fn());
vi.mock("@tanstack/react-query", async (original) => ({ ...await original<typeof import("@tanstack/react-query")>(), useQueryClient: () => ({ prefetchQuery: prefetch }) }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace" }));
vi.mock("./issue-relations-dialog", () => ({ default: ({ issueId, onClose }: { issueId: string; onClose: () => void }) => <div role="dialog" aria-label={issueId}><button onClick={onClose}>Close</button></div> }));

function ui(issueId: string) {
  return <I18nProvider locale="en" resources={{ en: { issues: enIssues } }}><IssueRelationsButton issueId={issueId} /></I18nProvider>;
}
describe("issue relations button", () => {
  it("prefetches structural relations on hover and keyboard focus, without opening", () => {
    prefetch.mockClear();
    render(ui("root"));
    const button = screen.getByRole("button", { name: "Relations graph" });
    expect(prefetch).not.toHaveBeenCalled();
    fireEvent.mouseEnter(button);
    fireEvent.focus(button);
    expect(prefetch).toHaveBeenCalledTimes(2);
    expect(prefetch.mock.calls[0]![0]).toMatchObject({ queryKey: ["issues", "workspace", "relations", "root", 1, false], staleTime: 30_000 });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
  it("mounts the dialog shell synchronously and clears on issue switch without a key", () => {
    const view = render(ui("root"));
    fireEvent.click(screen.getByRole("button", { name: "Relations graph" }));
    expect(screen.getByRole("dialog", { name: "root" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Relations graph" }));
    view.rerender(ui("next"));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    view.rerender(ui("root"));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});

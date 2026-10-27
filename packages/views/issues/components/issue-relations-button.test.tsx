import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import enIssues from "../../locales/en/issues.json";
import { IssueRelationsButton } from "./issue-relations-button";

const lazyLoad = vi.hoisted(() => vi.fn());
vi.mock("./issue-relations-dialog", () => {
  lazyLoad();
  return { default: ({ issueId, onClose }: { issueId: string; onClose: () => void }) => <div role="dialog" aria-label={issueId}><button onClick={onClose}>Close</button></div> };
});

describe("issue relations button", () => {
  it("does not load or mount graph code until clicked, and clears on issue switch", async () => {
    const ui = (issueId: string) => <I18nProvider locale="en" resources={{ en: { issues: enIssues } }}><IssueRelationsButton key={issueId} issueId={issueId} /></I18nProvider>;
    const view = render(ui("root"));
    expect(lazyLoad).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Relations graph" }));
    expect(await screen.findByRole("dialog", { name: "root" })).toBeInTheDocument();
    expect(lazyLoad).toHaveBeenCalledOnce();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Relations graph" }));
    expect(await screen.findByRole("dialog", { name: "root" })).toBeInTheDocument();
    view.rerender(ui("next"));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});

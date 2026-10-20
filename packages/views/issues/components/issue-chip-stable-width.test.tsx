import { cleanup, render, screen } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createQueryClient } from "@multica/core/query-client";
import { issueKeys } from "@multica/core/issues/queries";
import type { Issue, ListIssuesCache } from "@multica/core/types";
import { IssueChip } from "./issue-chip";
import { IssueChipLoading } from "./issue-chip-loading";
import { ISSUE_CHIP_BASE_CLASS, ISSUE_CHIP_STABLE_WIDTH_CLASS } from "./issue-chip-shell";

// CHE-1829, approved design B (design/bare-key-chip-stability, PR #294): a bare
// key's loading box and resolved chip share one fixed width of 14 rem (224 px).
// These are style-policy assertions over the rendered classes in jsdom. They are
// not geometry, baseline or mobile-wrap evidence.

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { getIssue: vi.fn() },
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));

vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => ({ colorOf: () => null, iconOf: () => null }),
}));

vi.mock("./status-icon", () => ({
  StatusIcon: () => <svg data-testid="status-icon" />,
}));

const issue = {
  id: "11111111-1111-4111-8111-111111111111",
  workspace_id: "workspace-1",
  number: 7,
  identifier: "MUL-7",
  title: "A short title",
  status: "todo",
  priority: "none",
  metadata: {},
  properties: {},
} as unknown as Issue;

const SELECTED_CAP_REM = 14;
const classesOf = (el: Element | null | undefined) => (el?.getAttribute("class") ?? "").split(/\s+/);
const remOf = (cls: string) => Number(/min\((\d+(?:\.\d+)?)rem,100%\)/.exec(cls)?.[1]);

describe("bare-key chip width policy (design B)", () => {
  let queryClient: ReturnType<typeof createQueryClient>;

  beforeEach(() => {
    queryClient = createQueryClient();
    queryClient.setQueryData<ListIssuesCache>(issueKeys.listSorted("workspace-1"), {
      byStatus: { unstarted: { issues: [issue], total: 1 } },
    });
  });

  afterEach(() => {
    cleanup();
    queryClient.clear();
  });

  function chip(stableWidth: boolean) {
    return (
      <QueryClientProvider client={queryClient}>
        <IssueChip issueId={issue.id} fallbackLabel="MUL-7" stableWidth={stableWidth} />
      </QueryClientProvider>
    );
  }

  it("pins the shared width to the selected 14 rem cap", () => {
    expect(remOf(ISSUE_CHIP_STABLE_WIDTH_CLASS)).toBe(SELECTED_CAP_REM);
  });

  it("gives the loading box and the resolved bare-key chip that same width", () => {
    const loading = render(<IssueChipLoading identifier="MUL-7" />);
    const loadingBox = loading.container.querySelector("[data-issue-chip-loading]");
    expect(classesOf(loadingBox)).toContain(ISSUE_CHIP_STABLE_WIDTH_CLASS);
    loading.unmount();

    render(chip(true));
    const resolved = screen.getByText("A short title").closest(".issue-mention");
    expect(classesOf(resolved)).toContain(ISSUE_CHIP_STABLE_WIDTH_CLASS);
  });

  it("keeps the narrow-container bound: the fixed width never exceeds the content cap", () => {
    expect(ISSUE_CHIP_STABLE_WIDTH_CLASS).toContain(",100%)");
    expect(remOf(ISSUE_CHIP_STABLE_WIDTH_CLASS)).toBeLessThanOrEqual(remOf(ISSUE_CHIP_BASE_CLASS));
  });

  it("leaves an authored URL or UUID chip content-sized", () => {
    render(chip(false));
    const resolved = screen.getByText("A short title").closest(".issue-mention");
    expect(classesOf(resolved)).not.toContain(ISSUE_CHIP_STABLE_WIDTH_CLASS);
    expect(classesOf(resolved)).toEqual(expect.arrayContaining(ISSUE_CHIP_BASE_CLASS.split(" ")));
  });
});

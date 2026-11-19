import type { ReactNode } from "react";
import { screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import { ApiError, type ApiClient } from "@multica/core/api/client";
import { createQueryClient } from "@multica/core/query-client";
import { buildIssueStatusCatalog } from "@multica/core/issue-statuses/queries";
import type { PinnedItem } from "@multica/core/types";
import { useSidebarTreeStore } from "@multica/core/issues/stores/sidebar-tree-store";
import { renderWithI18n } from "../../test/i18n";
import { IssueContextMenuProvider } from "../../issues/actions";
import { PinnedSection, usePanelPins } from "./pinned-section";

vi.mock("../../issues/actions/issue-actions-menu-items", () => ({
  contextPrimitives: {},
  IssueActionsMenuItems: ({ issue, variant }: { issue: { title: string }; variant?: string }) => (
    <div>{`${variant} menu: ${issue.title}`}</div>
  ),
}));
vi.mock("../../issues/actions/use-issue-actions", () => ({ useIssueActions: () => ({}) }));

const state = vi.hoisted(() => ({
  pins: [] as PinnedItem[],
  deletePin: vi.fn(),
}));

vi.mock("@multica/core/issue-statuses/hooks", () => ({
  useIssueStatuses: () => buildIssueStatusCatalog([]),
}));
vi.mock("../../navigation", () => ({
  AppLink: ({ children, href, ...props }: { children: ReactNode; href: string } & React.ComponentProps<"a">) => (
    <a href={href} {...props}>
      {children}
    </a>
  ),
}));
vi.mock("../../issues/components/status-icon", () => ({ StatusIcon: () => <span /> }));
vi.mock("@multica/ui/components/common/tooltip-hint", () => ({
  TooltipHint: ({ children }: { children: ReactNode }) => <>{children}</>,
}));
vi.mock("@multica/core/paths", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/paths")>();
  return { ...actual, useWorkspacePaths: () => actual.paths.workspace("acme") };
});
vi.mock("@multica/core/pins/queries", () => ({ pinListOptions: () => ({ queryKey: ["pins"], queryFn: async () => state.pins }) }));
vi.mock("@multica/core/pins/mutations", () => ({
  useDeletePin: () => ({ mutate: state.deletePin }),
  useReorderPins: () => ({ mutate: vi.fn() }),
}));
vi.mock("@multica/core/projects/queries", () => ({ projectDetailOptions: () => ({ queryKey: ["project"] }) }));
vi.mock("@multica/core/issue-views/queries", () => ({
  issueViewDetailOptions: () => ({ queryKey: ["view"] }),
}));

const PIN: PinnedItem = {
  id: "pin-1",
  workspace_id: "ws-1",
  user_id: "user-1",
  item_type: "issue",
  item_id: "issue-1",
  position: 1,
  created_at: "2026-05-06T00:00:00Z",
};

function Harness() {
  const pins = usePanelPins("ws-1", "user-1", "/acme/issues", null);
  return (
    <IssueContextMenuProvider variant="sidebar">
      <PinnedSection pins={pins} wsId="ws-1" />
    </IssueContextMenuProvider>
  );
}

// Real query client and real issueDetailOptions: the retry policy is the thing
// under test, so nothing between PinRow and the cache may be mocked.
describe("PinnedSection detail read", () => {
  let qc: QueryClient;
  beforeEach(() => {
    state.pins = [PIN];
    state.deletePin.mockReset();
    useSidebarTreeStore.setState({ pinnedCollapsed: false });
    qc = createQueryClient();
  });

  it("recovers a pinned issue after one transient 503", async () => {
    const getIssue = vi
      .fn()
      .mockRejectedValueOnce(new ApiError("unavailable", 503, "Service Unavailable"))
      .mockResolvedValue({ id: "issue-1", identifier: "MUL-1", title: "Recovered pin", status: "todo" });
    setApiInstance({ getIssue } as unknown as ApiClient);
    renderWithI18n(
      <QueryClientProvider client={qc}>
        <Harness />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("Recovered pin", {}, { timeout: 4000 })).toBeInTheDocument();
    expect(getIssue).toHaveBeenCalledTimes(2);
    expect(state.deletePin).not.toHaveBeenCalled();
  });

  it("control: renders a pinned issue after a clean read", async () => {
    const getIssue = vi
      .fn()
      .mockResolvedValue({ id: "issue-1", identifier: "MUL-1", title: "Clean pin", status: "todo" });
    setApiInstance({ getIssue } as unknown as ApiClient);
    renderWithI18n(
      <QueryClientProvider client={qc}>
        <Harness />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("Clean pin")).toBeInTheDocument();
    expect(getIssue).toHaveBeenCalledTimes(1);
  });
});

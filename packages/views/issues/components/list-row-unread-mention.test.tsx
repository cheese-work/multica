import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Issue } from "@multica/core/types";
import { NavigationProvider, type NavigationAdapter } from "../../navigation";
import { ListRow } from "./list-row";

const hasUnread = vi.hoisted(() => vi.fn<(wsId: string, issueId: string) => boolean>());

vi.mock("@multica/core/inbox", () => ({
  useHasUnreadMention: hasUnread,
}));

vi.mock("@tanstack/react-query", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-query")>()),
  useQuery: () => ({ data: [] }),
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/properties", () => ({ propertyListOptions: () => ({ queryKey: ["properties"] }) }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}` }),
}));
vi.mock("@multica/core/issues/stores/view-store-context", () => ({
  useViewStore: (selector: (state: unknown) => unknown) =>
    selector({ cardProperties: {}, cardPropertyIds: [] }),
}));
vi.mock("../../i18n", () => ({
  useLocale: () => "en",
  useT: () => ({ t: () => "Unread mention" }),
}));
vi.mock("../actions", () => ({
  IssueActionsContextMenu: ({ children }: { children: ReactNode }) => children,
}));
vi.mock("./issue-agent-activity-indicator", () => ({ IssueAgentActivityIndicator: () => null }));
vi.mock("./custom-status-chip", () => ({ CustomStatusChip: () => null }));

const navigation: NavigationAdapter = {
  push: vi.fn(),
  replace: vi.fn(),
  back: vi.fn(),
  pathname: "/acme/issues",
  searchParams: new URLSearchParams(),
  hash: "",
  getShareableUrl: (path) => `https://app.example${path}`,
};

const issue = {
  id: "issue-1",
  identifier: "MUL-1",
  title: "Mentioned row",
  status: "todo",
  priority: "none",
  labels: [],
  properties: {},
} as unknown as Issue;

function renderRow() {
  return render(
    <NavigationProvider value={navigation}>
      <ListRow issue={issue} />
    </NavigationProvider>,
  );
}

describe("ListRow unread-mention badge", () => {
  beforeEach(() => hasUnread.mockReset());

  it("shows a labelled badge when the issue has an unread mention", () => {
    hasUnread.mockImplementation((wsId, issueId) => wsId === "ws-1" && issueId === "issue-1");
    renderRow();
    expect(screen.getByLabelText("Unread mention")).toBeTruthy();
  });

  it("shows no badge otherwise", () => {
    hasUnread.mockReturnValue(false);
    renderRow();
    expect(screen.queryByLabelText("Unread mention")).toBeNull();
  });
});

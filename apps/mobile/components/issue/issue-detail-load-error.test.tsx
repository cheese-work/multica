// @vitest-environment jsdom
// Renders the real issue detail screen with every collaborator stubbed, to check
// which message a failed load shows. Wiring only, not native rendering.
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const state = vi.hoisted(() => ({
  params: { id: "CHE-3680", workspace: "cheese" } as Record<string, string>,
  detail: {} as Record<string, unknown>,
  refetch: vi.fn(),
}));

vi.mock("react-native", async () => ({
  ...(await import("../../data/stores/test-support/react-native-stub")),
  ActionSheetIOS: { showActionSheetWithOptions: vi.fn() },
  ActivityIndicator: () => <div>loading</div>,
  Linking: { openURL: vi.fn() },
}));
vi.mock("expo-router", async () => {
  const mocks = (await import("../../data/stores/test-support/form-screen-mocks")).expoRouter();
  return { ...mocks, useLocalSearchParams: () => state.params };
});
vi.mock("expo-clipboard", () => ({ setStringAsync: vi.fn() }));
vi.mock("@/components/ui/text", async () => (await import("../../data/stores/test-support/form-screen-mocks")).text());
vi.mock("@/components/ui/button", () => ({
  Button: ({ children, onPress }: { children?: React.ReactNode; onPress?: () => void }) => (
    <button type="button" onClick={onPress}>{children}</button>
  ),
}));
vi.mock("@/components/ui/icon-button", () => ({ IconButton: () => null }));
vi.mock("@/components/issue/timeline-list", () => ({ TimelineList: () => <div>timeline</div> }));
vi.mock("@/components/issue/agent-header-badge", () => ({ AgentHeaderBadge: () => null }));
vi.mock("@/components/issue/inline-comment-composer", () => ({ InlineCommentComposer: () => null }));
vi.mock("@/lib/i18n", async () => (await import("../../data/stores/test-support/form-screen-mocks")).i18n());
vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey?: string[] }) =>
    opts?.queryKey?.[0] === "detail" ? state.detail : { data: undefined, isLoading: false },
  useQueryClient: () => ({ invalidateQueries: vi.fn() }),
}));
vi.mock("@/data/queries/issues", () => ({
  issueDetailOptions: () => ({ queryKey: ["detail"] }),
  issueTimelineOptions: () => ({ queryKey: ["timeline"] }),
  issueKeys: { timeline: () => ["timeline"] },
}));
vi.mock("@/data/queries/inbox", () => ({ inboxListOptions: () => ({ queryKey: ["inbox"] }) }));
vi.mock("@/data/mutations/inbox", () => ({ useMarkInboxRead: () => ({ mutate: vi.fn() }) }));
vi.mock("@/data/queries/pins", () => ({ pinListOptions: () => ({ queryKey: ["pins"] }) }));
vi.mock("@/data/mutations/issues", () => ({ useDeleteIssue: () => ({ mutate: vi.fn() }) }));
vi.mock("@/data/api", () => ({
  ApiError: class ApiError extends Error {
    constructor(message: string, readonly status = 0) {
      super(message);
    }
  },
}));
vi.mock("@/data/mutations/pins", () => ({
  useCreatePin: () => ({ mutate: vi.fn() }),
  useDeletePin: () => ({ mutate: vi.fn() }),
}));
vi.mock("@/data/auth-store", () => ({
  useAuthStore: (sel: (s: { user: null }) => unknown) => sel({ user: null }),
}));
vi.mock("@/data/realtime/use-issue-realtime", () => ({ useIssueRealtime: () => {} }));
vi.mock("@/data/workspace-store", () => ({
  useWorkspaceStore: (sel: (s: { currentWorkspaceId: string }) => unknown) =>
    sel({ currentWorkspaceId: "ws1" }),
}));
vi.mock("@/data/viewed-issues-store", () => ({ useViewedIssuesStore: { getState: () => ({ push: vi.fn() }) } }));
vi.mock("@/data/comment-select-store", () => ({ useCommentSelectStore: { getState: () => ({ clear: vi.fn() }) } }));

import { SchemaValidationError } from "@/lib/parse-response";
import IssueDetail from "@/app/(app)/[workspace]/issue/[id]";

const failed = (error: unknown) => {
  state.detail = { data: undefined, isLoading: false, error, refetch: state.refetch };
};

describe("issue detail screen: failed load", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    state.params = { id: "CHE-3680", workspace: "cheese" };
  });
  afterEach(cleanup);

  it("says the data is unreadable, with the issue key and Retry, when a 200 fails schema validation", () => {
    failed(new SchemaValidationError("/api/issues/CHE-3680", ["metadata.decision_park"]));
    render(<IssueDetail />);
    expect(screen.getByText("errors.data_error")).toBeTruthy();
    expect(screen.getByText("CHE-3680")).toBeTruthy();
    expect(screen.queryByText("errors.load_failed")).toBeNull();
    expect(screen.queryByText("errors.not_found")).toBeNull();
    fireEvent.click(screen.getByText("common:actions.retry"));
    expect(state.refetch).toHaveBeenCalledTimes(1);
  });

  it("does not show a UUID as the issue key", () => {
    state.params = { id: "5b0f3c2e-1d4a-4c7e-9a1b-2f3e4d5c6b7a", workspace: "cheese" };
    failed(new SchemaValidationError("/api/issues/x", ["metadata.decision_park"]));
    render(<IssueDetail />);
    expect(screen.getByText("errors.data_error")).toBeTruthy();
    expect(screen.queryByText(state.params.id)).toBeNull();
  });

  it("keeps 'not found' for a 404", () => {
    failed(Object.assign(new Error("not found"), { status: 404 }));
    render(<IssueDetail />);
    expect(screen.getByText("errors.not_found")).toBeTruthy();
    expect(screen.queryByText("errors.data_error")).toBeNull();
  });

  it("keeps the connection message for a network error", () => {
    failed(new Error("Network request failed"));
    render(<IssueDetail />);
    expect(screen.getByText("errors.load_failed")).toBeTruthy();
    expect(screen.queryByText("errors.data_error")).toBeNull();
  });
});

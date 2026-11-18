import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, renderHook, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { chatKeys, issueDecisionCardsOptions } from "@multica/core/chat/queries";
import type { WSClient } from "@multica/core/api/ws-client";
import { useRealtimeSync, type RealtimeSyncStores } from "@multica/core/realtime";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { setCurrentWorkspace } from "@multica/core/platform";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { DecisionSummary } from "./decision-summary";

const api = vi.hoisted(() => ({ listIssueDecisionCards: vi.fn(), listMessages: vi.fn() }));
vi.mock("@multica/core/api", async (importActual) => ({ ...(await importActual<typeof import("@multica/core/api")>()), api }));
vi.mock("@multica/core/paths", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/paths")>()),
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

const card = (outcome: string): RequestCard => ({
  id: "r1", issue_id: "issue-1", issue_identifier: "CHE-1", ask: "Top up?", state: "answered", created_at: "2026-11-11T06:20:00Z",
  action: { id: "a1", action_type: "issue.run_budget.top_up", payload_sha256: "p", state: "executed", effect: "applied", decided_at: "2026-11-11T06:44:00Z", receipt: { outcome, applied_at: "2026-11-11T06:44:00Z" } },
} as unknown as RequestCard);

// Both surfaces hold their card in a query (issue page: card list; DM: message page). The helper's
// invalidation must reach each of them without a reload.
function IssuePageCards() {
  const { data } = useQuery(issueDecisionCardsOptions("ws-1", "issue-1"));
  return <>{data?.map((c) => <div key={c.id} data-testid="issue-card"><DecisionSummary decision={c} /></div>)}</>;
}
function DmCards() {
  const { data } = useQuery({ queryKey: chatKeys.messages("dm-1"), queryFn: () => api.listMessages(), staleTime: Infinity });
  return <>{data?.map((m: { id: string; decision_request: RequestCard }) => <div key={m.id} data-testid="dm-card"><DecisionSummary decision={m.decision_request} /></div>)}</>;
}

describe("decided cards update live from realtime events", () => {
  const handlers: Record<string, (payload: unknown) => void> = {};
  let qc: QueryClient;
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  const serve = (outcome: string) => {
    api.listIssueDecisionCards.mockResolvedValue([card(outcome)]);
    api.listMessages.mockResolvedValue([{ id: "m1", decision_request: card(outcome) }]);
  };

  beforeEach(() => {
    setCurrentWorkspace("acme", "ws-1");
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    serve("still_blocked:runs");
    const ws = {
      on: (event: string, handler: (payload: unknown) => void) => { handlers[event] = handler; return () => {}; },
      onAny: () => () => {},
      onReconnect: () => () => {},
    } as unknown as WSClient;
    renderHook(() => useRealtimeSync(ws, { authStore: { getState: () => ({ user: { id: "u1" } }) } } as unknown as RealtimeSyncStores), { wrapper });
    renderWithI18n(<QueryClientProvider client={qc}><WorkspaceSlugProvider slug="acme"><IssuePageCards /><DmCards /></WorkspaceSlugProvider></QueryClientProvider>);
  });
  afterEach(cleanup);

  const both = async (text: string) => {
    await waitFor(() => {
      expect(screen.getByTestId("issue-card")).toHaveTextContent(text);
      expect(screen.getByTestId("dm-card")).toHaveTextContent(text);
    });
  };

  it("issue:updated with status_changed: true moves both cards to superseded:status_moved", async () => {
    await both("Still blocked: no runs left.");
    serve("superseded:status_moved");
    await act(async () => { handlers["issue:updated"]!({ issue: { id: "issue-1", status: "todo" }, status_changed: true }); await new Promise((r) => setTimeout(r, 400)); });
    await both("Not resumed: the issue moved to another status.");
  });

  it("decision_answer:updated moves both cards to superseded:later_window", async () => {
    await both("Still blocked: no runs left.");
    serve("superseded:later_window");
    await act(async () => { handlers["decision_answer:updated"]!({ answer_id: "x", issue_id: "issue-1", state: "recorded" }); });
    await both("Not resumed: a newer budget window replaced this one.");
  });

  it("an issue:updated that is not a status change leaves the cards alone", async () => {
    await both("Still blocked: no runs left.");
    serve("superseded:status_moved");
    await act(async () => { handlers["issue:updated"]!({ issue: { id: "issue-1", status: "todo" }, status_changed: false }); await new Promise((r) => setTimeout(r, 400)); });
    await both("Still blocked: no runs left.");
  });
});

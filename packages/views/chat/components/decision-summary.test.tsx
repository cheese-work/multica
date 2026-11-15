import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionRequestCard } from "./decision-request-card";
import { DecisionSummary } from "./decision-summary";

// B v9 S9 (CHE-5419): one collapsed block for a decided card, shared by the DM and the issue page;
// Revoke while revocable; the collapse animation honours prefers-reduced-motion.

vi.mock("../../platform", () => ({ openExternal: vi.fn() }));
vi.mock("../../platform/local-directory", () => ({ isDesktopShell: () => false }));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  useOptionalNavigation: () => null,
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));
vi.mock("../../issues/components/issue-chip", () => ({
  IssueChip: ({ fallbackLabel, children }: { fallbackLabel?: string; children?: React.ReactNode }) => <span>{children ?? fallbackLabel}</span>,
}));
vi.mock("../../issues/components/comment-mention-card", () => ({
  CommentMentionCard: ({ fallback }: { fallback: React.ReactNode }) => <>{fallback}</>,
}));
const signPasskeyChallenge = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/auth/webauthn", () => ({ signPasskeyChallenge }));
const api = vi.hoisted(() => ({
  listDecisionArtifacts: vi.fn(),
  getDecisionActionOutcome: vi.fn(),
  challengeDecisionRevoke: vi.fn(),
  revokeDecision: vi.fn(),
}));
vi.mock("@multica/core/api", async (importActual) => ({ ...(await importActual<typeof import("@multica/core/api")>()), api }));
vi.mock("@multica/core/permissions", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/permissions")>()),
  useCurrentMember: () => ({ userId: "u1", role: "owner", member: null, isLoading: false }),
}));
vi.mock("@multica/core/paths", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => ({ id: "ws-1" }),
}));

const base = { id: "action", action_type: "issue.run_budget.top_up", payload_sha256: "payload" };
const applied = {
  ...base, state: "executed", effect: "applied", decided_at: "2026-11-11T06:44:00Z",
  decided_by_name: "Tofu", on_behalf_of_name: "Cheese", revocable: true,
};
const card = (action: Record<string, unknown> | undefined, extra: Partial<RequestCard> = {}): RequestCard => ({
  id: "request", issue_id: "issue", issue_identifier: "CHE-3459", issue_title: "Sync", asking_agent_name: "x99-claude-gstack",
  ask: "Reopen CHE-3459 so the v0.8.0 sync can finish?",
  state: "answered", created_at: "2026-11-11T06:20:00Z", record_comment_id: "record",
  answer: { state: "picked_up", answered_at: "2026-11-11T06:44:00Z" },
  action: action as never, ...extra,
});

const wrap = (node: React.ReactNode) => (
  <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme">{node}</WorkspaceSlugProvider></QueryClientProvider>
);
const summary = (decision: RequestCard, onRevoke?: (reason: string) => Promise<void>) =>
  renderWithI18n(wrap(<DecisionSummary decision={decision} onRevoke={onRevoke} />));

const reduceMotion = (reduce: boolean) =>
  vi.spyOn(window, "matchMedia").mockImplementation((query: string) => ({
    matches: reduce && query.includes("prefers-reduced-motion"), media: query, onchange: null,
    addListener: () => {}, removeListener: () => {}, addEventListener: () => {}, removeEventListener: () => {}, dispatchEvent: () => false,
  }) as MediaQueryList);

describe("DecisionSummary: one block per decided card", () => {
  beforeEach(() => {
    api.listDecisionArtifacts.mockResolvedValue([]);
    api.getDecisionActionOutcome.mockResolvedValue({ id: "action", state: "executed" });
  });
  afterEach(() => vi.restoreAllMocks());

  it.each([
    ["applied by a delegate", applied, ["Applied.", "Tofu, for Cheese", "11 Nov 2026, 13:20 ICT", "11 Nov 2026, 13:44 ICT", "x99-claude-gstack"]],
    ["not applied, with the reason", { ...base, state: "failed", effect: "not_applied", effect_reason: "target_changed", decided_at: "2026-11-11T06:44:00Z", decided_by_name: "Cheese", on_behalf_of_name: "Cheese" },
      ["Not applied", "target_changed", "By", "Cheese"]],
    ["rejected", { ...base, state: "rejected", effect: "not_applied", effect_reason: "rejected", decided_at: "2026-11-11T06:44:00Z", decided_by_name: "Cheese", on_behalf_of_name: "Cheese" },
      ["Not applied", "Nothing ran."]],
    ["expired", { ...base, state: "expired", effect: "expired", decided_at: "2026-11-12T06:20:00Z" },
      ["Expired", "12 Nov 2026, 13:20 ICT"]],
    ["revoked", { ...applied, revocable: false, revoke: { at: "2026-11-12T02:21:00Z", by_name: "Cheese", reason: "wrong run" } },
      ["Revoked", "12 Nov 2026, 09:21 ICT", "wrong run"]],
  ])("%s", (_name, action, texts) => {
    summary(card(action));
    const block = screen.getByTestId("decision-summary");
    for (const text of texts) expect(block).toHaveTextContent(text);
  });

  it("shows the asked and decided times in ICT and who asked", () => {
    summary(card(applied));
    const meta = screen.getByTestId("decision-request-meta");
    expect(meta).toHaveTextContent("Asked");
    expect(meta).toHaveTextContent("Decided");
    expect(meta).toHaveTextContent("From");
  });
});

describe("Revoke shows only while revocable", () => {
  beforeEach(() => api.listDecisionArtifacts.mockResolvedValue([]));

  it("shows Revoke on an applied, revocable decision", () => {
    summary(card(applied), vi.fn());
    expect(screen.getByRole("button", { name: "Revoke" })).toBeInTheDocument();
  });

  it.each([
    ["rejected", { ...base, state: "rejected", effect: "not_applied", effect_reason: "rejected", revocable: true }],
    ["expired", { ...base, state: "expired", effect: "expired", revocable: true }],
    ["not applied", { ...base, state: "failed", effect: "not_applied", effect_reason: "x", revocable: true }],
    ["answered (no action)", undefined],
    ["revoked", { ...applied, revoke: { at: "2026-11-12T02:21:00Z", reason: "r" } }],
    ["applied but no longer revocable", { ...applied, revocable: false }],
  ])("never on a %s decision, even if the flag says true", (_name, action) => {
    summary(card(action as never), vi.fn());
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
  });

  it("is read-only without a revoke handler (the issue view)", () => {
    summary(card(applied));
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
  });

  it("asks for a reason, then calls the handler with it", async () => {
    const onRevoke = vi.fn().mockResolvedValue(undefined);
    const user = userEvent.setup();
    summary(card(applied), onRevoke);
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    const confirm = screen.getByRole("button", { name: "Confirm revoke" });
    expect(confirm).toBeDisabled();
    await user.type(screen.getByRole("textbox", { name: "Reason" }), "  wrong run ");
    await user.click(confirm);
    expect(onRevoke).toHaveBeenCalledWith("wrong run");
  });

  it("keeps the reason and says so when the revoke is refused", async () => {
    const onRevoke = vi.fn().mockRejectedValue(new Error("no"));
    const user = userEvent.setup();
    summary(card(applied), onRevoke);
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    await user.type(screen.getByRole("textbox", { name: "Reason" }), "why");
    await user.click(screen.getByRole("button", { name: "Confirm revoke" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Revoke failed");
    expect(screen.getByRole("textbox", { name: "Reason" })).toHaveValue("why");
  });
});

describe("DM and issue views share one block", () => {
  beforeEach(() => {
    api.listDecisionArtifacts.mockResolvedValue([]);
    api.getDecisionActionOutcome.mockResolvedValue({ id: "action", state: "executed" });
    api.challengeDecisionRevoke.mockReset().mockResolvedValue({ challenge_id: "ch" });
    api.revokeDecision.mockReset().mockResolvedValue({ outcome: "revoked" });
    signPasskeyChallenge.mockReset().mockResolvedValue({ id: "cred" });
  });

  it("DM: Revoke runs the passkey ceremony and posts the reason", async () => {
    const user = userEvent.setup();
    renderWithI18n(wrap(<DecisionRequestCard card={card(applied)} />));
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    await user.type(screen.getByRole("textbox", { name: "Reason" }), "wrong run");
    await user.click(screen.getByRole("button", { name: "Confirm revoke" }));
    await waitFor(() => expect(api.revokeDecision).toHaveBeenCalledWith("request", { reason: "wrong run", assertion: { id: "cred" } }));
    expect(api.challengeDecisionRevoke).toHaveBeenCalledWith("request");
  });

  it("issue: the same block, read-only, no Revoke", () => {
    renderWithI18n(wrap(<DecisionRequestCard card={card(applied)} inIssue />));
    expect(screen.getByTestId("decision-summary")).toHaveTextContent("Tofu, for Cheese");
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
  });
});

describe("collapse animation", () => {
  const rerender = (reduce: boolean, decided: RequestCard) => {
    reduceMotion(reduce);
    const open = card(undefined, { state: "open", answer: undefined });
    const tree = (c: RequestCard) => wrap(<DecisionRequestCard card={c} />);
    const result = renderWithI18n(tree(open));
    result.rerender(tree(decided));
    return result;
  };
  beforeEach(() => api.listDecisionArtifacts.mockResolvedValue([]));
  afterEach(() => vi.restoreAllMocks());

  it("collapses in 240 ms, then rings once for 600 ms, and expands in 200 ms", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      rerender(false, card(applied));
      const body = document.querySelector("[data-fold-body]") as HTMLElement;
      expect(body.style.transitionDuration).toBe("240ms");
      const block = screen.getByTestId("decision-request-card");
      expect(block).toHaveAttribute("data-ring", "true");
      await act(async () => { vi.advanceTimersByTime(600); });
      expect(block).toHaveAttribute("data-ring", "false");
      await userEvent.setup({ advanceTimers: vi.advanceTimersByTime }).click(screen.getByTestId("decision-request-fold-toggle"));
      expect(body.style.transitionDuration).toBe("200ms");
    } finally {
      vi.useRealTimers();
    }
  });

  it("gives no ring when the decision was not applied", () => {
    rerender(false, card({ ...base, state: "rejected", effect: "not_applied", effect_reason: "rejected" }));
    expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-ring", "false");
  });

  it("takes 0 ms and shows no ring under prefers-reduced-motion", () => {
    rerender(true, card(applied));
    const body = document.querySelector("[data-fold-body]") as HTMLElement;
    expect(body.style.transitionDuration).toBe("0ms");
    expect(screen.getByTestId("decision-request-card")).toHaveAttribute("data-ring", "false");
  });

  it("says what happened in a polite live region", () => {
    rerender(false, card(applied));
    expect(document.querySelector("[aria-live=polite]")).toHaveTextContent("Approved. Applied.");
  });
});

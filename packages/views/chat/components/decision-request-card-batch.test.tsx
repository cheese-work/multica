import { createHash } from "node:crypto";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionRequestCard } from "./decision-request-card";

// Direction B v9 batch card (design/decision-card/approved): per-item Approve / Reject, none chosen on open,
// one button that counts what is left or names the result, one passkey for the approved items.

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
  getDecisionAction: vi.fn(),
  challengeDecisionBatch: vi.fn(),
  approveDecisionBatch: vi.fn(),
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

const item = (id: string, number: number) => ({
  action_id: id, issue_id: `issue-${id}`, issue_identifier: `CHE-${number}`, issue_title: `Title ${number}`, state: "pending",
});
const batch: RequestCard = {
  id: "request", issue_id: "issue-a1", issue_identifier: "CHE-1", issue_title: "Title 1", asking_agent_name: "x99-claude-gstack",
  ask: "**Proposed batch**\n3 × `issue.status.set`", state: "open", created_at: "2026-11-11T06:20:00Z",
  batch_id: "batch", batch_items: [item("a1", 1), item("a2", 2), item("a3", 3)],
};

const view = (card: RequestCard) => renderWithI18n(
  <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={card} /></WorkspaceSlugProvider></QueryClientProvider>,
);
const pick = (user: ReturnType<typeof userEvent.setup>, issue: string, name: "Approve" | "Reject") =>
  user.click(within(screen.getByRole("radiogroup", { name: `Decision for ${issue}` })).getByRole("radio", { name }));
const sha = (hashes: string[]) => createHash("sha256").update([...hashes].sort().join("\n")).digest("hex");

describe("DecisionRequestCard batch", () => {
  beforeEach(() => {
    sessionStorage.clear();
    api.listDecisionArtifacts.mockResolvedValue([]);
    api.getDecisionAction.mockReset().mockImplementation(async (id: string) => ({ id, payload_sha256: `hash-${id}` }));
    api.challengeDecisionBatch.mockReset().mockResolvedValue({ challenge_id: "ch" });
    api.approveDecisionBatch.mockReset();
    signPasskeyChallenge.mockReset().mockResolvedValue({ challenge_id: "ch", credential: "cred" });
  });

  it("opens with every item unchosen and a disabled button that counts what is left", async () => {
    const user = userEvent.setup();
    view(batch);
    for (const issue of ["CHE-1", "CHE-2", "CHE-3"]) {
      const group = await screen.findByRole("radiogroup", { name: `Decision for ${issue}` });
      for (const radio of within(group).getAllByRole("radio")) expect(radio).toHaveAttribute("aria-checked", "false");
    }
    expect(screen.getByRole("button", { name: "Choose 3 more items" })).toBeDisabled();
    await pick(user, "CHE-1", "Approve");
    expect(screen.getByRole("button", { name: "Choose 2 more items" })).toBeDisabled();
    await pick(user, "CHE-2", "Reject");
    expect(screen.getByRole("button", { name: "Choose 1 more item" })).toBeDisabled();
    await user.click(within(screen.getByRole("radiogroup", { name: "Decision for CHE-1" })).getByRole("radio", { name: "Approve" }));
    expect(screen.getByRole("button", { name: "Choose 2 more items" })).toBeDisabled();
  });

  it("names the mixed result and ends with the send key", async () => {
    const user = userEvent.setup();
    view(batch);
    await screen.findByRole("radiogroup", { name: "Decision for CHE-1" });
    await pick(user, "CHE-1", "Approve");
    await pick(user, "CHE-2", "Approve");
    await pick(user, "CHE-3", "Reject");
    const send = screen.getByRole("button", { name: /^Approve 2, Reject 1 with passkey \(.+\)$/ });
    expect(send).toBeEnabled();
  });

  it("signs once over the approved items and shows each item's own effect", async () => {
    const user = userEvent.setup();
    api.approveDecisionBatch.mockResolvedValue({
      batch_id: "batch",
      items: [
        { id: "a1", state: "executed" },
        { id: "a2", state: "failed", error_code: "precondition_drift", error_message: "the issue moved" },
        { id: "a3", state: "rejected" },
      ],
    });
    view(batch);
    await screen.findByRole("radiogroup", { name: "Decision for CHE-1" });
    await pick(user, "CHE-1", "Approve");
    await pick(user, "CHE-2", "Approve");
    await pick(user, "CHE-3", "Reject");
    await user.click(screen.getByRole("button", { name: /^Approve 2, Reject 1 with passkey/ }));
    const choices = { a1: "approve", a2: "approve", a3: "reject" };
    await waitFor(() => expect(api.approveDecisionBatch).toHaveBeenCalledOnce());
    expect(api.challengeDecisionBatch).toHaveBeenCalledWith("batch", choices);
    expect(signPasskeyChallenge).toHaveBeenCalledOnce();
    expect(api.approveDecisionBatch).toHaveBeenCalledWith("batch", {
      payload_sha256: sha(["hash-a1", "hash-a2"]),
      assertion: { challenge_id: "ch", credential: "cred" },
      choices,
    });
    expect(await screen.findByTestId("decision-batch-effect-a1")).toHaveTextContent("Applied.");
    expect(screen.getByTestId("decision-batch-effect-a2")).toHaveTextContent("Not applied: the issue moved.");
    expect(screen.getByTestId("decision-batch-effect-a3")).toHaveTextContent("Not applied. You rejected it. Nothing ran.");
    expect(screen.queryByRole("radiogroup")).toBeNull();
  });

  it("rejects the whole batch without a passkey", async () => {
    const user = userEvent.setup();
    api.approveDecisionBatch.mockResolvedValue({ batch_id: "batch", items: ["a1", "a2", "a3"].map((id) => ({ id, state: "rejected" })) });
    view(batch);
    await screen.findByRole("radiogroup", { name: "Decision for CHE-1" });
    for (const issue of ["CHE-1", "CHE-2", "CHE-3"]) await pick(user, issue, "Reject");
    await user.click(screen.getByRole("button", { name: /^Confirm Reject 3 \(.+\)$/ }));
    await waitFor(() => expect(api.approveDecisionBatch).toHaveBeenCalledOnce());
    expect(signPasskeyChallenge).not.toHaveBeenCalled();
    expect(api.challengeDecisionBatch).not.toHaveBeenCalled();
    expect(api.approveDecisionBatch).toHaveBeenCalledWith("batch", { payload_sha256: "", choices: { a1: "reject", a2: "reject", a3: "reject" } });
  });

  it("sends nothing and says so when the passkey prompt is cancelled", async () => {
    const user = userEvent.setup();
    signPasskeyChallenge.mockRejectedValue(new Error("cancelled"));
    view(batch);
    await screen.findByRole("radiogroup", { name: "Decision for CHE-1" });
    for (const issue of ["CHE-1", "CHE-2", "CHE-3"]) await pick(user, issue, "Approve");
    await user.click(screen.getByRole("button", { name: /^Approve 3 with passkey/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not send. Try again.");
    expect(api.approveDecisionBatch).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: /^Approve 3 with passkey/ })).toBeEnabled();
  });

  // jsdom has no layout, so assert the classes that keep the card inside 358 px: shrinkable rows that wrap, and 44 px touch targets.
  it("keeps rows shrinkable and wrapping with phone-size targets", async () => {
    view(batch);
    const group = await screen.findByRole("radiogroup", { name: "Decision for CHE-1" });
    const row = group.closest("li")!;
    expect(row).toHaveClass("min-w-0", "flex-wrap");
    expect(screen.getByTestId("decision-batch-form")).toHaveClass("min-w-0");
    for (const radio of within(group).getAllByRole("radio")) expect(radio).toHaveClass("max-sm:min-h-11");
    expect(screen.getByRole("button", { name: "Choose 3 more items" })).toHaveClass("max-sm:h-11");
  });

  it("shows decided items as read-only effects, with no choice", async () => {
    view({
      ...batch,
      batch_items: [
        { ...item("a1", 1), state: "executed" },
        { ...item("a2", 2), state: "failed", error_code: "precondition_drift" },
        { ...item("a3", 3), state: "rejected" },
      ],
    });
    expect(await screen.findByTestId("decision-batch-effect-a2")).toHaveTextContent("Not applied: precondition_drift.");
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.queryByRole("button", { name: /Choose|passkey/ })).toBeNull();
  });
});

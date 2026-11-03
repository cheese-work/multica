import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { ApiError } from "@multica/core/api/client";
import { renderWithI18n } from "../test/i18n";
import { DecisionActionApproval } from "./decision-action-approval";

const api = vi.hoisted(() => ({
  challengeDecisionAction: vi.fn(),
  approveDecisionAction: vi.fn(),
  getDecisionActionOutcome: vi.fn(),
}));
const sign = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", () => ({ api }));
vi.mock("@multica/core/auth/webauthn", () => ({ signPasskeyChallenge: sign }));

const outcome = (state: string, error_code?: string) => ({ id: "a1", state, ...(error_code && { error_code }) });
const envelope = { challenge_id: "ch", credential: { id: "k" } };

function renderApproval(onEnroll = vi.fn()) {
  renderWithI18n(<DecisionActionApproval actionId="a1" payloadSha256="abc" onEnroll={onEnroll} />);
  return {
    onEnroll,
    approve: async () => fireEvent.click(await screen.findByRole("button", { name: "Approve with passkey" })),
  };
}

beforeEach(() => {
  Object.values(api).forEach((fn) => fn.mockReset());
  sign.mockReset();
  api.challengeDecisionAction.mockResolvedValue({ challenge_id: "ch", options: { publicKey: {} }, expires_at: "t" });
  api.getDecisionActionOutcome.mockResolvedValue(outcome("pending"));
  sign.mockResolvedValue(envelope);
});
afterEach(cleanup);

describe("DecisionActionApproval", () => {
  it("runs challenge, assertion and approve, then shows executed", async () => {
    api.approveDecisionAction.mockResolvedValue(outcome("executed"));
    await renderApproval().approve();
    expect(await screen.findByText("Executed")).toBeTruthy();
    expect(api.challengeDecisionAction).toHaveBeenCalledWith("a1");
    expect(api.approveDecisionAction).toHaveBeenCalledWith("a1", { payload_sha256: "abc", assertion: envelope });
  });

  it.each([
    ["stale_cap", "A higher cap is already in force."],
    ["stale_window", "The budget window changed. Start a new request."],
    ["not_budgeted", "This issue is no longer budgeted."],
  ])("shows failed with code %s", async (code, text) => {
    api.approveDecisionAction.mockResolvedValue(outcome("failed", code));
    await renderApproval().approve();
    expect(await screen.findByText("Failed")).toBeTruthy();
    expect(screen.getByText(text)).toBeTruthy();
  });

  it.each([["rejected", "Rejected"], ["expired", "Expired"]])("shows %s", async (state, label) => {
    api.approveDecisionAction.mockResolvedValue(outcome(state));
    await renderApproval().approve();
    expect(await screen.findByText(label)).toBeTruthy();
  });

  it("prompts enrollment when the user has no passkey", async () => {
    api.challengeDecisionAction.mockRejectedValue(new ApiError("no_passkey: enroll a passkey before approving", 409, "Conflict"));
    const { approve, onEnroll } = renderApproval();
    await approve();
    fireEvent.click(await screen.findByRole("button", { name: "Add a passkey" }));
    expect(onEnroll).toHaveBeenCalledOnce();
    expect(api.approveDecisionAction).not.toHaveBeenCalled();
  });

  it("reads the outcome after a lost response and never approves twice", async () => {
    api.approveDecisionAction.mockRejectedValue(new TypeError("network error"));
    api.getDecisionActionOutcome.mockResolvedValueOnce(outcome("pending")).mockResolvedValueOnce(outcome("pending")).mockResolvedValueOnce(outcome("executed"));
    await renderApproval().approve();
    fireEvent.click(await screen.findByRole("button", { name: "Check outcome" }));
    expect(await screen.findByText("Executed")).toBeTruthy();
    expect(api.getDecisionActionOutcome).toHaveBeenCalledTimes(3);
    expect(api.approveDecisionAction).toHaveBeenCalledOnce();
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
  });

  it("lets the person retry when the passkey ceremony is cancelled before approve", async () => {
    sign.mockRejectedValueOnce(new Error("passkey sign-in was cancelled"));
    api.approveDecisionAction.mockResolvedValue(outcome("executed"));
    const { approve } = renderApproval();
    await approve();
    await waitFor(() => expect(screen.getByRole("alert")).toBeTruthy());
    expect(api.approveDecisionAction).not.toHaveBeenCalled();
    await approve();
    expect(await screen.findByText("Executed")).toBeTruthy();
  });

  it("reads the outcome on mount and offers no approve for a decided action", async () => {
    api.getDecisionActionOutcome.mockResolvedValue(outcome("executed"));
    renderApproval();
    expect(await screen.findByText("Executed")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
    expect(api.challengeDecisionAction).not.toHaveBeenCalled();
  });

  it("reads the outcome when the challenge says the action is no longer pending", async () => {
    api.challengeDecisionAction.mockRejectedValue(new ApiError("not_pending: this action is no longer waiting for a decision", 409, "Conflict"));
    const { approve } = renderApproval();
    api.getDecisionActionOutcome.mockResolvedValue(outcome("executed"));
    await approve();
    expect(await screen.findByText("Executed")).toBeTruthy();
    expect(api.approveDecisionAction).not.toHaveBeenCalled();
  });

  it("shows expired when the challenge says the approval window closed", async () => {
    api.challengeDecisionAction.mockRejectedValue(new ApiError("expired: this action's approval window has closed", 409, "Conflict"));
    await renderApproval().approve();
    expect(await screen.findByText("Expired")).toBeTruthy();
    expect(api.approveDecisionAction).not.toHaveBeenCalled();
  });
});

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { ApiError } from "@multica/core/api/client";
import { renderWithI18n } from "../test/i18n";
import { DecisionActionApproval } from "./decision-action-approval";

const api = vi.hoisted(() => ({
  challengeDecisionAction: vi.fn(),
  approveDecisionAction: vi.fn(),
  rejectDecisionAction: vi.fn(),
  getDecisionActionOutcome: vi.fn(),
}));
const sign = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", () => ({ api }));
vi.mock("@multica/core/auth/webauthn", () => ({ signPasskeyChallenge: sign }));

const outcome = (state: string, error_code?: string) => ({ id: "a1", state, ...(error_code && { error_code }) });
const envelope = { challenge_id: "ch", credential: { id: "k" } };

function renderApproval(onEnroll = vi.fn(), previewSha256?: string) {
  renderWithI18n(<DecisionActionApproval actionId="a1" payloadSha256="abc" previewSha256={previewSha256} onEnroll={onEnroll} />);
  return {
    onEnroll,
    approve: async () => fireEvent.click(await screen.findByRole("button", { name: "Approve with passkey" })),
  };
}

beforeEach(() => {
  sessionStorage.clear();
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

  it("sends the digest of the preview it was shown, only when it was shown one", async () => {
    api.approveDecisionAction.mockResolvedValue(outcome("executed"));
    await renderApproval(vi.fn(), "d".repeat(64)).approve();
    await screen.findByText("Executed");
    expect(api.approveDecisionAction).toHaveBeenCalledWith("a1", { payload_sha256: "abc", preview_sha256: "d".repeat(64), assertion: envelope });
  });

  it("shows a preview_required refusal and does not offer or send a second approve", async () => {
    api.approveDecisionAction.mockRejectedValue(new ApiError("preview_required: approve with the digest of the preview you were shown", 409, "Conflict"));
    await renderApproval(vi.fn(), "d".repeat(64)).approve();
    expect(await screen.findByRole("alert")).toHaveTextContent("The changes shown no longer match the request. Reload the card and review again.");
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
    expect(api.approveDecisionAction).toHaveBeenCalledTimes(1);
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

  it.each([
    [{ outcome: "cap_applied", from: 10, to: 30 }, ["Cap applied", "10 → 30"]],
    [{ outcome: "run_queued" }, ["Run queued"]],
    [{ outcome: "still_blocked:budget_exhausted" }, ["Still blocked: budget_exhausted"]],
    [{ outcome: "future_thing" }, ["Executed"]],
    [undefined, ["Executed"]],
  ])("shows the executed outcome %j", async (result, texts) => {
    api.approveDecisionAction.mockResolvedValue({ ...outcome("executed"), result });
    await renderApproval().approve();
    for (const text of texts) expect(await screen.findByText(text)).toBeTruthy();
  });

  it("shows stale_base as a failed approval that needs a new request", async () => {
    api.approveDecisionAction.mockResolvedValue(outcome("failed", "stale_base"));
    await renderApproval().approve();
    expect(await screen.findByText("Failed")).toBeTruthy();
    expect(screen.getByText("The target changed after the request. Ask again.")).toBeTruthy();
  });

  it("reads the outcome again when the card around it reports a new action state", async () => {
    const view = renderWithI18n(<DecisionActionApproval actionId="a1" payloadSha256="abc" state="pending" />);
    await screen.findByRole("button", { name: "Approve with passkey" });
    api.getDecisionActionOutcome.mockResolvedValue({ ...outcome("executed"), result: { outcome: "run_queued" } });
    view.rerender(<DecisionActionApproval actionId="a1" payloadSha256="abc" state="executed" />);
    expect(await screen.findByText("Run queued")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
  });

  it("rejects without a passkey and shows rejected", async () => {
    api.rejectDecisionAction.mockResolvedValue(outcome("rejected"));
    renderApproval();
    fireEvent.click(await screen.findByRole("button", { name: "Reject" }));
    expect(await screen.findByText("Rejected")).toBeTruthy();
    expect(api.rejectDecisionAction).toHaveBeenCalledWith("a1");
    expect(api.challengeDecisionAction).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
  });

  it("reads the outcome back when the reject response is lost", async () => {
    api.rejectDecisionAction.mockRejectedValue(new Error("network"));
    api.getDecisionActionOutcome.mockResolvedValueOnce(outcome("pending")).mockResolvedValue(outcome("rejected"));
    renderApproval();
    fireEvent.click(await screen.findByRole("button", { name: "Reject" }));
    expect(await screen.findByText("Rejected")).toBeTruthy();
  });

  it("keeps both buttons and shows an error when reject is refused", async () => {
    api.rejectDecisionAction.mockRejectedValue(new ApiError("busy", 409, "Conflict"));
    renderApproval();
    fireEvent.click(await screen.findByRole("button", { name: "Reject" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reject" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Approve with passkey" })).toBeTruthy();
  });

  it("prompts enrollment when the user has no passkey", async () => {
    api.challengeDecisionAction.mockRejectedValue(new ApiError("no_passkey: enroll a passkey before approving", 409, "Conflict"));
    const { approve, onEnroll } = renderApproval();
    await approve();
    fireEvent.click(await screen.findByRole("button", { name: "Add a passkey" }));
    expect(onEnroll).toHaveBeenCalledOnce();
    expect(api.approveDecisionAction).not.toHaveBeenCalled();
  });

  it("still lets the user reject after the no-passkey prompt", async () => {
    api.challengeDecisionAction.mockRejectedValue(new ApiError("no_passkey: enroll a passkey before approving", 409, "Conflict"));
    api.rejectDecisionAction.mockResolvedValue(outcome("rejected"));
    await renderApproval().approve();
    await screen.findByRole("button", { name: "Add a passkey" });
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));
    expect(await screen.findByText("Rejected")).toBeTruthy();
    expect(api.rejectDecisionAction).toHaveBeenCalledWith("a1");
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

  it("stays readback-only after a remount following a lost response", async () => {
    api.approveDecisionAction.mockRejectedValue(new TypeError("network error"));
    await renderApproval().approve();
    await screen.findByRole("button", { name: "Check outcome" });
    cleanup();
    renderApproval();
    expect(await screen.findByRole("button", { name: "Check outcome" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
    expect(api.approveDecisionAction).toHaveBeenCalledOnce();
  });
});

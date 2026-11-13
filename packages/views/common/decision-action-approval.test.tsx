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

function renderApproval(onEnroll = vi.fn(), onOutcome?: (state: string) => void) {
  renderWithI18n(<DecisionActionApproval actionId="a1" payloadSha256="abc" onEnroll={onEnroll} onOutcome={onOutcome} />);
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
    expect(await screen.findByText("Applied")).toBeTruthy();
    expect(api.challengeDecisionAction).toHaveBeenCalledWith("a1");
    expect(api.approveDecisionAction).toHaveBeenCalledWith("a1", { payload_sha256: "abc", assertion: envelope });
  });

  it("reports a terminal outcome once, so the caller can refresh what the action changed", async () => {
    api.approveDecisionAction.mockResolvedValue(outcome("executed"));
    const onOutcome = vi.fn();
    await renderApproval(vi.fn(), onOutcome).approve();
    await screen.findByText("Applied");
    expect(onOutcome).toHaveBeenCalledTimes(1);
    expect(onOutcome).toHaveBeenCalledWith("executed");
  });

  it("does not report an outcome while the action is still pending", async () => {
    const onOutcome = vi.fn();
    renderApproval(vi.fn(), onOutcome);
    await screen.findByRole("button", { name: "Approve with passkey" });
    expect(onOutcome).not.toHaveBeenCalled();
  });

  it.each([
    ["stale_cap", "A higher cap is already in force."],
    ["stale_window", "The budget window changed. Start a new request."],
    ["not_budgeted", "This issue is no longer budgeted."],
  ])("shows failed with code %s", async (code, text) => {
    api.approveDecisionAction.mockResolvedValue(outcome("failed", code));
    await renderApproval().approve();
    expect(await screen.findByText("Not applied")).toBeTruthy();
    expect(screen.getByText(text)).toBeTruthy();
  });

  it("shows the reason an action was not applied", async () => {
    api.approveDecisionAction.mockResolvedValue({ ...outcome("failed", "boom"), error_message: "refused by the runtime" });
    await renderApproval().approve();
    expect(await screen.findByText("Not applied")).toBeTruthy();
    expect(screen.getByText("refused by the runtime")).toBeTruthy();
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
    [{ outcome: "future_thing" }, ["Applied"]],
    [undefined, ["Applied"]],
  ])("shows the executed outcome %j", async (result, texts) => {
    api.approveDecisionAction.mockResolvedValue({ ...outcome("executed"), result });
    await renderApproval().approve();
    for (const text of texts) expect(await screen.findByText(text)).toBeTruthy();
  });

  it("shows stale_base as a failed approval that needs a new request", async () => {
    api.approveDecisionAction.mockResolvedValue(outcome("failed", "stale_base"));
    await renderApproval().approve();
    expect(await screen.findByText("Not applied")).toBeTruthy();
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
    expect(await screen.findByText("Applied")).toBeTruthy();
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
    expect(await screen.findByText("Applied")).toBeTruthy();
  });

  it("reads the outcome on mount and offers no approve for a decided action", async () => {
    api.getDecisionActionOutcome.mockResolvedValue(outcome("executed"));
    renderApproval();
    expect(await screen.findByText("Applied")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
    expect(api.challengeDecisionAction).not.toHaveBeenCalled();
  });

  it("reads the outcome when the challenge says the action is no longer pending", async () => {
    api.challengeDecisionAction.mockRejectedValue(new ApiError("not_pending: this action is no longer waiting for a decision", 409, "Conflict"));
    const { approve } = renderApproval();
    api.getDecisionActionOutcome.mockResolvedValue(outcome("executed"));
    await approve();
    expect(await screen.findByText("Applied")).toBeTruthy();
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

  it("approves with the digest of the preview the person was shown", async () => {
    api.approveDecisionAction.mockResolvedValue(outcome("executed"));
    renderWithI18n(<DecisionActionApproval actionId="a1" payloadSha256="abc" previewSha256="def" />);
    fireEvent.click(await screen.findByRole("button", { name: "Approve with passkey" }));
    expect(await screen.findByText("Applied")).toBeTruthy();
    expect(api.approveDecisionAction).toHaveBeenCalledWith("a1", { payload_sha256: "abc", assertion: envelope, preview_sha256: "def" });
  });

  it("offers no approve button when the person may not approve, and still allows reject", async () => {
    api.rejectDecisionAction.mockResolvedValue(outcome("rejected"));
    renderWithI18n(<DecisionActionApproval actionId="a1" payloadSha256="abc" canApprove={false} />);
    expect(await screen.findByRole("button", { name: "Reject" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
    expect(screen.getByText("Only the workspace owner can approve this change.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));
    expect(await screen.findByText("Rejected")).toBeTruthy();
    expect(api.challengeDecisionAction).not.toHaveBeenCalled();
  });

  it.each([
    ["class_needs_owner", "Only the workspace owner can approve this change."],
    ["payload_mismatch", "The approved change is not the change that was requested. Ask again."],
    ["preview_required", "Approve against the preview you were shown. Reload the card."],
    ["precondition_drift", "The target changed since the request. Ask again."],
    ["busy_retry", "Another change is in progress. Try again."],
    ["not_permitted", "You may not make this change."],
    ["action_not_executable", "This kind of change cannot be approved yet."],
    ["approval_proof_unavailable", "This server cannot verify a passkey approval yet."],
  ])("shows the refusal %s with its code", async (code, text) => {
    api.approveDecisionAction.mockRejectedValue(new ApiError(`${code}: refused by the server`, 409, "Conflict"));
    await renderApproval().approve();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(text);
    expect(alert.textContent).toContain(code);
    expect(api.approveDecisionAction).toHaveBeenCalledOnce();
  });

  it("shows action_not_executable with its code when the server answers 501", async () => {
    api.approveDecisionAction.mockRejectedValue(new ApiError("action_not_executable: this action type cannot be approved yet", 501, "Not Implemented"));
    await renderApproval().approve();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("This kind of change cannot be approved yet.");
    expect(alert.textContent).toContain("action_not_executable");
    expect(screen.queryByTestId("decision-action-approve")).toBeNull();
    expect(api.approveDecisionAction).toHaveBeenCalledOnce();
  });

  it("shows an unknown refusal code verbatim", async () => {
    api.approveDecisionAction.mockRejectedValue(new ApiError("future_code: a newer refusal", 409, "Conflict"));
    await renderApproval().approve();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("future_code");
  });

  it("keeps Approve disabled after preview_required until the card shows a different preview, and never retries", async () => {
    api.approveDecisionAction.mockRejectedValue(new ApiError("preview_required: approve against the stored preview", 409, "Conflict"));
    const view = renderWithI18n(<DecisionActionApproval actionId="a1" payloadSha256="abc" previewSha256="old" />);
    fireEvent.click(await screen.findByRole("button", { name: "Approve with passkey" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("preview_required");
    expect(screen.getByRole("button", { name: "Approve with passkey" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Reject" })).toBeEnabled();
    expect(api.approveDecisionAction).toHaveBeenCalledOnce();
    view.rerender(<DecisionActionApproval actionId="a1" payloadSha256="abc" previewSha256="old" />);
    expect(screen.getByRole("button", { name: "Approve with passkey" })).toBeDisabled();
    view.rerender(<DecisionActionApproval actionId="a1" payloadSha256="abc" previewSha256="new" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Approve with passkey" })).toBeEnabled());
    expect(api.approveDecisionAction).toHaveBeenCalledOnce();
  });

  it("shows a refusal of the challenge too", async () => {
    api.challengeDecisionAction.mockRejectedValue(new ApiError("class_needs_owner: this action needs the workspace owner", 403, "Forbidden"));
    await renderApproval().approve();
    expect((await screen.findByRole("alert")).textContent).toContain("class_needs_owner");
    expect(api.approveDecisionAction).not.toHaveBeenCalled();
  });

  it("removes the approve button after an owner refusal", async () => {
    api.approveDecisionAction.mockRejectedValue(new ApiError("class_needs_owner: this action needs the workspace owner", 403, "Forbidden"));
    await renderApproval().approve();
    await screen.findByRole("alert");
    expect(screen.queryByRole("button", { name: "Approve with passkey" })).toBeNull();
    expect(screen.getByRole("button", { name: "Reject" })).toBeTruthy();
  });
});

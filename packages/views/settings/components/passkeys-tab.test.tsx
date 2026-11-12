import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { toast } from "sonner";
import { renderWithI18n } from "../../test/i18n";

const mocks = vi.hoisted(() => ({
  listPasskeys: vi.fn(),
  startPasskeyEnrollment: vi.fn(),
  finishPasskeyEnrollment: vi.fn(),
  challengePasskeyRevocation: vi.fn(),
  revokePasskey: vi.fn(),
  startPasskeyRevocationByCode: vi.fn(),
  confirmPasskeyRevocationByCode: vi.fn(),
  createPasskey: vi.fn(),
  signPasskeyChallenge: vi.fn(),
  openExternal: vi.fn(),
  desktop: false,
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@multica/core/api", () => ({
  api: {
    listPasskeys: mocks.listPasskeys,
    startPasskeyEnrollment: mocks.startPasskeyEnrollment,
    finishPasskeyEnrollment: mocks.finishPasskeyEnrollment,
    challengePasskeyRevocation: mocks.challengePasskeyRevocation,
    revokePasskey: mocks.revokePasskey,
    startPasskeyRevocationByCode: mocks.startPasskeyRevocationByCode,
    confirmPasskeyRevocationByCode: mocks.confirmPasskeyRevocationByCode,
  },
}));
vi.mock("@multica/core/auth/webauthn", () => ({
  createPasskey: mocks.createPasskey,
  signPasskeyChallenge: mocks.signPasskeyChallenge,
}));
vi.mock("../../platform", () => ({ openExternal: mocks.openExternal }));
vi.mock("../../platform/local-directory", () => ({ isDesktopShell: () => mocks.desktop }));
vi.mock("../../navigation", () => ({
  useNavigation: () => ({
    pathname: "/acme/settings",
    searchParams: new URLSearchParams("tab=profile"),
    hash: "",
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    getShareableUrl: (path: string) => `https://app.example.test${path}`,
  }),
}));

import { PasskeysTab } from "./passkeys-tab";

const key = (id: string, over: Record<string, unknown> = {}) => ({
  id, created_at: "2026-10-01T00:00:00Z", last_used_at: null, transports: ["usb"], state: "active", ...over,
});
const challenge = { challenge_id: "ch1", options: { publicKey: { challenge: "AQ" } }, expires_at: "t" };
const start = { ceremony_id: "ce1", options: { publicKey: { challenge: "AQ" } }, expires_at: "t" };

beforeEach(() => {
  vi.clearAllMocks();
  mocks.desktop = false;
  mocks.listPasskeys.mockResolvedValue([key("p1"), key("p2", { last_used_at: "2026-10-05T00:00:00Z" })]);
});

describe("PasskeysTab", () => {
  it("lists the enrolled passkeys", async () => {
    renderWithI18n(<PasskeysTab />);
    expect(await screen.findAllByRole("button", { name: /^Revoke passkey/ })).toHaveLength(2);
    expect(screen.getByText(/Never used/)).toBeInTheDocument();
  });

  it("shows an empty state", async () => {
    mocks.listPasskeys.mockResolvedValue([]);
    renderWithI18n(<PasskeysTab />);
    expect(await screen.findByText("No passkeys yet.")).toBeInTheDocument();
  });

  it("shows a load failure instead of an empty list", async () => {
    mocks.listPasskeys.mockRejectedValue(new Error("boom"));
    renderWithI18n(<PasskeysTab />);
    expect(await screen.findByText(/Could not load your passkeys/)).toBeInTheDocument();
    expect(screen.queryByText("No passkeys yet.")).not.toBeInTheDocument();
  });

  it("enrolls: mails the code, creates the credential, finishes, refreshes", async () => {
    const user = userEvent.setup();
    mocks.startPasskeyEnrollment.mockResolvedValue(start);
    mocks.createPasskey.mockResolvedValue({ id: "cred" });
    mocks.finishPasskeyEnrollment.mockResolvedValue(undefined);
    renderWithI18n(<PasskeysTab />);
    await screen.findAllByRole("button", { name: /^Revoke passkey/ });

    await user.click(screen.getByRole("button", { name: "Add passkey" }));
    await user.type(await screen.findByLabelText("Email code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(mocks.finishPasskeyEnrollment).toHaveBeenCalledWith({
      ceremony_id: "ce1", code: "123456", attestation: { id: "cred" },
    }));
    expect(mocks.createPasskey).toHaveBeenCalledWith(start.options);
    await waitFor(() => expect(mocks.listPasskeys).toHaveBeenCalledTimes(2));
    expect(toast.success).toHaveBeenCalled();
  });

  it("shows the attempt tag next to the code field so the user picks the matching email", async () => {
    const user = userEvent.setup();
    mocks.startPasskeyEnrollment.mockResolvedValue({ ...start, tag: "ab12" });
    renderWithI18n(<PasskeysTab />);
    await screen.findAllByRole("button", { name: /^Revoke passkey/ });
    await user.click(screen.getByRole("button", { name: "Add passkey" }));
    expect(await screen.findByText(/Attempt ab12/)).toBeTruthy();
  });

  it("starts one ceremony on a double click of Add passkey", async () => {
    const user = userEvent.setup();
    mocks.startPasskeyEnrollment.mockResolvedValue(start);
    renderWithI18n(<PasskeysTab />);
    await screen.findAllByRole("button", { name: /^Revoke passkey/ });
    await user.dblClick(screen.getByRole("button", { name: "Add passkey" }));
    await screen.findByLabelText("Email code");
    expect(mocks.startPasskeyEnrollment).toHaveBeenCalledTimes(1);
  });

  it("signs the gate assertion first when enrollment start returns one", async () => {
    const user = userEvent.setup();
    const envelope = { challenge_id: "ch1", credential: { id: "sig" } };
    mocks.startPasskeyEnrollment.mockResolvedValue({ ...start, assertion: challenge });
    mocks.signPasskeyChallenge.mockResolvedValue(envelope);
    mocks.createPasskey.mockResolvedValue({ id: "cred" });
    mocks.finishPasskeyEnrollment.mockResolvedValue(undefined);
    renderWithI18n(<PasskeysTab />);
    await screen.findAllByRole("button", { name: /^Revoke passkey/ });

    await user.click(screen.getByRole("button", { name: "Add passkey" }));
    await user.type(await screen.findByLabelText("Email code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(mocks.finishPasskeyEnrollment).toHaveBeenCalledWith(
      expect.objectContaining({ assertion: envelope }),
    ));
    expect(mocks.signPasskeyChallenge).toHaveBeenCalledWith(challenge);
  });

  it("reports an enrollment failure and leaves the list alone", async () => {
    const user = userEvent.setup();
    mocks.startPasskeyEnrollment.mockResolvedValue(start);
    mocks.createPasskey.mockRejectedValue(new Error("passkey creation was cancelled"));
    renderWithI18n(<PasskeysTab />);
    await screen.findAllByRole("button", { name: /^Revoke passkey/ });

    await user.click(screen.getByRole("button", { name: "Add passkey" }));
    await user.type(await screen.findByLabelText("Email code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("passkey creation was cancelled"));
    expect(mocks.finishPasskeyEnrollment).not.toHaveBeenCalled();
    expect(mocks.listPasskeys).toHaveBeenCalledTimes(1);
    expect(toast.success).not.toHaveBeenCalled();
  });

  it("reports a start failure", async () => {
    const user = userEvent.setup();
    mocks.startPasskeyEnrollment.mockRejectedValue(new Error("passkey enrollment is not open"));
    renderWithI18n(<PasskeysTab />);
    await screen.findAllByRole("button", { name: /^Revoke passkey/ });
    await user.click(screen.getByRole("button", { name: "Add passkey" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("passkey enrollment is not open"));
    expect(screen.queryByLabelText("Email code")).not.toBeInTheDocument();
  });

  it("revokes after confirmation: challenge, sign, revoke, refresh", async () => {
    const user = userEvent.setup();
    const envelope = { challenge_id: "ch1", credential: { id: "sig" } };
    mocks.challengePasskeyRevocation.mockResolvedValue(challenge);
    mocks.signPasskeyChallenge.mockResolvedValue(envelope);
    mocks.revokePasskey.mockResolvedValue(undefined);
    renderWithI18n(<PasskeysTab />);

    const buttons = await screen.findAllByRole("button", { name: /^Revoke passkey/ });
    await user.click(buttons[0]!);
    expect(mocks.challengePasskeyRevocation).not.toHaveBeenCalled();
    await user.click(await screen.findByRole("button", { name: "Revoke" }));

    await waitFor(() => expect(mocks.revokePasskey).toHaveBeenCalledWith("p1", envelope));
    expect(mocks.challengePasskeyRevocation).toHaveBeenCalledWith("p1");
    expect(mocks.signPasskeyChallenge).toHaveBeenCalledWith(challenge);
    await waitFor(() => expect(mocks.listPasskeys).toHaveBeenCalledTimes(2));
    expect(toast.success).toHaveBeenCalled();
  });

  it("reports a revoke failure and keeps the passkey", async () => {
    const user = userEvent.setup();
    mocks.challengePasskeyRevocation.mockRejectedValue(new Error("no_passkey: no usable passkey to sign with"));
    renderWithI18n(<PasskeysTab />);
    const buttons = await screen.findAllByRole("button", { name: /^Revoke passkey/ });
    await user.click(buttons[0]!);
    await user.click(await screen.findByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("no_passkey: no usable passkey to sign with"));
    expect(mocks.revokePasskey).not.toHaveBeenCalled();
    expect(mocks.listPasskeys).toHaveBeenCalledTimes(1);
  });

  it("revokes the only active passkey with a mailed code, since no other key can sign", async () => {
    const user = userEvent.setup();
    mocks.listPasskeys.mockResolvedValue([key("p1")]);
    mocks.startPasskeyRevocationByCode.mockResolvedValue({ ceremony_id: "rc1", expires_at: "t" });
    mocks.confirmPasskeyRevocationByCode.mockResolvedValue(undefined);
    renderWithI18n(<PasskeysTab />);

    await user.click(await screen.findByRole("button", { name: "Revoke passkey 1" }));
    await user.click(await screen.findByRole("button", { name: "Revoke" }));
    await user.type(await screen.findByLabelText("Email code"), "654321");
    await user.click(screen.getByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(mocks.confirmPasskeyRevocationByCode).toHaveBeenCalledWith("p1", { ceremony_id: "rc1", code: "654321" }));
    expect(mocks.startPasskeyRevocationByCode).toHaveBeenCalledWith("p1");
    expect(mocks.challengePasskeyRevocation).not.toHaveBeenCalled();
    await waitFor(() => expect(mocks.listPasskeys).toHaveBeenCalledTimes(2));
    expect(toast.success).toHaveBeenCalled();
    expect(screen.queryByLabelText("Email code")).not.toBeInTheDocument();
  });

  it("keeps the passkey and shows the server reason when the code revoke is refused", async () => {
    const user = userEvent.setup();
    mocks.listPasskeys.mockResolvedValue([key("p1")]);
    mocks.startPasskeyRevocationByCode.mockResolvedValue({ ceremony_id: "rc1", expires_at: "t" });
    mocks.confirmPasskeyRevocationByCode.mockRejectedValue(new Error("passkey_floor: at least two passkeys must stay active"));
    renderWithI18n(<PasskeysTab />);

    await user.click(await screen.findByRole("button", { name: "Revoke passkey 1" }));
    await user.click(await screen.findByRole("button", { name: "Revoke" }));
    await user.type(await screen.findByLabelText("Email code"), "654321");
    await user.click(screen.getByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("passkey_floor: at least two passkeys must stay active"));
    expect(toast.success).not.toHaveBeenCalled();
    expect(mocks.listPasskeys).toHaveBeenCalledTimes(1);
  });

  it("labels a suspect passkey and revokes it by signing with an active one", async () => {
    const user = userEvent.setup();
    const envelope = { challenge_id: "ch1", credential: { id: "sig" } };
    mocks.listPasskeys.mockResolvedValue([key("p1"), key("p2", { state: "suspect" })]);
    mocks.challengePasskeyRevocation.mockResolvedValue(challenge);
    mocks.signPasskeyChallenge.mockResolvedValue(envelope);
    mocks.revokePasskey.mockResolvedValue(undefined);
    renderWithI18n(<PasskeysTab />);

    expect(await screen.findAllByText("Suspect")).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: "Revoke passkey 2" }));
    await user.click(await screen.findByRole("button", { name: "Revoke" }));

    await waitFor(() => expect(mocks.revokePasskey).toHaveBeenCalledWith("p2", envelope));
    expect(mocks.startPasskeyRevocationByCode).not.toHaveBeenCalled();
  });

  it("uses a mailed code for the last active passkey even when a suspect one remains", async () => {
    const user = userEvent.setup();
    mocks.listPasskeys.mockResolvedValue([key("p1"), key("p2", { state: "suspect" })]);
    mocks.startPasskeyRevocationByCode.mockResolvedValue({ ceremony_id: "rc1", expires_at: "t" });
    renderWithI18n(<PasskeysTab />);

    await user.click(await screen.findByRole("button", { name: "Revoke passkey 1" }));
    await user.click(await screen.findByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(mocks.startPasskeyRevocationByCode).toHaveBeenCalledWith("p1"));
    expect(mocks.challengePasskeyRevocation).not.toHaveBeenCalled();
  });

  it("offers an email code when the other active passkeys are lost too", async () => {
    const user = userEvent.setup();
    mocks.listPasskeys.mockResolvedValue([key("p1"), key("p2")]);
    mocks.startPasskeyRevocationByCode.mockResolvedValue({ ceremony_id: "rc1", expires_at: "t" });
    mocks.confirmPasskeyRevocationByCode.mockResolvedValue(undefined);
    renderWithI18n(<PasskeysTab />);

    await user.click(await screen.findByRole("button", { name: "Revoke passkey 1" }));
    await user.click(await screen.findByRole("button", { name: "Use an email code instead" }));
    await waitFor(() => expect(mocks.startPasskeyRevocationByCode).toHaveBeenCalledWith("p1"));
    expect(mocks.challengePasskeyRevocation).not.toHaveBeenCalled();
    expect(mocks.signPasskeyChallenge).not.toHaveBeenCalled();
    await user.type(await screen.findByLabelText("Email code"), "654321");
    await user.click(screen.getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(mocks.confirmPasskeyRevocationByCode).toHaveBeenCalledWith("p1", { ceremony_id: "rc1", code: "654321" }));
  });

  it("shows the server floor refusal when the code start is refused", async () => {
    const user = userEvent.setup();
    mocks.listPasskeys.mockResolvedValue([key("p1"), key("p2")]);
    mocks.startPasskeyRevocationByCode.mockRejectedValue(new Error("passkey_floor: the decision gate needs at least two active passkeys"));
    renderWithI18n(<PasskeysTab />);

    await user.click(await screen.findByRole("button", { name: "Revoke passkey 1" }));
    await user.click(await screen.findByRole("button", { name: "Use an email code instead" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("passkey_floor: the decision gate needs at least two active passkeys"));
    expect(screen.queryByLabelText("Email code")).not.toBeInTheDocument();
  });

  it("on desktop opens the web page in the system browser and does not call the API", async () => {
    mocks.desktop = true;
    const user = userEvent.setup();
    renderWithI18n(<PasskeysTab />);
    await user.click(await screen.findByRole("button", { name: "Open in browser" }));
    expect(mocks.openExternal).toHaveBeenCalledWith("https://app.example.test/acme/settings?tab=passkeys");
    expect(mocks.listPasskeys).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Add passkey" })).not.toBeInTheDocument();
  });
});

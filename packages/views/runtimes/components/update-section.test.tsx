// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { UpdateSection } from "./update-section";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

const { initiateUpdate, getUpdateResult } = vi.hoisted(() => ({
  initiateUpdate: vi.fn(),
  getUpdateResult: vi.fn(),
}));

vi.mock("@multica/core/api", () => ({
  api: { initiateUpdate, getUpdateResult },
}));

const SERVER_COMMIT = "0123456789abcdef0123456789abcdef01234567";
const SERVER_VERSION = "0.7.1-20261029-2357";

const NO_UPDATE = {
  available: false,
  serverCommit: SERVER_COMMIT,
  serverVersion: SERVER_VERSION,
  reason: "",
};
const UPDATE = { ...NO_UPDATE, available: true };

type Info = typeof NO_UPDATE;

function renderSection(props: {
  runtimeId: string | null;
  launchedBy?: string | null;
  currentVersion?: string;
  updateStatus?: Info;
}) {
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <UpdateSection
        runtimeId={props.runtimeId}
        currentVersion={props.currentVersion ?? "v0.4.0"}
        isOnline
        launchedBy={props.launchedBy}
        updateStatus={props.updateStatus ?? NO_UPDATE}
      />
    </I18nProvider>,
  );
}

let fetchSpy: ReturnType<typeof vi.fn>;

beforeEach(() => {
  initiateUpdate.mockReset();
  getUpdateResult.mockReset();
  fetchSpy = vi.fn().mockRejectedValue(new Error("no network in tests"));
  vi.stubGlobal("fetch", fetchSpy);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("UpdateSection source of truth", () => {
  it("never asks api.github.com whether an update exists", async () => {
    renderSection({ runtimeId: "runtime-1", updateStatus: UPDATE });
    await screen.findByRole("button", { name: "Update" });

    const urls = fetchSpy.mock.calls.map(([url]) => String(url));
    expect(urls.filter((u) => u.includes("api.github.com"))).toEqual([]);
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("shows the server version and short commit when the server says an update is available", () => {
    renderSection({ runtimeId: "runtime-1", updateStatus: UPDATE });

    expect(screen.getByText(`${SERVER_VERSION} (0123456)`)).toBeInTheDocument();
    expect(screen.getByText("available")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update" })).toBeInTheDocument();
  });

  it("shows Latest, not a badge, when the server says no update", () => {
    renderSection({ runtimeId: "runtime-1", updateStatus: NO_UPDATE });

    expect(screen.getByText("Latest")).toBeInTheDocument();
    expect(screen.queryByText("available")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Update" })).not.toBeInTheDocument();
  });

  it("ignores the version string: an old release-looking version with no server update shows no badge", () => {
    renderSection({ runtimeId: "runtime-1", currentVersion: "v0.1.0", updateStatus: NO_UPDATE });

    expect(screen.queryByText("available")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Update" })).not.toBeInTheDocument();
  });

  it("shows Local build, never Latest, when the daemon commit is unknown", () => {
    renderSection({
      runtimeId: "runtime-1",
      currentVersion: "dev",
      updateStatus: { ...NO_UPDATE, reason: "unknown_commit" },
    });

    expect(screen.getByText("Local build")).toBeInTheDocument();
    expect(screen.queryByText("Latest")).not.toBeInTheDocument();
    expect(screen.queryByText("available")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Update" })).not.toBeInTheDocument();
  });

  it.each(["no_build", "unsupported_os"])(
    "claims neither an update nor Latest when the server reports %s",
    (reason) => {
      renderSection({
        runtimeId: "runtime-1",
        updateStatus: { ...NO_UPDATE, serverCommit: "", serverVersion: "", reason },
      });

      expect(screen.queryByText("Latest")).not.toBeInTheDocument();
      expect(screen.queryByText("available")).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Update" })).not.toBeInTheDocument();
    },
  );

  it("starts the update against the server build", async () => {
    initiateUpdate.mockResolvedValue({ id: "upd-1" });
    getUpdateResult.mockResolvedValue({ status: "running" });
    renderSection({ runtimeId: "runtime-1", updateStatus: UPDATE });

    fireEvent.click(screen.getByRole("button", { name: "Update" }));

    await waitFor(() => expect(initiateUpdate).toHaveBeenCalledWith("runtime-1", SERVER_VERSION));
  });

  it("falls back to the commit as the target when the server has no version string", async () => {
    initiateUpdate.mockResolvedValue({ id: "upd-1" });
    getUpdateResult.mockResolvedValue({ status: "running" });
    renderSection({
      runtimeId: "runtime-1",
      updateStatus: { ...UPDATE, serverVersion: "" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Update" }));

    await waitFor(() => expect(initiateUpdate).toHaveBeenCalledWith("runtime-1", SERVER_COMMIT));
  });
});

describe("UpdateSection read-only and managed states", () => {
  it("shows Latest without a redundant read-only label or update action", () => {
    renderSection({ runtimeId: null });

    expect(screen.getByText("Latest")).toBeInTheDocument();
    expect(screen.queryByText("Read-only")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Update" })).not.toBeInTheDocument();
  });

  it("shows the Desktop manager without exposing an update action, even when the server offers one", () => {
    renderSection({ runtimeId: "runtime-1", launchedBy: "desktop", updateStatus: UPDATE });

    expect(screen.getByText("Managed by Desktop")).toBeInTheDocument();
    expect(screen.queryByText("available")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Update" })).not.toBeInTheDocument();
  });

  it("shows an available version without an action for a read-only viewer", () => {
    renderSection({ runtimeId: null, updateStatus: UPDATE });

    expect(screen.getByText("available")).toBeInTheDocument();
    expect(screen.getByText("Read-only")).toBeInTheDocument();
    expect(
      screen.getByTitle("Only runtime owners and workspace admins can update the CLI."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Update" })).not.toBeInTheDocument();
  });
});

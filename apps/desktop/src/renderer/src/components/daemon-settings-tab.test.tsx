import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import type { DaemonStatus } from "../../../shared/daemon-types";

vi.mock("@multica/views/i18n", () => ({
  useT: () => ({
    t: (
      selector: (resources: unknown) => string,
      values?: Record<string, string>,
    ) => {
      const proxy: unknown = new Proxy(
        {},
        {
          get: (_target, key) =>
            key === "cli_download_progress" ? "Downloading {{done}} / {{total}} MB" : proxy,
        },
      );
      const picked = selector(proxy);
      const label = typeof picked === "string" ? picked : "label";
      return Object.entries(values ?? {}).reduce(
        (result, [key, value]) => result.replace(`{{${key}}}`, value),
        label,
      );
    },
  }),
}));

vi.mock("@multica/views/settings", () => ({
  SettingsTab: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  SettingsCard: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  SettingsSection: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  SettingsRow: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("../platform/daemon-reauth", () => ({ reauthenticateDaemon: vi.fn() }));

import { DaemonSettingsTab } from "./daemon-settings-tab";

function mountWith(status: DaemonStatus) {
  Object.defineProperty(window, "daemonAPI", {
    configurable: true,
    value: {
      getPrefs: vi.fn().mockResolvedValue({ autoStart: true, autoStop: false }),
      isCliInstalled: vi.fn().mockResolvedValue(true),
      getStatus: vi.fn().mockResolvedValue(status),
      onStatusChange: vi.fn(() => () => {}),
    },
  });
  return render(<DaemonSettingsTab />);
}

describe("DaemonSettingsTab CLI download progress", () => {
  beforeEach(() => {
    Object.defineProperty(window, "desktopAPI", {
      configurable: true,
      value: { openExternal: vi.fn() },
    });
  });

  it("shows a progress bar with MB done and total while the CLI downloads", async () => {
    mountWith({
      state: "installing_cli",
      cliDownload: { bytesDone: 5 * 1024 * 1024, bytesTotal: 20 * 1024 * 1024 },
    });

    const bar = await screen.findByRole("progressbar");
    expect(bar).toHaveAttribute("aria-valuenow", "25");
    expect(screen.getByText("Downloading 5.0 / 20.0 MB")).toBeInTheDocument();
  });

  it("shows no bar without cliDownload", async () => {
    mountWith({ state: "installing_cli" });

    await waitFor(() => expect(window.daemonAPI.getStatus).toHaveBeenCalled());
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();
  });
});

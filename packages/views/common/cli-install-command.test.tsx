import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { configStore } from "@multica/core/config";
import { CliInstallCommand } from "./cli-install-command";

const apiMock = vi.hoisted(() => ({ baseUrl: "" as string | null }));
vi.mock("@multica/core/api", () => ({
  api: {
    getBaseUrl: () => {
      if (apiMock.baseUrl === null) throw new Error("ApiClient not initialised");
      return apiMock.baseUrl;
    },
  },
}));

const LABELS = {
  group: "Choose your platform",
  macosLinux: "macOS / Linux",
};

function renderSwitch() {
  return render(
    <CliInstallCommand labels={LABELS}>
      {(command) => <code>{command}</code>}
    </CliInstallCommand>,
  );
}

describe("CliInstallCommand", () => {
  beforeEach(() => {
    apiMock.baseUrl = "";
    configStore.setState({ daemonServerUrl: "" });
  });
  afterEach(cleanup);

  it("installs from the configured server, not from upstream", () => {
    configStore.setState({ daemonServerUrl: "https://multica.example.com/" });
    renderSwitch();

    expect(
      screen.getByRole("tablist", { name: LABELS.group }),
    ).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "macOS / Linux" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(
      screen.getByText(
        "curl -fsSL https://multica.example.com/api/daemon/install.sh | bash",
      ),
    ).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent("githubusercontent");
    expect(document.body).not.toHaveTextContent("multica-ai");
  });

  it("falls back to the API base URL when the server sets none", () => {
    apiMock.baseUrl = "https://api.example.com";
    renderSwitch();

    expect(
      screen.getByText(
        "curl -fsSL https://api.example.com/api/daemon/install.sh | bash",
      ),
    ).toBeInTheDocument();
  });

  it("falls back to the page origin when no API client exists", () => {
    apiMock.baseUrl = null;
    renderSwitch();

    expect(
      screen.getByText(
        `curl -fsSL ${window.location.origin}/api/daemon/install.sh | bash`,
      ),
    ).toBeInTheDocument();
  });

  // The server builds Linux and macOS only (CHE-2641).
  it("offers no Windows installer", () => {
    renderSwitch();

    expect(screen.getAllByRole("tab")).toHaveLength(1);
    expect(screen.queryByRole("tab", { name: /windows/i })).toBeNull();
    expect(document.body).not.toHaveTextContent("irm ");
  });
});

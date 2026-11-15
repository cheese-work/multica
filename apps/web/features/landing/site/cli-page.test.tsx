import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { CliPage } from "./cli-page";

// The header, footer and demo scenes need the auth store and locale provider;
// this test is about the install instructions, so render only the page body.
vi.mock("../i18n", () => ({
  useLocale: () => ({ locale: "en" }),
  docsHrefForLocale: () => "/docs",
}));
vi.mock("./site-page", () => ({
  SitePage: ({ children }: { children: ReactNode }) => <main>{children}</main>,
  CtaBand: () => null,
}));
vi.mock("./demo/cli-visuals", () => ({
  CliScene: () => null,
  AgentSessionVisual: () => null,
}));

describe("CliPage install instructions", () => {
  it("installs the CLI from this server, not from upstream", () => {
    const { container } = render(<CliPage />);

    expect(
      screen.getByText(
        `curl -fsSL ${window.location.origin}/api/daemon/install.sh | bash`,
      ),
    ).toBeInTheDocument();
    expect(container.textContent).not.toMatch(
      /multica-ai|raw\.githubusercontent\.com|brew install|install\.ps1/,
    );
  });
});

import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { LocaleProvider } from "../i18n";
import { CliPage } from "./cli-page";

vi.mock("next/navigation", () => ({
  usePathname: () => "/product/cli",
  useRouter: () => ({ push: vi.fn() }),
}));

// Header and footer need the auth store; the page body is what is under test.
vi.mock("./site-page", () => ({
  SitePage: ({ children }: { children: React.ReactNode }) => <main>{children}</main>,
  CtaBand: () => null,
}));

const renderPage = () =>
  render(
    <LocaleProvider>
      <CliPage />
    </LocaleProvider>,
  );

// The fork's CLI comes from the fork's own server (CHE-2641), so the install
// step must show that server's installer, never upstream's.
describe("CliPage install step", () => {
  it("installs the CLI from this server's installer", async () => {
    renderPage();

    expect(
      await screen.findByText(
        `curl -fsSL ${window.location.origin}/api/daemon/install.sh | bash`,
      ),
    ).toBeInTheDocument();
  });

  it("offers no upstream install route", () => {
    const { container } = renderPage();

    expect(container.textContent).not.toMatch(/multica-ai\/multica\/main\/scripts/);
    expect(screen.queryByRole("button", { name: "Homebrew" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Windows" })).not.toBeInTheDocument();
  });
});

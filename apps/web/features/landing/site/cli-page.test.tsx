import { render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { CliPage } from "./cli-page";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }), usePathname: () => "/" }));
vi.mock("../i18n", () => ({ useLocale: () => ({ locale: "en" }), docsHrefForLocale: () => "/docs" }));
vi.mock("./site-page", () => ({
  SitePage: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  CtaBand: () => null,
}));
vi.mock("./demo/cli-visuals", () => ({ AgentSessionVisual: () => null, CliScene: () => null }));
vi.mock("./surface-kit", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./surface-kit")>()),
  SurfaceHero: () => null,
}));

describe("CLI page install step", () => {
  it("installs from the fork server and names no upstream installer", () => {
    const { container } = render(<CliPage />);
    const text = container.textContent ?? "";
    expect(text).toContain("/api/daemon/install.sh");
    expect(text).not.toMatch(/multica-ai|brew install|install\.ps1/);
  });
});

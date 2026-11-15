import { describe, expect, it } from "vitest";
import { installSteps } from "./cli-page";

describe("CLI page install step", () => {
  const [install] = installSteps("https://fork.example/api/daemon/install.sh");

  it("shows only the fork server install command", () => {
    expect(install!.code).toEqual([{ label: "macOS / Linux", code: "https://fork.example/api/daemon/install.sh" }]);
  });

  it("names no upstream installer", () => {
    expect(JSON.stringify(install)).not.toMatch(/multica-ai|brew|install\.ps1|Windows/i);
  });
});

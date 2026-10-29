import { describe, expect, it, vi } from "vitest";

// Views suites mock react-query with only the exports they use; importing the
// hook module must not touch any other export.
vi.mock("@tanstack/react-query", () => ({ useQueryClient: () => ({}) }));

describe("mermaid-orientation module", () => {
  it("imports against a react-query mock that lacks QueryClient", async () => {
    const mod = await import("./mermaid-orientation");
    expect(mod.readMermaidOrientationSetting(undefined)).toBe("auto");
  });
});

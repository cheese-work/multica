import { afterEach, describe, expect, it, vi } from "vitest";
import { instanceOrigin } from "./instance-origin";

const TAILSCALE = "https://congvc-c00.taila6fa8a.ts.net:19443";

afterEach(() => vi.unstubAllGlobals());

describe("instanceOrigin", () => {
  it("uses the origin the visitor is on over the configured app URL", () => {
    vi.stubGlobal("location", {
      protocol: "https:",
      origin: "https://multica.congvc.dev",
    });
    expect(instanceOrigin(TAILSCALE)).toBe("https://multica.congvc.dev");
  });

  it("falls back to the app URL on a non-http origin (desktop shell)", () => {
    vi.stubGlobal("location", { protocol: "file:", origin: "null" });
    expect(instanceOrigin(`${TAILSCALE}/`)).toBe(TAILSCALE);
  });

  it("falls back to the app URL when there is no page origin", () => {
    expect(instanceOrigin(TAILSCALE)).toBe(TAILSCALE);
  });

  it("is empty when nothing is known", () => {
    expect(instanceOrigin(undefined)).toBe("");
    expect(instanceOrigin("  ")).toBe("");
  });
});

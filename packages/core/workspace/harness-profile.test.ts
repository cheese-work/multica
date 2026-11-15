// @vitest-environment node
import { describe, expect, it } from "vitest";
import { readHarnessProfile, resolveHarnessProfile } from "./harness-profile";

describe("readHarnessProfile", () => {
  it("defaults to personal when nothing is saved", () => {
    expect(readHarnessProfile(undefined)).toBe("personal");
    expect(readHarnessProfile({})).toBe("personal");
  });

  it("reads a saved managed value", () => {
    expect(readHarnessProfile({ harness_profile: "managed" })).toBe("managed");
  });

  it.each([["managed_sandbox"], [""], [7], [null], [true], [["managed"]]])(
    "falls back to personal for %j",
    (value) => {
      expect(readHarnessProfile({ harness_profile: value })).toBe("personal");
    },
  );
});

describe("resolveHarnessProfile", () => {
  it("agent override beats the workspace default", () => {
    expect(resolveHarnessProfile({ harness_profile: "managed" }, { harness_profile: "personal" })).toBe("personal");
    expect(resolveHarnessProfile({}, { harness_profile: "managed" })).toBe("managed");
  });

  it("inherits the workspace default without an override", () => {
    expect(resolveHarnessProfile({ harness_profile: "managed" }, {})).toBe("managed");
    expect(resolveHarnessProfile({ harness_profile: "managed" }, undefined)).toBe("managed");
  });

  it("ignores a null or malformed override and falls back to personal", () => {
    expect(resolveHarnessProfile({ harness_profile: "managed" }, { harness_profile: null })).toBe("managed");
    expect(resolveHarnessProfile({}, { harness_profile: "bogus" })).toBe("personal");
    expect(resolveHarnessProfile(undefined, undefined)).toBe("personal");
  });
});

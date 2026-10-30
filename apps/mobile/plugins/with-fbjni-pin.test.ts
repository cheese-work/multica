// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";
import { describe, expect, it } from "vitest";
import { FBJNI_VERSION, pinFbjni } from "./with-fbjni-pin.js";

const appGradle = `android {\n    namespace 'ai.multica.mobile.dev'\n}\n`;

describe("fbjni pin", () => {
  it("forces fbjni to the pinned version on every configuration", () => {
    const out = pinFbjni(appGradle);
    expect(out).toContain(
      `force 'com.facebook.fbjni:fbjni:${FBJNI_VERSION}'`,
    );
    expect(out.startsWith(appGradle)).toBe(true);
  });

  it("is idempotent across repeated prebuilds", () => {
    const once = pinFbjni(appGradle);
    expect(pinFbjni(once)).toBe(once);
  });

  // react-android ships the libc++_shared.so built for its own fbjni. A newer
  // libfbjni.so (pulled in by AndroidMath) needs a newer libc++ and crashes at
  // load (CHE-2592), so the pin must follow React Native's catalog version.
  it("matches the fbjni version React Native was built with", () => {
    const requireFromTest = createRequire(import.meta.url);
    const rnRoot = dirname(requireFromTest.resolve("react-native/package.json"));
    const catalog = readFileSync(
      join(rnRoot, "gradle", "libs.versions.toml"),
      "utf8",
    );
    expect(catalog).toMatch(new RegExp(`^fbjni = "${FBJNI_VERSION}"$`, "m"));
  });
});

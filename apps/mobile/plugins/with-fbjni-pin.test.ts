// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";
import { describe, expect, it } from "vitest";
import type { ConfigContext } from "expo/config";
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

// Behavior: the real app config must put the pin into the generated Android
// app/build.gradle. Run the config's own Gradle mod chain on a sample file.
describe("app config applies the fbjni pin", () => {
  it("adds the force rule when the app build.gradle mod runs", async () => {
    const { default: createConfig } = await import("../app.config");
    const config = createConfig({ config: {} } as ConfigContext);
    const mods = (config as { mods?: { android?: Record<string, (c: never) => Promise<{ modResults: { contents: string } }>> } }).mods;
    const mod = mods?.android?.appBuildGradle;
    expect(mod, "app config registers no appBuildGradle mod").toBeTypeOf("function");

    const result = await mod!({
      ...config,
      modResults: { language: "groovy", contents: appGradle },
      modRequest: { projectRoot: "", platformProjectRoot: "", modName: "appBuildGradle", platform: "android", introspect: true },
      modRawConfig: config,
    } as never);

    expect(result.modResults.contents).toContain(
      `force 'com.facebook.fbjni:fbjni:${FBJNI_VERSION}'`,
    );
  });
});

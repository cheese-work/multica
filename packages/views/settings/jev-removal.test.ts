// @vitest-environment node
import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { ApiClient } from "@multica/core/api";

// CHE-2761 (MJ removal slice C2b): the Settings "Jev / Governance" tab, its
// core API surface and its locale blocks are gone. Guards against a revert or
// a merge that brings any of them back.
const HERE = dirname(fileURLToPath(import.meta.url));
const VIEWS = resolve(HERE, "..");
const LOCALES = ["en", "fr", "ja", "ko", "zh-Hans", "zh-Hant"];

describe("Jev / Governance settings removal", () => {
  it("settings page has no jev-governance tab", () => {
    const page = readFileSync(resolve(HERE, "components/settings-page.tsx"), "utf8");
    expect(page).not.toMatch(/jev-governance|JevGovernanceTab/);
    const search = readFileSync(resolve(HERE, "components/use-settings-search-index.ts"), "utf8");
    expect(search).not.toMatch(/jev-governance/);
  });

  it("tab components are deleted", () => {
    expect(existsSync(resolve(HERE, "components/jev-governance-tab.tsx"))).toBe(false);
    expect(existsSync(resolve(HERE, "components/use-change-format.ts"))).toBe(false);
  });

  it.each(LOCALES)("%s settings locale has no jev_governance block", (locale) => {
    const settings = JSON.parse(
      readFileSync(resolve(VIEWS, "locales", locale, "settings.json"), "utf8"),
    );
    expect(settings.jev_governance).toBeUndefined();
    expect(settings.page.tabs.jev_governance).toBeUndefined();
  });

  it("api client has no governance or jev credential methods", () => {
    const proto = ApiClient.prototype as unknown as Record<string, unknown>;
    for (const name of [
      "getGovernanceConfig",
      "patchGovernanceConfig",
      "getJevCredential",
      "putJevCredential",
      "deleteJevCredential",
    ]) {
      expect(proto[name]).toBeUndefined();
    }
  });
});

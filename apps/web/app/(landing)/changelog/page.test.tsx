import { readFileSync } from "node:fs";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import { createLandingDict } from "@/features/landing/i18n/dictionary";

const localeState = vi.hoisted(() => ({ locale: "en" as "en" | "ja" }));
vi.mock("node:fs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("node:fs")>();
  const readFileSync = vi.fn();
  return { ...actual, readFileSync, default: { ...actual, readFileSync } };
});
vi.mock("@/features/landing/components/landing-header", () => ({ LandingHeader: () => null }));
vi.mock("@/features/landing/components/landing-footer", () => ({ LandingFooter: () => null }));
vi.mock("@/features/landing/i18n", () => ({ useLocale: () => ({ locale: localeState.locale, t: createLandingDict(localeState.locale, true) }) }));

afterEach(() => { vi.unstubAllEnvs(); vi.resetModules(); localeState.locale = "en"; });

async function renderPage() {
  const { default: ChangelogPage } = await import("./page");
  return renderToStaticMarkup(ChangelogPage());
}

describe("build-time changelog page seam", () => {
  it("keeps upstream and explains an unavailable file", async () => {
    vi.mocked(readFileSync).mockImplementation(() => { throw new Error("ENOENT"); });
    const html = await renderPage();
    expect(html).toContain("Fork entries unavailable for this build");
    expect(html).toContain("v0.6.1");
    expect(html).toContain("dev");
    expect(readFileSync).toHaveBeenCalledWith(expect.stringContaining(".fork-changelog/entries.json"), "utf8");
  });

  it("explains an empty file and rejects corrupt data", async () => {
    vi.mocked(readFileSync).mockReturnValue('{"entries":[]}');
    expect(await renderPage()).toContain("No fork entries yet");
    vi.resetModules();
    vi.mocked(readFileSync).mockReturnValue("invalid JSON");
    expect(await renderPage()).toContain("Fork entries unavailable for this build");
  });

  it("shows English fork categories, duplicate notes and build stamp in a non-English locale", async () => {
    localeState.locale = "ja";
    vi.stubEnv("NEXT_PUBLIC_APP_VERSION", "0.6.3");
    vi.stubEnv("NEXT_PUBLIC_APP_BUILD", "20261014-0200");
    vi.stubEnv("NEXT_PUBLIC_APP_COMMIT", "98fea199475b");
    vi.stubEnv("NEXT_PUBLIC_BUILD_DATE", "2026-10-13T19:00:00Z");
    vi.mocked(readFileSync).mockReturnValue(JSON.stringify({ entries: [1, 2].map((number) => ({
      id: `fork-pr-${number}`, number, date: "2026-10-14", mergedAt: "2026-10-13T19:00:00Z",
      section: "New Features", text: "Duplicate sentence.",
    })) }));
    const html = await renderPage();
    expect(html).toContain("New Features");
    expect(html).toContain("98fea19 · 2026-10-14 02:00 ICT");
    expect(html.match(/Duplicate sentence\./g)).toHaveLength(2);
    expect(html).toContain("release-fork-2026-10-14");
    expect(html).not.toContain("vfork-");
    expect(html).not.toContain("Fork entries unavailable");
    expect(html).toContain(createLandingDict("ja", true).changelog.entries[0]!.title);
  });

  it("takes a file snapshot at module load, not on each page request", async () => {
    vi.mocked(readFileSync).mockReturnValue('{"entries":[]}');
    expect(await renderPage()).toContain("No fork entries yet");
    vi.mocked(readFileSync).mockImplementation(() => { throw new Error("must not read again"); });
    expect(await renderPage()).toContain("No fork entries yet");
  });
});

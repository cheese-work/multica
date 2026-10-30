// @vitest-environment node
import { describe, expect, it } from "vitest";
import { forkBuildStamp, mergeChangelogReleases, parseForkChangelog, releaseVersionLabel } from "./fork-changelog";

const entry = (number: number, mergedAt = "2026-10-13T18:00:00Z") => ({
  id: `fork-pr-${number}`, number, mergedAt, date: "2026-10-14",
  section: "New Features", text: "Changes now show in the changelog.",
});

describe("fork changelog page data", () => {
  it("distinguishes unavailable, empty and populated entries", () => {
    expect(parseForkChangelog("broken").status).toBe("unavailable");
    expect(parseForkChangelog('{"entries":[]}').status).toBe("empty");
    expect(parseForkChangelog(JSON.stringify({ entries: [entry(1)] })).status).toBe("populated");
    for (const entries of [[null], [{ ...entry(1), date: "2026-10-13" }], [entry(1), entry(1)], [{ ...entry(1), section: "__proto__" }]]) {
      expect(parseForkChangelog(JSON.stringify({ entries })).status).toBe("unavailable");
    }
  });

  it("groups ICT days, sorts by merge time then PR number, and keeps duplicate text with stable keys", () => {
    const fork = parseForkChangelog(JSON.stringify({ entries: [entry(1), entry(2), entry(3, "2026-10-13T19:00:00Z")] }));
    expect(fork.releases).toHaveLength(1);
    const release = fork.releases[0]!;
    expect(release.version).toBe("fork-2026-10-14");
    expect(releaseVersionLabel(release)).toBe("Fork");
    expect(release.features).toEqual(Array(3).fill(entry(1).text));
    expect(release.changeKeys?.features).toEqual(["fork-pr-3", "fork-pr-2", "fork-pr-1"]);
    const edited = parseForkChangelog(JSON.stringify({ entries: [{ ...entry(1), text: "Edited sentence." }] }));
    expect(edited.releases[0]?.version).toBe(release.version);
    expect(edited.releases[0]?.changeKeys?.features).toEqual(["fork-pr-1"]);
  });

  it("merges upstream and fork releases on the same day without changing upstream", () => {
    const upstream = [
      { version: "0.6.1", date: "2026-10-14", title: "Upstream", changes: ["Existing note."] },
      { version: "0.6.0", date: "2026-10-01", title: "Older upstream", changes: [] },
    ];
    const snapshot = JSON.stringify(upstream);
    const fork = parseForkChangelog(JSON.stringify({ entries: [entry(1)] }));
    expect(mergeChangelogReleases(upstream, fork).map((release) => release.version)).toEqual(["fork-2026-10-14", "0.6.1", "0.6.0"]);
    expect(JSON.stringify(upstream)).toBe(snapshot);
  });

  it("shows a seven-character SHA and commit build date in ICT, or dev locally", () => {
    expect(forkBuildStamp()).toBe("dev");
    expect(forkBuildStamp("dev", "2026-10-13T19:00:00Z")).toBe("dev");
    expect(forkBuildStamp("sha-98fea199475b", "2026-10-13T19:00:00Z")).toBe("98fea19 · 2026-10-14 02:00 ICT");
  });

  it("groups backfilled entries by ISO week, dated on the Monday", () => {
    const old = (number: number, mergedAt: string, date: string) => ({ ...entry(number, mergedAt), date, group: "week" });
    const fork = parseForkChangelog(JSON.stringify({ entries: [
      old(1, "2026-05-31T18:00:00Z", "2026-06-01"), // Mon 1 Jun ICT, ISO week 23
      old(2, "2026-06-04T05:00:00Z", "2026-06-04"), // Thu, same week
      old(3, "2026-05-31T05:00:00Z", "2026-05-31"), // Sun, previous week 22
      entry(4),
    ] }));
    expect(fork.releases.map((r) => [r.version, r.date])).toEqual([
      ["fork-2026-10-14", "2026-10-14"],
      ["fork-2026-W23", "2026-06-01"],
      ["fork-2026-W22", "2026-05-25"],
    ]);
    expect(fork.releases[1]?.changeKeys?.features).toEqual(["fork-pr-2", "fork-pr-1"]);
    expect(releaseVersionLabel(fork.releases[1]!)).toBe("Fork");
  });
});

import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { DeploySections } from "./fork-deploys";
import { liveVersion, parseForkChangelog } from "./fork-changelog";

const entry = (number: number, deploy: string, mergedAt = "2026-11-05T01:00:00Z") => ({
  id: `fork-pr-${number}`, number, mergedAt, date: "2026-11-05", section: "Bug Fixes", text: `Fix ${number}.`, deploy,
});
const deploy = (sha: string, version: string, deployedAt: string, hiddenCount = 0, live = false) => ({ sha, version, deployedAt, hiddenCount, ...(live ? { live: true } : {}) });
const doc = (entries: unknown[], deploys: unknown) => JSON.stringify({ entries, deploys });
const three = [
  deploy("c", "0.7.2-20261105-1426", "2026-11-05T07:26:00Z", 0, true),
  deploy("b", "0.7.2-20261105-1100", "2026-11-05T04:00:00Z", 2),
  deploy("a", "0.7.2-20261105-0800", "2026-11-05T01:00:00Z", 0),
  deploy("z", "0.7.2-20261104-0800", "2026-11-04T01:00:00Z", 0),
];

describe("fork changelog deploys parser", () => {
  it("makes one section per deploy, newest first, grouped by the ICT day of the deploy", () => {
    const fork = parseForkChangelog(doc([entry(1, "live"), entry(2, "a", "2026-11-04T17:30:00Z")], three));
    expect(fork.status).toBe("populated");
    expect(fork.releases.map((r) => r.version)).toEqual(["fork-2026-11-05", "fork-2026-11-04"]);
    const day = fork.releases[0]!;
    expect(day.deploys!.map((d) => [d.version, d.rank, d.live])).toEqual([
      ["0.7.2-20261105-1426", 0, true], ["0.7.2-20261105-1100", 1, false], ["0.7.2-20261105-0800", 2, false],
    ]);
    // entry 2 merged on the previous ICT day but belongs to deploy a
    expect(day.deploys![2]!.fixes).toEqual(["Fix 2."]);
    expect(day.deploys![0]!.fixes).toEqual(["Fix 1."]);
    expect(day.deploys![1]!.hiddenCount).toBe(2);
    expect(fork.releases[1]!.deploys![0]!.rank).toBe(3);
  });

  it("falls back to day grouping when deploys is absent or malformed, never unavailable", () => {
    for (const deploys of [undefined, "x", [{}], [deploy("a", "v", "bad", 0, true)], [deploy("a", "v", "2026-11-05T01:00:00Z")], [three[0], three[0]]]) {
      const fork = parseForkChangelog(doc([entry(1, "live")], deploys));
      expect(fork.status).toBe("populated");
      expect(fork.releases[0]!.deploys).toBeUndefined();
      expect(fork.releases[0]!.fixes).toEqual(["Fix 1."]);
    }
    const unknown = parseForkChangelog(doc([entry(1, "nope")], three));
    expect(unknown.status).toBe("populated");
    expect(unknown.releases[0]!.deploys).toBeUndefined();
  });

  it("liveVersion is the Live divider's version, undefined without valid deploys", () => {
    expect(liveVersion(parseForkChangelog(doc([entry(1, "live")], three)))).toBe("0.7.2-20261105-1426");
    expect(liveVersion(parseForkChangelog(doc([entry(1, "live")], undefined)))).toBeUndefined();
  });

  it("liveVersion stays the Live divider's version when a previous deploy lands on a later ICT day", () => {
    const later = [deploy("c", "live-v", "2026-11-05T07:26:00Z", 0, true), deploy("b", "prev-v", "2026-11-05T18:00:00Z")];
    expect(liveVersion(parseForkChangelog(doc([entry(1, "live")], later)))).toBe("live-v");
  });

  it("still rejects an invalid entry", () => {
    expect(parseForkChangelog(doc([{ ...entry(1, "live"), date: "2000-01-01" }], three)).status).toBe("unavailable");
  });

  it("keeps week-grouped backfill entries as they are", () => {
    const old = { ...entry(9, "live"), deploy: undefined, group: "week", mergedAt: "2026-06-04T05:00:00Z", date: "2026-06-04" };
    const fork = parseForkChangelog(doc([entry(1, "live"), old], three));
    expect(fork.releases.map((r) => r.version)).toEqual(["fork-2026-11-05", "fork-2026-11-04", "fork-2026-W23"]);
  });
});

describe("DeploySections", () => {
  const html = (entries: unknown[], deploys: unknown) => {
    const fork = parseForkChangelog(doc(entries, deploys));
    return renderToStaticMarkup(<DeploySections release={fork.releases[0]!} />);
  };

  it("renders Live, Previous and light levels with chips and labelled dividers", () => {
    const out = html([entry(1, "live"), entry(2, "b"), entry(3, "a")], three);
    expect(out).toContain("0.7.2-20261105-1426");
    expect(out).toContain("· Live");
    expect(out).toContain("0.7.2-20261105-1100 · 2026-11-05 11:00 ICT");
    expect(out).toMatch(/data-heat="live"[\s\S]*data-heat="previous"[\s\S]*data-heat="earlier"/);
    expect(out).toContain(">Live<");
    expect(out).toContain(">Previous<");
    expect(out).toContain("Fix 1.");
  });

  it("shows the count for a deploy with no visible entry, and says so when it is 0", () => {
    const out = html([entry(1, "live")], three);
    expect(out).toContain("2 changes without notes");
    expect(out).toContain("No user-facing changes");
    const one = html([entry(1, "live")], [three[0], deploy("b", "v-b", "2026-11-05T04:00:00Z", 1)]);
    expect(one).toContain("1 change without notes");
  });
});

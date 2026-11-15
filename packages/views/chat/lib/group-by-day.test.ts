// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { ChatSession } from "@multica/core/types";
import { groupByDay } from "./group-by-day";

// Friday 13 Nov 2026 15:30 in Asia/Ho_Chi_Minh (UTC+7, no DST).
const NOW = new Date("2026-11-13T08:30:00Z");
const TZ = "Asia/Ho_Chi_Minh";

function at(id: string, iso: string): ChatSession {
  return { id, created_at: iso, updated_at: iso, last_message: null } as unknown as ChatSession;
}

function keysOf(groups: ReturnType<typeof groupByDay>) {
  return Object.fromEntries(groups.map((g) => [g.key, g.sessions.map((s) => s.id)]));
}

describe("groupByDay", () => {
  it("returns the four groups in order, empty ones included", () => {
    const groups = groupByDay([], NOW, TZ);
    expect(groups.map((g) => g.key)).toEqual(["today", "yesterday", "thisWeek", "older"]);
    expect(groups.every((g) => g.sessions.length === 0)).toBe(true);
  });

  it("cuts days at local midnight", () => {
    const got = keysOf(
      groupByDay(
        [
          at("today-midnight", "2026-11-12T17:00:00Z"), // 13 Nov 00:00 local
          at("yesterday-last-second", "2026-11-12T16:59:59Z"), // 12 Nov 23:59:59 local
          at("yesterday-midnight", "2026-11-11T17:00:00Z"), // 12 Nov 00:00 local
        ],
        NOW,
        TZ,
      ),
    );
    expect(got.today).toEqual(["today-midnight"]);
    expect(got.yesterday).toEqual(["yesterday-last-second", "yesterday-midnight"]);
  });

  it("starts 'this week' on the ISO Monday, before that is older", () => {
    const got = keysOf(
      groupByDay(
        [
          at("wed-last-second", "2026-11-11T16:59:59Z"), // 11 Nov 23:59:59 local
          at("monday-midnight", "2026-11-08T17:00:00Z"), // Mon 9 Nov 00:00 local
          at("sunday-last-second", "2026-11-08T16:59:59Z"), // Sun 8 Nov 23:59:59 local
        ],
        NOW,
        TZ,
      ),
    );
    expect(got.thisWeek).toEqual(["wed-last-second", "monday-midnight"]);
    expect(got.older).toEqual(["sunday-last-second"]);
  });

  it("on a Monday only today and yesterday (Sunday) exist before older", () => {
    const monday = new Date("2026-11-09T08:30:00Z");
    const got = keysOf(
      groupByDay(
        [at("mon", "2026-11-09T01:00:00Z"), at("sun", "2026-11-08T10:00:00Z"), at("sat", "2026-11-07T10:00:00Z")],
        monday,
        TZ,
      ),
    );
    expect(got).toEqual({ today: ["mon"], yesterday: ["sun"], thisWeek: [], older: ["sat"] });
  });

  it("uses the given time zone, not UTC", () => {
    // 23:00Z on 12 Nov is 13 Nov 06:00 in Ho Chi Minh but still 12 Nov in UTC.
    const s = [at("x", "2026-11-12T23:00:00Z")];
    expect(keysOf(groupByDay(s, NOW, TZ)).today).toEqual(["x"]);
    expect(keysOf(groupByDay(s, NOW, "UTC")).yesterday).toEqual(["x"]);
  });

  it("keeps input order within a group and files a future time under today", () => {
    const got = keysOf(
      groupByDay([at("b", "2026-11-13T07:00:00Z"), at("a", "2026-11-13T06:00:00Z"), at("future", "2026-11-20T00:00:00Z")], NOW, TZ),
    );
    expect(got.today).toEqual(["b", "a", "future"]);
  });
});

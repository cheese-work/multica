import { describe, expect, it } from "vitest";
import { formatCompactionTime } from "./compaction-time";

describe("formatCompactionTime", () => {
  // CHE-2169 M1: the divider prints ICT (UTC+7) wall time whatever the
  // device zone, so the web and mobile dividers agree.
  it("prints HH:MM in Asia/Bangkok", () => {
    expect(formatCompactionTime("2026-10-28T02:18:00Z")).toBe("09:18");
  });

  it("rolls over midnight", () => {
    expect(formatCompactionTime("2026-10-28T20:05:00Z")).toBe("03:05");
  });

  it("returns null for an unparseable timestamp", () => {
    expect(formatCompactionTime("not-a-date")).toBeNull();
    expect(formatCompactionTime("")).toBeNull();
  });
});

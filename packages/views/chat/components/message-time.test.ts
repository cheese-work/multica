import { describe, expect, it } from "vitest";
import { formatDayLabel, formatMessageDateTime, formatMessageTime, formatTimeWithZone } from "./message-time";

// 2026-10-20T04:20:00Z: 11:20 in Ho Chi Minh (UTC+7), 00:20 in New York (EDT, UTC-4).
const AT = "2026-10-20T04:20:00Z";

describe("formatMessageTime", () => {
  it("renders the same instant in each reader's own locale and zone", () => {
    expect(formatMessageTime(AT, "vi-VN", "Asia/Ho_Chi_Minh")).toBe("11:20");
    expect(formatMessageTime(AT, "en-US", "America/New_York")).toMatch(/^12:20\sAM$/);
  });

  it("returns the raw string for an unparseable timestamp", () => {
    expect(formatMessageTime("not-a-date", "en-US", "UTC")).toBe("not-a-date");
  });

  it("falls back to local time for a zone the runtime does not know", () => {
    expect(formatMessageTime(AT, "en-US", "Mars/Olympus")).toMatch(/\d{1,2}:\d{2}/);
  });
});

describe("formatMessageDateTime", () => {
  it("includes the date as well as the time", () => {
    expect(formatMessageDateTime(AT, "en-US", "America/New_York")).toMatch(/Oct 20, 2026/);
  });
});

describe("formatDayLabel", () => {
  const now = new Date("2026-10-20T04:30:00Z");
  it("names today and yesterday in the reader's locale", () => {
    expect(formatDayLabel("2026-10-20T01:00:00Z", now, "en-US", "Asia/Ho_Chi_Minh")).toBe("Today");
    expect(formatDayLabel("2026-10-19T08:00:00Z", now, "en-US", "Asia/Ho_Chi_Minh")).toBe("Yesterday");
  });

  it("judges the day in the reader's zone, not UTC", () => {
    // 19 Oct 20:00Z is 03:00 on the 20th in Ho Chi Minh but 16:00 on the 19th in New York.
    expect(formatDayLabel("2026-10-19T20:00:00Z", now, "en-US", "Asia/Ho_Chi_Minh")).toBe("Today");
    expect(formatDayLabel("2026-10-19T20:00:00Z", now, "en-US", "America/New_York")).toBe("Yesterday");
  });

  it("uses a locale date for older days", () => {
    expect(formatDayLabel("2026-10-01T04:20:00Z", now, "en-US", "UTC")).toBe("Oct 1, 2026");
  });
});

describe("formatTimeWithZone", () => {
  it("prints 24h time with the zone abbreviation, naming UTC+7 ICT", () => {
    expect(formatTimeWithZone(AT, "en-US", "Asia/Ho_Chi_Minh")).toBe("11:20 ICT");
    expect(formatTimeWithZone(AT, "vi-VN", "Asia/Bangkok")).toBe("11:20 ICT");
  });

  it("uses the runtime's own abbreviation for other zones", () => {
    expect(formatTimeWithZone(AT, "en-US", "America/New_York")).toBe("00:20 EDT");
    expect(formatTimeWithZone(AT, "en-US", "UTC")).toBe("04:20 UTC");
  });

  it("returns the raw string for an unparseable timestamp", () => {
    expect(formatTimeWithZone("not-a-date", "en-US", "UTC")).toBe("not-a-date");
  });
});

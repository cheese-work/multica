import { describe, expect, it } from "vitest";
import {
  anchorFromViewable,
  chatAnchorKey,
  isNearEnd,
  issueAnchorKey,
  loadReadingAnchor,
  parseReadingAnchor,
  recordReadingAnchor,
  restoreIndex,
  saveReadingAnchor,
} from "./reading-anchor";

const ids = ["a", "b", "c", "d", "e"];

describe("anchorFromViewable", () => {
  it("anchors on the top visible item", () => {
    expect(anchorFromViewable([{ index: 3 }, { index: 2 }], ids)).toEqual({
      itemId: "c",
      atLiveEnd: false,
    });
  });

  it("flags the live end when the last item is visible", () => {
    expect(anchorFromViewable([{ index: 3 }, { index: 4 }], ids)).toEqual({
      itemId: "d",
      atLiveEnd: true,
    });
  });

  it("returns null when nothing is viewable or indexes are missing", () => {
    expect(anchorFromViewable([], ids)).toBeNull();
    expect(anchorFromViewable([{ index: null }], ids)).toBeNull();
    expect(anchorFromViewable([{ index: 9 }], ids)).toBeNull();
  });

  it("skips a synthetic row id", () => {
    expect(anchorFromViewable([{ index: 1 }, { index: 2 }], ["a", "x", "c"], "x")).toEqual({
      itemId: "c",
      atLiveEnd: true,
    });
  });
});

describe("restoreIndex", () => {
  it("returns the anchor's index when it is loaded and not at the live end", () => {
    expect(restoreIndex({ itemId: "c", atLiveEnd: false }, ids)).toBe(2);
  });

  it("opens at the default position at the live end, with no anchor, or when not loaded", () => {
    expect(restoreIndex({ itemId: "c", atLiveEnd: true }, ids)).toBeUndefined();
    expect(restoreIndex(null, ids)).toBeUndefined();
    expect(restoreIndex({ itemId: "zzz", atLiveEnd: false }, ids)).toBeUndefined();
  });
});

describe("parseReadingAnchor", () => {
  it("accepts a well-formed anchor", () => {
    expect(parseReadingAnchor({ itemId: "a", atLiveEnd: false })).toEqual({
      itemId: "a",
      atLiveEnd: false,
    });
  });

  it.each([null, undefined, "x", 1, {}, { itemId: 1, atLiveEnd: false }, { itemId: "a" }, { itemId: "", atLiveEnd: false }])(
    "treats %j as nothing saved",
    (bad) => {
      expect(parseReadingAnchor(bad)).toBeNull();
    },
  );
});

describe("store", () => {
  it("keeps separate anchors per key", () => {
    saveReadingAnchor(chatAnchorKey("s1"), { itemId: "a", atLiveEnd: false });
    saveReadingAnchor(chatAnchorKey("s2"), { itemId: "b", atLiveEnd: false });
    saveReadingAnchor(issueAnchorKey("s1"), { itemId: "c", atLiveEnd: false });
    expect(loadReadingAnchor(chatAnchorKey("s1"))?.itemId).toBe("a");
    expect(loadReadingAnchor(chatAnchorKey("s2"))?.itemId).toBe("b");
    expect(loadReadingAnchor(issueAnchorKey("s1"))?.itemId).toBe("c");
    expect(loadReadingAnchor(chatAnchorKey("none"))).toBeNull();
  });
});

describe("live end uses scroll distance, not row visibility", () => {
  it("a partly visible last row far above the bottom edge is not the live end", () => {
    expect(anchorFromViewable([{ index: 3 }, { index: 4 }], ids, undefined, false)).toEqual({
      itemId: "d",
      atLiveEnd: false,
    });
  });

  it("isNearEnd compares the distance left to scroll against a small slack", () => {
    expect(isNearEnd(3000, 2100, 900)).toBe(true);
    expect(isNearEnd(3000, 2000, 900)).toBe(false);
    expect(isNearEnd(3000, 2000 - 1600, 900)).toBe(false);
  });
});

describe("recordReadingAnchor", () => {
  const key = chatAnchorKey("record");

  it("saves the top row", () => {
    recordReadingAnchor(key, [{ index: 1 }, { index: 2 }], ids, false);
    expect(loadReadingAnchor(key)).toEqual({ itemId: "b", atLiveEnd: false });
  });

  it("clears a stale anchor when the reader is back above every row", () => {
    recordReadingAnchor(key, [{ index: 1 }], ids, false);
    recordReadingAnchor(key, [], ids, false);
    expect(loadReadingAnchor(key)).toBeNull();
  });

  it("keeps the anchor while the list is still loading", () => {
    recordReadingAnchor(key, [{ index: 1 }], ids, false);
    recordReadingAnchor(key, [], [], false);
    expect(loadReadingAnchor(key)).toEqual({ itemId: "b", atLiveEnd: false });
  });
});

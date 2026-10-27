import { describe, expect, it } from "vitest";
import { SELF_MENTION_COLORS, selfMentionLinkVariants } from "./self-mention";

const VIEWER = "f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff";
const OTHER = "8f14e45f-ceea-4d0e-a1a2-9b1c0d3e4f5a";

function channel(c: number) {
  const v = c / 255;
  return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
}
function luminance(hex: string) {
  const [r, g, b] = [1, 3, 5].map((i) => channel(parseInt(hex.slice(i, i + 2), 16)));
  return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!;
}

describe("selfMentionLinkVariants", () => {
  it.each(["light", "dark"] as const)("styles only the viewer's member mention in %s", (scheme) => {
    const t = SELF_MENTION_COLORS[scheme];
    const variants = selfMentionLinkVariants(VIEWER, t)!;
    const [[pattern, style]] = Object.entries(variants);
    const re = new RegExp(pattern);

    expect(re.test(`mention://member/${VIEWER}`)).toBe(true);
    for (const url of [
      `mention://member/${OTHER}`,
      `mention://agent/${VIEWER}`,
      `mention://member/${VIEWER}x`,
      "mention://all/all",
      "https://example.com",
    ]) {
      expect(re.test(url)).toBe(false);
    }
    expect(style).toMatchObject({
      color: t.fg,
      backgroundColor: t.bg,
      underline: false,
    });
  });

  it("returns nothing without a viewer", () => {
    expect(selfMentionLinkVariants(undefined, SELF_MENTION_COLORS.light)).toBeUndefined();
  });

  it.each(["light", "dark"] as const)("keeps text/fill contrast >= 4.5:1 in %s", (scheme) => {
    const { bg, fg } = SELF_MENTION_COLORS[scheme];
    const [a, b] = [luminance(bg), luminance(fg)];
    expect((Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)).toBeGreaterThanOrEqual(4.5);
  });
});

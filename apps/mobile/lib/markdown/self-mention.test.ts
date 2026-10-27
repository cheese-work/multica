import { describe, expect, it } from "vitest";
import { selfMentionLinkVariants } from "./self-mention";

const VIEWER = "f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff";
const OTHER = "8f14e45f-ceea-4d0e-a1a2-9b1c0d3e4f5a";
const COLORS = { brand: "hsl(225 71% 58%)", background: "hsl(0 0% 100%)" };

/** Style the native renderer would pick for `url`, or undefined for the plain link style. */
function styleFor(url: string) {
  const variants = selfMentionLinkVariants(VIEWER, COLORS);
  const hit = Object.entries(variants).find(([pattern]) => new RegExp(pattern).test(url));
  return hit?.[1];
}

describe("selfMentionLinkVariants", () => {
  it("styles the viewer's member mention as a solid brand chip with background text", () => {
    expect(styleFor(`mention://member/${VIEWER}`)).toEqual({
      color: COLORS.background,
      backgroundColor: COLORS.brand,
      underline: false,
    });
  });

  it("styles @all like the viewer's own mention", () => {
    expect(styleFor("mention://all/all")).toEqual(styleFor(`mention://member/${VIEWER}`));
    expect(styleFor("mention://all/all")).toBeDefined();
  });

  it.each([
    `mention://member/${OTHER}`,
    `mention://agent/${VIEWER}`,
    `mention://issue/${VIEWER}`,
    `mention://member/${VIEWER}-x`,
    `https://example.com/mention://member/${VIEWER}`,
  ])("leaves %s as a plain link", (url) => {
    expect(styleFor(url)).toBeUndefined();
  });

  it("styles nothing without a viewer, not even @all", () => {
    expect(selfMentionLinkVariants(null, COLORS)).toEqual({});
    expect(selfMentionLinkVariants(undefined, COLORS)).toEqual({});
  });
});

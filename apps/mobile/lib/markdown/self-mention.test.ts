import { describe, expect, it } from "vitest";
import { emphasizeSelfMentions, selfMentionLinkVariants } from "./self-mention";

const VIEWER = "f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff";
const OTHER = "8f14e45f-ceea-4d0e-a1a2-9b1c0d3e4f5a";
const COLORS = { color: "#fff", backgroundColor: "#35c" };

describe("emphasizeSelfMentions", () => {
  it("bolds the viewer's member mention and @all, leaves others alone", () => {
    const md = [
      `[@Me](mention://member/${VIEWER})`,
      `[@Other](mention://member/${OTHER})`,
      `[@Bot](mention://agent/${VIEWER})`,
      "[@all](mention://all/all)",
    ].join(" ");

    expect(emphasizeSelfMentions(md, VIEWER)).toBe(
      [
        `[**@Me**](mention://member/${VIEWER})`,
        `[@Other](mention://member/${OTHER})`,
        `[@Bot](mention://agent/${VIEWER})`,
        "[**@all**](mention://all/all)",
      ].join(" "),
    );
  });

  it("is a no-op without a viewer", () => {
    const md = `[@Me](mention://member/${VIEWER}) [@all](mention://all/all)`;
    expect(emphasizeSelfMentions(md, null)).toBe(md);
  });

  it("does not re-wrap or break labels that already carry markdown emphasis", () => {
    const md = `[@a*b](mention://member/${VIEWER})`;
    expect(emphasizeSelfMentions(md, VIEWER)).toBe(md);
  });
});

describe("selfMentionLinkVariants", () => {
  it("matches the viewer's member URL and every @all URL only", () => {
    const variants = selfMentionLinkVariants(VIEWER, COLORS);
    const hit = (url: string) =>
      Object.keys(variants).some((p) => new RegExp(p).test(url));

    expect(hit(`mention://member/${VIEWER}`)).toBe(true);
    expect(hit("mention://all/all")).toBe(true);
    expect(hit(`mention://member/${OTHER}`)).toBe(false);
    expect(hit(`mention://agent/${VIEWER}`)).toBe(false);
    expect(hit(`mention://member/${VIEWER}x`)).toBe(false);
    expect(hit("https://example.com/mention://all/")).toBe(false);
  });

  it("highlights with a filled background and no underline", () => {
    for (const style of Object.values(selfMentionLinkVariants(VIEWER, COLORS))) {
      expect(style).toEqual({ ...COLORS, underline: false });
    }
  });

  it("returns no variants without a viewer", () => {
    expect(selfMentionLinkVariants(null, COLORS)).toEqual({});
  });
});

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { act, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createAuthStore, registerAuthStore } from "@multica/core/auth";
import type { ApiClient } from "@multica/core/api";
import type { User } from "@multica/core/types";
import { RichContent } from "./rich-content";

vi.mock("../i18n", async () => {
  const editor = (await import("../locales/en/editor.json")).default;
  return {
    useT: () => ({
      t: (select: (bundle: typeof editor) => string) => select(editor),
    }),
  };
});

vi.mock("../editor/link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));

const VIEWER_ID = "f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff";
const OTHER_ID = "8f14e45f-ceea-4d0e-a1a2-9b1c0d3e4f5a";
const MENTION_STYLES = readFileSync(resolve(import.meta.dirname, "rich-content.css"), "utf8");
const TOKENS = readFileSync(resolve(import.meta.dirname, "../../ui/styles/tokens.css"), "utf8");
const CONTENT = [
  `[@ChEeSe](mention://member/${VIEWER_ID})`,
  `[@Other](mention://member/${OTHER_ID})`,
  `[@Agent](mention://agent/${VIEWER_ID})`,
  "[@all](mention://all/all)",
].join(" ");

// WCAG relative luminance of an `oklch(L C H)` token (oklab -> linear sRGB).
type Vec3 = [number, number, number];
const cube = ([x, y, z]: Vec3): Vec3 => [x ** 3, y ** 3, z ** 3];

function luminance(oklch: string) {
  const [l, c, h] = oklch.match(/oklch\(([^)]+)\)/)![1]!.split(/\s+/).map(Number) as Vec3;
  const a = c * Math.cos((h * Math.PI) / 180);
  const b = c * Math.sin((h * Math.PI) / 180);
  const [lp, mp, sp] = cube([
    l + 0.3963377774 * a + 0.2158037573 * b,
    l - 0.1055613458 * a - 0.0638541728 * b,
    l - 0.0894841775 * a - 1.291485548 * b,
  ]);
  const [r, g, bl] = (
    [
      4.0767416621 * lp - 3.3077115913 * mp + 0.2309699292 * sp,
      -1.2684380046 * lp + 2.6097574011 * mp - 0.3413193965 * sp,
      -0.0041960863 * lp - 0.7034186147 * mp + 1.707614701 * sp,
    ] as Vec3
  ).map((v) => Math.min(1, Math.max(0, v))) as Vec3;
  return 0.2126 * r + 0.7152 * g + 0.0722 * bl;
}

function tokenBlock(selector: string) {
  return TOKENS.split(new RegExp(`^${selector.replace(".", "\\.")} \\{`, "m"))[1]!.split("\n}")[0]!;
}

let authStore: ReturnType<typeof createAuthStore>;

beforeEach(() => {
  authStore = createAuthStore({
    api: {} as ApiClient,
    storage: {
      getItem: () => null,
      setItem: () => {},
      removeItem: () => {},
    },
  });
  authStore.setState({ user: { id: VIEWER_ID } as User });
  registerAuthStore(authStore);
});

describe("RichContent self-member mentions", () => {
  it.each(["compact", "document"] as const)(
    "uses the mention-self fill and text tokens only for the viewer in %s content",
    (density) => {
      const { container } = render(
        <>
          <style>{MENTION_STYLES}</style>
          <RichContent content={CONTENT} density={density} />
        </>,
      );
      const selfRule = Array.from(container.querySelector("style")!.sheet!.cssRules).find(
        (rule): rule is CSSStyleRule =>
          rule instanceof CSSStyleRule && rule.selectorText.includes(".mention-self"),
      )!;

      expect(screen.getByText("@ChEeSe")).toHaveClass("mention-self");
      expect(screen.getByText("@ChEeSe").matches(selfRule.selectorText)).toBe(true);
      expect(selfRule.style.getPropertyValue("background-color")).toBe("var(--mention-self-bg)");
      expect(selfRule.style.getPropertyValue("color")).toBe("var(--mention-self-fg)");
      for (const label of ["@Other", "@Agent", "@all"]) {
        expect(screen.getByText(label).matches(selfRule.selectorText)).toBe(false);
        expect(screen.getByText(label)).toHaveAttribute("class", "mention");
      }
    },
  );

  it.each([":root", ".dark"])("keeps mention-self text/fill contrast >= 4.5:1 in %s", (selector) => {
    const block = tokenBlock(selector);
    const [bg, fg] = ["bg", "fg"].map((k) => luminance(block.match(new RegExp(`--mention-self-${k}:\\s*(oklch\\([^)]+\\))`))![1]!));
    expect((Math.max(bg!, fg!) + 0.05) / (Math.min(bg!, fg!) + 0.05)).toBeGreaterThanOrEqual(4.5);
  });

  it.each(["compact", "document"] as const)(
    "highlights only the viewer's member mention in %s content without changing case",
    (density) => {
      render(<RichContent content={CONTENT} density={density} />);

      expect(screen.getByText("@ChEeSe")).toHaveClass("mention", "mention-self");
      expect(screen.getByText("@ChEeSe")).toHaveTextContent(/^@ChEeSe$/);
      for (const label of ["@Other", "@Agent", "@all"]) {
        expect(screen.getByText(label)).toHaveAttribute("class", "mention");
      }
    },
  );

  it("does not highlight member mentions without a current user", () => {
    authStore.setState({ user: null });
    const { container } = render(<RichContent content={CONTENT} />);

    expect(container.querySelectorAll(".mention")).toHaveLength(4);
    expect(container.querySelector(".mention-self")).toBeNull();
  });

  it("updates the self variant when the viewer changes or logs out", () => {
    render(<RichContent content={CONTENT} />);

    act(() => authStore.setState({ user: { id: OTHER_ID } as User }));
    expect(screen.getByText("@ChEeSe")).toHaveAttribute("class", "mention");
    expect(screen.getByText("@Other")).toHaveClass("mention-self");

    act(() => authStore.setState({ user: null }));
    expect(screen.getByText("@Other")).toHaveAttribute("class", "mention");
  });
});

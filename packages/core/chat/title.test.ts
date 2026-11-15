import { describe, expect, it } from "vitest";
import { deriveChatTitle } from "./title";

const VI =
  "tôi muốn adopt ý tưởng này vào hệ thống chat của chúng ta để tiêu đề cuộc trò chuyện hiển thị đầy đủ hơn nhiều";

describe("deriveChatTitle", () => {
  it("cuts a long Vietnamese message at a word boundary with no ellipsis", () => {
    expect(Array.from(VI).length).toBeGreaterThan(80);
    const t = deriveChatTitle(VI);
    expect(t).not.toContain("…");
    expect(t).not.toMatch(/\.\.\.$/);
    expect(Array.from(t).length).toBeLessThanOrEqual(80);
    expect(Array.from(t).length).toBeGreaterThanOrEqual(60);
    expect(VI.startsWith(t + " ")).toBe(true);
  });

  it("counts code points, so emoji never split", () => {
    const t = deriveChatTitle("😀".repeat(100));
    expect(Array.from(t)).toHaveLength(80);
    expect(t).toBe("😀".repeat(80));
  });

  it("uses the first non-empty line and keeps short text whole", () => {
    expect(deriveChatTitle("\n# Hello  world\nsecond")).toBe("Hello world");
    expect(deriveChatTitle("a".repeat(80))).toBe("a".repeat(80));
  });
});

describe("deriveChatTitle long URL", () => {
  it("hard-cuts a long URL instead of dropping it", () => {
    const msg =
      "tôi muốn adopt ý tưởng này https://github.com/cheese-work/multica/blob/main/docs/some/very/long/path/README.md";
    const t = deriveChatTitle(msg);
    expect(Array.from(t)).toHaveLength(80);
    expect(msg.startsWith(t)).toBe(true);
  });
});

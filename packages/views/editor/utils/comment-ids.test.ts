import { describe, expect, it } from "vitest";
import { preprocessCommentIds } from "@multica/ui/markdown";

const ID = "01a1769e-060e-7b2c-84dd-b396f8d9a660";

describe("preprocessCommentIds", () => {
  it("rewrites a full UUID into a comment mention link", () => {
    expect(preprocessCommentIds(`See comment ${ID} for details`)).toBe(
      `See comment [${ID}](mention://comment/${ID}) for details`,
    );
  });

  it("leaves a short prefix as plain text", () => {
    const text = "See comment 01a1769e-060e for details";
    expect(preprocessCommentIds(text)).toBe(text);
  });

  it("skips code, existing links and URLs", () => {
    for (const text of [
      `\`${ID}\``,
      `\`\`\`\n${ID}\n\`\`\``,
      `[label](https://x.test/issues/1#comment-${ID})`,
      `https://x.test/issues/1#comment-${ID}`,
    ]) {
      expect(preprocessCommentIds(text)).toBe(text);
    }
  });

  it("does not match inside a longer hex-dash token", () => {
    const text = `x${ID}-1`;
    expect(preprocessCommentIds(text)).toBe(text);
  });
});

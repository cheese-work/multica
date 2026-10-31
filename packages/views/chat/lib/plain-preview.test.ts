import { describe, expect, it } from "vitest";
import { plainPreview } from "./plain-preview";

describe("plainPreview", () => {
  it("drops Markdown quote markers and decodes HTML entities", () => {
    expect(plainPreview("> First&nbsp;line\n> > Second &amp; third")).toBe("First line Second & third");
  });

  it("strips heading, list, emphasis, code and link syntax down to the words", () => {
    expect(plainPreview("## Title\n- **bold** and `code`\n[a link](https://x.test/y)")).toBe("Title bold and code a link");
  });

  it("decodes numeric entities and collapses whitespace", () => {
    expect(plainPreview("a&#160;&#x41;\n\n\n  b")).toBe("a A b");
  });

  it("leaves unknown entities alone", () => {
    expect(plainPreview("&bogus; stays")).toBe("&bogus; stays");
  });
});

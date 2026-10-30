import { marked } from "marked";
import { describe, expect, it, vi } from "vitest";
import {
  SELF_MENTION_COLORS,
  emphasizeSelfMentions,
  emphasizeSelfMentionSegments,
  selfMentionLinkVariants,
} from "./self-mention";
import { splitMarkdown } from "./split-markdown";

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
    const hit = (url: string) => Object.keys(variants).some((p) => new RegExp(p).test(url));

    expect(hit(`mention://member/${VIEWER}`)).toBe(true);
    expect(hit("mention://all/all")).toBe(true);
    for (const url of [
      `mention://member/${OTHER}`,
      `mention://agent/${VIEWER}`,
      `mention://member/${VIEWER}x`,
      "https://example.com/mention://all/",
      "https://example.com",
    ]) {
      expect(hit(url)).toBe(false);
    }
    for (const style of Object.values(variants)) {
      expect(style).toMatchObject({
        color: t.fg,
        backgroundColor: t.bg,
        underline: false,
      });
    }
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
    expect(emphasizeSelfMentions(md, undefined)).toBe(md);
  });

  it("does not break labels that already carry markdown emphasis", () => {
    const md = `[@a*b](mention://member/${VIEWER})`;
    expect(emphasizeSelfMentions(md, VIEWER)).toBe(md);
  });

  it.each([
    ["@john_doe", "@john_doe"],
    ["@David\\[TF\\]", "@David\\[TF\\]"],
    ["@a\\*b", "@a\\*b"],
  ])("bolds the valid label %s", (label) => {
    const md = `[${label}](mention://member/${VIEWER})`;
    expect(emphasizeSelfMentions(md, VIEWER)).toBe(`[**${label}**](mention://member/${VIEWER})`);
  });

  it("leaves inline code and fenced code (``` and ~~~) verbatim", () => {
    const link = `[@Me](mention://member/${VIEWER})`;
    const md = [
      `inline \`${link}\` and ${link}`,
      "",
      "```md",
      link,
      "```",
      "",
      "~~~",
      link,
      "~~~",
      "",
      `> \`\`\``,
      `> ${link}`,
      `> \`\`\``,
    ].join("\n");
    const out = emphasizeSelfMentions(md, VIEWER);

    expect(out.match(/\*\*/g)).toHaveLength(2); // only the prose mention
    expect(out).toContain(`inline \`${link}\` and [**@Me**](mention://member/${VIEWER})`);
    expect(out).toContain(`\`\`\`md\n${link}\n\`\`\``);
    expect(out).toContain(`~~~\n${link}\n~~~`);
    expect(out).toContain(`> ${link}`);
  });
});

describe("emphasizeSelfMentions in multi-line and unterminated fences", () => {
  const link = `[@Me](mention://member/${VIEWER})`;

  it("keeps every line of a multi-line fence and an unterminated fence verbatim", () => {
    const closed = ["```", link, link, "```", `after ${link}`].join("\n");
    expect(emphasizeSelfMentions(closed, VIEWER)).toBe(
      ["```", link, link, "```", `after [**@Me**](mention://member/${VIEWER})`].join("\n"),
    );

    const open = ["```", link, link].join("\n");
    expect(emphasizeSelfMentions(open, VIEWER)).toBe(open);
  });
});

describe("emphasizeSelfMentions with source that looks like scratch tags", () => {
  const link = `[@Me](mention://member/${VIEWER})`;
  const bold = `[**@Me**](mention://member/${VIEWER})`;
  const tagged = `[**@Me**](mention://member/${VIEWER}#sm0)`;
  const prose = (content: string) => [{ type: "prose", content }];
  const run = (source: string) => emphasizeSelfMentionSegments(splitMarkdown(source), VIEWER);

  it.each([
    ["inline code", `\`${link}\``],
    ["an escaped mention", `\\${link}`],
    ["an image nested in a quote", `> !${link}`],
  ])("keeps %s verbatim beside an existing #sm0 link", (_name, literal) => {
    const source = `${literal}\n\n${tagged}`;
    expect(run(source)).toEqual(prose(source));
  });

  it("still bolds a real mention beside look-alikes and keeps its own fragment", () => {
    const source = `\`${link}\`\n\n${tagged}\n\n${link}\n\n[@all](mention://all/all#sm20)`;
    expect(run(source)).toEqual(
      prose(`\`${link}\`\n\n${tagged}\n\n${bold}\n\n[**@all**](mention://all/all#sm20)`),
    );
  });

  it("survives sources that already contain longer tag prefixes", () => {
    const head = `[x](https://e.com/#smx0) #sm0 #smxx1\n\n\`${link}\`\n\n`;
    expect(run(head + link)).toEqual(prose(head + bold));
  });

  it("leaves reference definitions, link labels and html blocks alone", () => {
    const ref = `[@Me][r]\n\n[r]: mention://member/${VIEWER}#sm0`;
    expect(run(ref)).toEqual(prose(ref));
    const html = `<div>\n${link}\n</div>`;
    expect(run(html)).toEqual(prose(html));
  });

  it("bolds a mention in a table cell next to a tagged look-alike", () => {
    expect(run(`| a |\n| - |\n| ${link} |\n\n\`${link}\`\n\n${tagged}`)).toEqual(
      prose(`| a |\n| - |\n| ${bold} |\n\n\`${link}\`\n\n${tagged}`),
    );
  });
});

describe("emphasizeSelfMentions with a long look-alike prefix", () => {
  it("handles a 32 KB run of tag-like text without throwing", () => {
    const prefix = `#sm${"x".repeat(32_768)}\n\n`;
    expect(emphasizeSelfMentions(`${prefix}[@all](mention://all/all)`, VIEWER)).toBe(
      `${prefix}[**@all**](mention://all/all)`,
    );
  });

  it("finds a free marker among many look-alikes without growing it", () => {
    const lookAlikes = Array.from({ length: 2_000 }, (_, i) => `#sm${i.toString(36)}`).join(" ");
    const lexer = vi.spyOn(marked, "lexer");
    try {
      expect(emphasizeSelfMentions(`${lookAlikes}\n\n[@all](mention://all/all)`, VIEWER)).toBe(
        `${lookAlikes}\n\n[**@all**](mention://all/all)`,
      );
      expect(lexer.mock.calls.length).toBeLessThanOrEqual(2);
      expect(String(lexer.mock.calls[0][0]).length).toBeLessThan(lookAlikes.length + 200);
    } finally {
      lexer.mockRestore();
    }
  });
});

describe("emphasizeSelfMentions when the lexer fails", () => {
  it("returns the source unchanged instead of throwing", () => {
    const lexer = vi.spyOn(marked, "lexer").mockImplementation(() => {
      throw new Error("boom");
    });
    try {
      const source = "[@all](mention://all/all)";
      expect(emphasizeSelfMentions(source, VIEWER)).toBe(source);
    } finally {
      lexer.mockRestore();
    }
  });
});

describe("emphasizeSelfMentions render cost", () => {
  it("lexes a long segment a constant number of times", () => {
    const count = 800;
    const all = "[@all](mention://all/all)";
    const lexer = vi.spyOn(marked, "lexer");
    try {
      const out = emphasizeSelfMentions(Array(count).fill(all).join(" "), VIEWER);
      expect(out).toBe(Array(count).fill("[**@all**](mention://all/all)").join(" "));
      expect(lexer.mock.calls.length).toBeLessThanOrEqual(2);
    } finally {
      lexer.mockRestore();
    }
  });
});

describe("emphasizeSelfMentionSegments", () => {
  const link = `[@Me](mention://member/${VIEWER})`;

  it("keeps top-level fenced and indented code blocks verbatim", () => {
    const md = [`hi ${link}`, "", "```", link, "```", "", `    ${link}`, "", `bye ${link}`].join("\n");
    const segments = emphasizeSelfMentionSegments(splitMarkdown(md), VIEWER);
    const code = segments.filter((s) => s.type === "code");
    const prose = segments.filter((s) => s.type === "prose");

    expect(code.map((s) => (s as { code: string }).code)).toEqual([link, link]);
    expect(prose.map((s) => (s as { content: string }).content).join("")).toContain("[**@Me**]");
    expect(JSON.stringify(code)).not.toContain("**");
  });

  it.each([
    ["indented code nested in a list", `- Example:\n\n      ${link}\n      [@all](mention://all/all)`],
    ["indented code nested in a quote", `> Example:\n>\n>     ${link}\n>     [@all](mention://all/all)`],
    ["tilde fence starting in a list item", `- ~~~md\n  ${link}\n  [@all](mention://all/all)\n  ~~~`],
  ])("keeps %s verbatim", (_name, source) => {
    expect(emphasizeSelfMentionSegments(splitMarkdown(source), VIEWER)).toEqual([
      { type: "prose", content: source },
    ]);
  });

  it("still bolds a prose mention next to nested code", () => {
    const source = `${link}\n\n- Example:\n\n      ${link}`;
    expect(emphasizeSelfMentionSegments(splitMarkdown(source), VIEWER)).toEqual([
      {
        type: "prose",
        content: `[**@Me**](mention://member/${VIEWER})\n\n- Example:\n\n      ${link}`,
      },
    ]);
  });

  it("keeps code verbatim when an escaped mention and a linked code label share the source", () => {
    const bold = `[**@Me**](mention://member/${VIEWER})`;
    const source = `\\${link}\n\n\`${link}\`\n\n${link}\n\n[\`${link}\`](https://example.com)`;
    expect(emphasizeSelfMentionSegments(splitMarkdown(source), VIEWER)).toEqual([
      {
        type: "prose",
        content: `\\${link}\n\n\`${link}\`\n\n${bold}\n\n[\`${link}\`](https://example.com)`,
      },
    ]);
  });

  it("still bolds a real mention next to an escaped mention", () => {
    const source = `\\${link} plus ${link}`;
    expect(emphasizeSelfMentionSegments(splitMarkdown(source), VIEWER)).toEqual([
      { type: "prose", content: `\\${link} plus [**@Me**](mention://member/${VIEWER})` },
    ]);
  });

  it("bolds links in lists, quotes and tables but not escaped or image look-alikes", () => {
    const out = (source: string) => emphasizeSelfMentions(source, VIEWER);
    const bold = `[**@Me**](mention://member/${VIEWER})`;

    expect(out(`- ${link}`)).toBe(`- ${bold}`);
    expect(out(`> ${link}`)).toBe(`> ${bold}`);
    expect(out(`| a |\n| - |\n| ${link} |`)).toBe(`| a |\n| - |\n| ${bold} |`);
    expect(out(`\\${link}`)).toBe(`\\${link}`);
    expect(out(`!${link}`)).toBe(`!${link}`);
  });
});


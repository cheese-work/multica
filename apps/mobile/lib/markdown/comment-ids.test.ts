import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { CommentPreview } from "@multica/core/types";
import {
  findCommentIds,
  findSegmentCommentIds,
  linkifyCommentIds,
  linkifySegments,
  parseCommentMention,
} from "./comment-ids";
import { splitMarkdown } from "./split-markdown";

const ID = "01a1769e-060e-7a3c-9a0b-123456789abc";
const ISSUE = "01a176f4-37b7-7904-85d6-b7dac9187a61";

const preview = (over: Partial<CommentPreview> = {}): CommentPreview => ({
  comment_id: ID,
  issue_id: ISSUE,
  issue_identifier: "CHE-2120",
  author_type: "agent",
  author_id: "a1",
  author_name: "Luna",
  preview: "Root cause found",
  ...over,
});

const CTX = { webOrigin: "https://app.example.test", slug: "acme" };
const URL_FORM = `https://app.example.test/acme/issues/CHE-2120#comment-${ID}`;

const resolved = (p: CommentPreview) => new Map([[p.comment_id, p]]);

describe("findCommentIds", () => {
  it("finds full UUIDs in prose, deduped", () => {
    expect(findCommentIds(`see ${ID} and ${ID}`, CTX)).toEqual([ID]);
  });

  it("skips code, links and bare URLs", () => {
    const text = `\`${ID}\` [x](https://a.test/${ID}) https://a.test/c/${ID}`;
    expect(findCommentIds(text, CTX)).toEqual([]);
  });

  it("finds the comment of a bare or labeled in-app comment-anchor URL", () => {
    expect(findCommentIds(`open ${URL_FORM}`, CTX)).toEqual([ID]);
    expect(findCommentIds(`open [the thread](${URL_FORM})`, CTX)).toEqual([ID]);
  });

  it("ignores anchor URLs from another origin, workspace or with a query", () => {
    const other = `https://evil.test/acme/issues/CHE-1#comment-${ID}`;
    const slug = `https://app.example.test/other/issues/CHE-1#comment-${ID}`;
    const query = `https://app.example.test/acme/issues/CHE-1?x=1#comment-${ID}`;
    expect(findCommentIds(`${other} [a](${slug}) ${query}`, CTX)).toEqual([]);
    expect(findCommentIds(`open ${URL_FORM}`, { ...CTX, webOrigin: null })).toEqual([]);
  });

  it("ignores short prefixes", () => {
    expect(findCommentIds("see 01a1769e-060e please", CTX)).toEqual([]);
  });
});

describe("linkifyCommentIds", () => {
  it("rewrites a resolved id to a chip link carrying the issue id", () => {
    expect(linkifyCommentIds(`see ${ID}.`, resolved(preview()), CTX)).toBe(
      `see [CHE-2120 · Luna: Root cause found](mention://comment/${ID}?issue=${ISSUE}).`,
    );
  });

  it("leaves unresolved ids as plain text", () => {
    expect(linkifyCommentIds(`see ${ID}`, new Map(), CTX)).toBe(`see ${ID}`);
  });

  it("escapes markdown-breaking characters in the label", () => {
    const out = linkifyCommentIds(ID, resolved(preview({ preview: "a [b] c" })), CTX);
    expect(out).toContain("a \\[b\\] c");
  });

  it("omits the author when absent", () => {
    const out = linkifyCommentIds(ID, resolved(preview({ author_name: "" })), CTX);
    expect(out.startsWith("[CHE-2120 · Root cause found]")).toBe(true);
  });

  it("rewrites a bare or labeled anchor URL to the chip", () => {
    const chip = `[CHE-2120 · Luna: Root cause found](mention://comment/${ID}?issue=${ISSUE})`;
    expect(linkifyCommentIds(`see ${URL_FORM}.`, resolved(preview()), CTX)).toBe(`see ${chip}.`);
    expect(linkifyCommentIds(`see [x](${URL_FORM})`, resolved(preview()), CTX)).toBe(`see ${chip}`);
  });

  it("keeps an unresolved anchor URL as the author wrote it", () => {
    const text = `see [x](${URL_FORM}) and ${URL_FORM}`;
    expect(linkifyCommentIds(text, new Map(), CTX)).toBe(text);
  });

  it("does not rewrite inside code or links", () => {
    const text = `\`${ID}\``;
    expect(linkifyCommentIds(text, resolved(preview()), CTX)).toBe(text);
  });

  it.each([
    ["double-backtick span", `see \`\`${ID}\`\` now`],
    ["span with a single backtick inside", `\`\`a \` ${ID}\`\``],
    ["triple-backtick inline span", `x \`\`\`${ID}\`\`\` y`],
    ["multi-line span", `\`\`a\n${ID}\`\``],
    ["quoted fence", `> \`\`\`\n> ${ID}\n> \`\`\``],
    ["list-nested fence", `- item\n  \`\`\`js\n  ${ID}\n  \`\`\`\n- next`],
    ["tilde fence in a list", `1. step\n   ~~~\n   ${ID}\n   ~~~`],
    ["unclosed nested fence", `> \`\`\`\n> ${ID}`],
  ])("leaves a %s byte-identical", (_name, text) => {
    expect(findCommentIds(text, CTX)).toEqual([]);
    expect(linkifyCommentIds(text, resolved(preview()), CTX)).toBe(text);
  });

  it("still rewrites prose around, and after, a double-backtick span", () => {
    const out = linkifyCommentIds(`\`\`${ID}\`\` then ${ID}`, resolved(preview()), CTX);
    expect(out.startsWith(`\`\`${ID}\`\` then [CHE-2120`)).toBe(true);
  });

  it("does not let a lone backtick or an unclosed span swallow later ids", () => {
    const out = linkifyCommentIds(`it\`s ${ID}\n\n\`\`open ${ID}`, resolved(preview()), CTX);
    expect(out.match(/\[CHE-2120/g)).toHaveLength(2);
  });
});

describe("parseCommentMention", () => {
  it("parses comment and issue ids", () => {
    expect(
      parseCommentMention(`mention://comment/${ID}?issue=${ISSUE}`),
    ).toEqual({ commentId: ID, issueId: ISSUE });
  });

  it("rejects non-comment and malformed urls", () => {
    expect(parseCommentMention(`mention://issue/${ID}`)).toBeNull();
    expect(parseCommentMention(`mention://comment/${ID}`)).toBeNull();
    expect(parseCommentMention(`mention://comment/not-a-uuid?issue=${ISSUE}`)).toBeNull();
  });
});

describe("segments", () => {
  const md = "before " + ID + "\n\n```\n" + ID + "\n```\n";

  it("finds ids in prose segments only, not fenced code", () => {
    expect(findSegmentCommentIds(splitMarkdown(md), CTX)).toEqual([ID]);
    expect(findSegmentCommentIds(splitMarkdown("```\n" + ID + "\n```"), CTX)).toEqual([]);
  });

  it("leaves fenced code byte-identical when a prose id resolves", () => {
    const out = linkifySegments(splitMarkdown(md), resolved(preview()), CTX);
    expect(out[0]).toMatchObject({ type: "prose" });
    expect((out[0] as { content: string }).content).toContain("mention://comment/");
    expect(out[1]).toEqual({ type: "code", lang: undefined, code: ID });
  });
});

// CHE-2969 / CHE-2922 native run: on the Android dev client (Hermes 0.17) no
// comment-preview request left the app, so the full UUID, the labeled anchor
// and the bare anchor stayed plain. Cause, reproduced with a real Hermes v0.17.0
// `hermes` binary: `String.prototype.matchAll` results carry no `.groups`
// (`exec` and `replace` do). The scan read `m.groups`, found no id, so nothing
// was fetched. Named groups themselves compile and match fine. This test
// emulates Hermes' `matchAll`: the scan must read numbered groups only.
describe("findCommentIds on an engine whose matchAll drops .groups (Hermes)", () => {
  beforeEach(() => {
    const real = String.prototype.matchAll;
    vi.spyOn(String.prototype, "matchAll").mockImplementation(function (
      this: string,
      re: RegExp,
    ) {
      const hermes = Array.from(real.call(this, re), (m) => {
        delete (m as { groups?: unknown }).groups;
        return m;
      });
      return hermes[Symbol.iterator]() as ReturnType<typeof real>;
    });
  });
  afterEach(() => vi.restoreAllMocks());

  it("finds a full UUID in prose", () => {
    expect(findCommentIds(`see ${ID} ok`, CTX)).toEqual([ID]);
  });

  it("finds the comment of a labeled in-app anchor link", () => {
    expect(findCommentIds(`open [the root comment](${URL_FORM})`, CTX)).toEqual([ID]);
  });

  it("finds the comment of a bare in-app anchor URL", () => {
    expect(findCommentIds(`open ${URL_FORM}`, CTX)).toEqual([ID]);
  });
});

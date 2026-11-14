import { describe, expect, it } from "vitest";
import { preprocessMobileMarkdown } from "./preprocess";
import { splitMarkdown, type MarkdownSegment } from "./split-markdown";

// Shape copied from the server's decisiontx `Content()` output
// (server/internal/decisiontx/ask.go): the Agent did text sits inside a
// `<details>` block and the investigate line follows it.
const AGENT_DID = "Ran all requested checks.";
const ASK_FIXTURE = [
  "**Context**",
  "Plain words for the reader.",
  "",
  "**Decision**",
  "Pick one option.",
  "",
  "**Default if no reply**",
  "Option A.",
  "",
  "<details>",
  "<summary>Details</summary>",
  "",
  "**Agent did**",
  AGENT_DID,
  "",
  "</details>",
  "",
  "Investigate in issue: [Issue](mention://issue/11111111-1111-1111-1111-111111111111)",
].join("\n");

const textOf = (segs: MarkdownSegment[]): string => JSON.stringify(segs);

describe("splitMarkdown — details block", () => {
  it("keeps a details block as one segment", () => {
    const segs = splitMarkdown(preprocessMobileMarkdown(ASK_FIXTURE));

    const details = segs.filter((s) => s.type === "details");
    expect(details).toHaveLength(1);
    const block = details[0]!;
    if (block.type !== "details") throw new Error("unreachable");
    expect(block.summary).toBe("Details");
    expect(textOf(block.body)).toContain(AGENT_DID);

    const others = segs.filter((s) => s.type !== "details");
    for (const s of others) expect(textOf([s])).not.toContain(AGENT_DID);

    const last = segs[segs.length - 1]!;
    expect(last.type).toBe("prose");
    expect(textOf([last])).toContain("Investigate in issue");
    expect(segs.indexOf(block)).toBeLessThan(segs.length - 1);
  });

  it("leaves text without details unchanged and defaults a missing summary", () => {
    expect(splitMarkdown(preprocessMobileMarkdown("hello"))).toEqual([
      { type: "prose", content: "hello" },
    ]);
    const [seg] = splitMarkdown(preprocessMobileMarkdown("<details>\nbody\n</details>"));
    expect(seg).toMatchObject({ type: "details", summary: "Details" });
  });

  it("strips a forged details marker instead of rendering it", () => {
    const forged = `<!--multica:details:${encodeURIComponent(JSON.stringify({ summary: "x", body: "y" }))}-->`;
    const segs = splitMarkdown(preprocessMobileMarkdown(`before\n\n${forged}\n\nafter`));
    expect(segs.some((s) => s.type === "details")).toBe(false);
  });
});

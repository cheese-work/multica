// @vitest-environment node
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

const readDoc = (filename: string) => readFileSync(resolve("content/docs", filename), "utf8");

describe("upstream sync documentation", () => {
  it("documents the upstream blocked issue-wakeup condition in the fork reference", () => {
    const content = readFileSync(resolve("../../server/internal/service/builtin_skills/multica-platform/references/wakeups.md"), "utf8");
    expect(content).toMatch(/--until-issue ISSUE[^\n]+`blocked`/);
  });

  it("keeps the steering anchor and demo alongside fork comment routing", () => {
    const content = readDoc("mentioning-agents.mdx");
    expect(content).toContain("## Steer a running agent");
    expect(content).toContain("<SteerDemo />");
    expect(content).toContain("Agent-authored comments use stricter routing");
    expect(content).toContain("each 10-minute window");
  });

  it.each(["github-integration.mdx", "github-integration.ja.mdx", "github-integration.ko.mdx", "github-integration.zh.mdx"])(
    "%s gives the restart instructions their own step",
    (filename) => {
      const content = readDoc(filename);
      expect(content.match(/^<Step>$/gm)?.length).toBe(content.match(/^<\/Step>$/gm)?.length);
      expect(content).toMatch(/<\/Step>\s*<Step>\s*### 3\./);
    },
  );
});

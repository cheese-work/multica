// @vitest-environment node
import { existsSync } from "node:fs";
import { join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { paths } from "@multica/core/paths";

// The wakeups deep links are query-string addresses: on web each must land on
// an App Router page of the existing project / issue detail route.
describe("web routes resolve the wakeups deep links", () => {
  const ws = paths.workspace("acme");
  const dashboard = resolve(__dirname, "[workspaceSlug]/(dashboard)");
  it.each([
    [ws.projectWakeups("p1"), "projects/[id]/page.tsx"],
    [ws.issueWakeups("i1"), "issues/[id]/page.tsx"],
  ])("%s", (url, page) => {
    const [, segment, id] = url.split("?")[0]!.split("/").filter(Boolean);
    expect(page.startsWith(`${segment}/`)).toBe(true);
    expect(id).toBeTruthy();
    expect(existsSync(join(dashboard, page))).toBe(true);
  });
});

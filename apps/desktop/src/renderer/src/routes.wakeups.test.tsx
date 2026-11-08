// @vitest-environment node
import { describe, expect, it } from "vitest";
import { matchRoutes } from "react-router-dom";
import { paths } from "@multica/core/paths";
import { appRoutes } from "./routes";

// The wakeups deep links are query-string addresses on the existing project
// and issue detail routes: they must resolve to those routes with the id.
describe("desktop routes resolve the wakeups deep links", () => {
  const ws = paths.workspace("acme");
  it.each([
    [ws.projectWakeups("p1"), "projects/:id", "p1"],
    [ws.issueWakeups("i1"), "issues/:id", "i1"],
  ])("%s", (url, route, id) => {
    const pathname = url.split("?")[0]!;
    const matches = matchRoutes(appRoutes, pathname);
    const leaf = matches?.at(-1);
    expect(leaf?.route.path).toBe(route);
    expect(leaf?.params.id).toBe(id);
  });
});

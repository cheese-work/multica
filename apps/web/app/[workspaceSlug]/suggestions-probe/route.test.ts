// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { GET } from "./route";

beforeEach(() => vi.stubEnv("MULTICA_COOKIE_PROBE", "1"));
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllEnvs();
});

function probe(cookie?: string, slug = "acme") {
  return GET(new Request(`https://app.multica.test/${slug}/suggestions-probe?q=probe`, {
    headers: cookie === undefined ? {} : { cookie },
  }), { params: Promise.resolve({ workspaceSlug: slug }) });
}

describe("temporary session-cookie probe", () => {
  it.each([
    [undefined, false],
    ["multica_logged_in=1; multica_csrf=not-a-session", false],
    ["other_multica_auth=not-a-session", false],
    ["unrelated=1; multica_auth=private-session-value; locale=en", true],
    ["multica_auth=", true],
  ])("records only cookie presence for %j", async (cookie, present) => {
    const log = vi.spyOn(console, "info").mockImplementation(() => {});
    const response = await probe(cookie);
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toBe("application/x-suggestions+json; charset=utf-8");
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(response.headers.get("set-cookie")).toBeNull();
    expect(response.headers.get("x-multica-cookie-probe")).toBe(present ? "present" : "absent");
    expect(await response.json()).toEqual(["probe", ["Multica cookie probe"]]);
    expect(log.mock.calls).toEqual([["Multica cookie probe: session cookie present", present]]);
  });

  it("does not echo a cookie value or private data to the response or logs", async () => {
    const log = vi.spyOn(console, "info").mockImplementation(() => {});
    const response = await probe("multica_auth=private-session-value");
    const output = `${await response.text()}${JSON.stringify([...response.headers])}${JSON.stringify(log.mock.calls)}`;
    expect(output).not.toContain("private-session-value");
    expect(output).not.toContain("multica_auth");
  });

  it("does not record a request when the probe is disabled", async () => {
    vi.stubEnv("MULTICA_COOKIE_PROBE", "0");
    const log = vi.spyOn(console, "info").mockImplementation(() => {});
    expect((await probe("multica_auth=private-session-value")).status).toBe(404);
    expect(log).not.toHaveBeenCalled();
  });

  it("rejects invalid workspace slugs without recording cookies", async () => {
    const log = vi.spyOn(console, "info").mockImplementation(() => {});
    expect((await probe("multica_auth=private-session-value", "ACME")).status).toBe(404);
    expect(log).not.toHaveBeenCalled();
  });
});

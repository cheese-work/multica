import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import { existsSync, mkdtempSync, unlinkSync, rmdirSync } from "node:fs";
import { request } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

test("cookie eligibility control and sanitized error diagnostics", { timeout: 15000 }, async () => {
  const directory = mkdtempSync(join(tmpdir(), "multica-cookie-probe-test-"));
  const artifact = join(directory, "probe.mjs");
  let child;
  let exited;
  let logs = "";
  try {
    execFileSync("node_modules/.pnpm/esbuild@0.25.12/node_modules/esbuild/bin/esbuild", [
      "scripts/cookie-probe.ts", "--bundle", "--platform=node", "--format=esm", `--outfile=${artifact}`,
    ]);
    child = spawn(process.execPath, [artifact], {
      env: {
        ...process.env, MULTICA_COOKIE_PROBE: "1", COOKIE_PROBE_PORT: "0",
        COOKIE_PROBE_ORIGIN: "http://127.0.0.1:19693",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    child.stdout.on("data", chunk => logs += chunk);
    child.stderr.on("data", chunk => logs += chunk);
    exited = once(child, "exit");
    await Promise.race([
      once(child.stdout, "data"),
      exited.then(() => { throw new Error("Probe exited before readiness"); }),
    ]);
    const port = Number(logs.match(/loopback port (\d+)/)?.[1]);
    assert.ok(port > 0);
    const origin = `http://127.0.0.1:${port}`;
    const status = async () => (await fetch(`${origin}/probe-status`)).json();
    assert.deepEqual(await status(), {
      withCookie: 0, withoutCookie: 0, controlWithCookie: 0, controlWithoutCookie: 0,
      controlCookiePresent: false, errors: 0,
    });
    const absent = await fetch(origin);
    assert.match(await absent.text(), /INCONCLUSIVE/);
    assert.equal((await status()).controlWithoutCookie, 1);
    assert.equal((await status()).controlCookiePresent, false);
    const present = await fetch(origin, { headers: { cookie: "multica_auth=SYNTHETIC-NOT-A-SESSION" } });
    assert.match(await present.text(), /Cookie eligibility control: PASS/);
    await fetch(origin, { method: "HEAD" });
    assert.equal((await status()).controlWithCookie, 1);
    assert.equal((await status()).controlCookiePresent, true);
    const response = await fetch(`${origin}/cheese-work/suggestions-probe?q=SYNTHETIC-QUERY`);
    assert.deepEqual(await response.json(), ["SYNTHETIC-QUERY", ["Multica cookie probe"]]);
    assert.equal((await status()).withoutCookie, 1);
    const errorStatus = await new Promise((resolve, reject) => {
      const invalid = request({ hostname: "127.0.0.1", port, path: "http://[invalid/" }, response => {
        response.resume();
        response.on("end", () => resolve(response.statusCode));
      });
      invalid.on("error", reject);
      invalid.end();
    });
    assert.equal(errorStatus, 500);
    assert.equal((await status()).errors, 1);
    assert.match(logs, /Cookie probe request failed/);
    assert.ok(!logs.includes("SYNTHETIC-NOT-A-SESSION"));
    assert.ok(!logs.includes("SYNTHETIC-QUERY"));
    assert.ok(!logs.includes("http://[invalid/"));
  } finally {
    if (child && child.exitCode === null) child.kill("SIGTERM");
    if (exited) await exited;
    if (existsSync(artifact)) unlinkSync(artifact);
    rmdirSync(directory);
  }
});

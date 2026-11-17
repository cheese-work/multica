import assert from "node:assert/strict";
import { createServer } from "node:http";
import test from "node:test";

// CHE-6481: the Playwright global setup warm-up fetched dev-server routes with no timeout, so one
// route that never answers held the E2E step until the 60 min job limit. This test serves a route
// that accepts the request and never replies, and requires the warm-up call to end on its timeout.
test("warmFetch gives up on a route that never answers", async () => {
  const sockets = new Set();
  const server = createServer(() => {}); // accepts, never responds
  server.on("connection", (socket) => sockets.add(socket));
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  const code = `
    import { warmFetch } from ${JSON.stringify(new URL("../e2e/warmup-fetch.ts", import.meta.url).href)};
    const started = Date.now();
    await warmFetch("http://127.0.0.1:${port}/slow", 500);
    console.log(Date.now() - started);
  `;
  const child = await new Promise((resolve) => {
    // async spawn: the parent event loop must stay free to run the hanging server
    import("node:child_process").then(({ spawn }) => {
      const p = spawn(process.execPath, ["--experimental-strip-types", "--no-warnings", "--input-type=module", "-e", code], { stdio: ["ignore", "pipe", "pipe"] });
      let out = "";
      p.stdout.on("data", (d) => (out += d));
      const killer = setTimeout(() => p.kill("SIGKILL"), 8000);
      p.on("close", (status, signal) => { clearTimeout(killer); resolve({ status, signal, out }); });
    });
  });
  for (const socket of sockets) socket.destroy();
  server.close();
  assert.equal(child.signal, null, "warmFetch was still waiting after 8 s: no timeout");
  assert.equal(child.status, 0);
  const elapsed = Number(child.out.trim());
  assert.ok(elapsed >= 400 && elapsed < 5000, `expected to end near the 500 ms timeout, took ${elapsed} ms`);
});

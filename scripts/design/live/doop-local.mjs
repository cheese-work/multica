// A local doop server for the Live track (CHE-6753 L6): the job's own server
// that stages frames and runs `traverse.mjs` before any production call.
//
// It boots `cheese-work/doop` from a checkout that was built on X99 (`vite
// build` has produced `dist/`, `bun install` has produced `node_modules/`).
// The server uses embedded PGlite in a temp directory, listens on 127.0.0.1's
// `localhost`, and never reads a production URL or key. It signs one throwaway
// user up and mints one agent key, so the same `/mcp` calls as production work.
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdtempSync, rmSync, symlinkSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";

const freePort = () =>
  new Promise((resolve, reject) => {
    const probe = createServer();
    probe.once("error", reject);
    probe.listen(0, "127.0.0.1", () => {
      const { port } = probe.address();
      probe.close(() => resolve(port));
    });
  });

/** Parse a `/mcp` reply: plain JSON, or an SSE body with one `data:` line. */
function parseRpc(text) {
  const data = text.startsWith("{") ? text : text.split("\n").find((l) => l.startsWith("data:"))?.slice(5);
  if (!data) throw new Error(`unreadable /mcp reply: ${text.slice(0, 200)}`);
  return JSON.parse(data);
}

export async function startLocalDoop({ doopDir, bootTimeoutMs = 90_000 } = {}) {
  if (!doopDir) throw new Error("startLocalDoop: doopDir is required");
  const dataDir = mkdtempSync(path.join(tmpdir(), "live-doop-"));
  // the server serves the client bundle from <cwd>/dist, and its data from <cwd>/data
  symlinkSync(path.join(doopDir, "dist"), path.join(dataDir, "dist"));
  const port = await freePort();
  const base = `http://localhost:${port}`;
  const proc = spawn(path.join(doopDir, "node_modules", ".bin", "tsx"), [path.join(doopDir, "server", "index.ts")], {
    cwd: dataDir, // PGlite persists to <cwd>/data
    env: {
      ...process.env,
      PORT: String(port),
      NODE_ENV: "production",
      BETTER_AUTH_SECRET: randomBytes(32).toString("hex"),
      BETTER_AUTH_URL: base,
      DATABASE_URL: "",
      POSTHOG_KEY: "",
      VITE_POSTHOG_KEY: "",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let log = "";
  proc.stdout.on("data", (c) => (log = (log + c).slice(-4000)));
  proc.stderr.on("data", (c) => (log = (log + c).slice(-4000)));
  const exited = new Promise((resolve) => proc.once("exit", resolve));
  const stop = async () => {
    proc.kill();
    await exited;
    rmSync(dataDir, { recursive: true, force: true });
  };

  const deadline = Date.now() + bootTimeoutMs;
  for (;;) {
    try {
      if ((await fetch(`${base}/healthz`)).ok) break;
    } catch {
      /* not up yet */
    }
    if (proc.exitCode !== null || Date.now() > deadline) {
      await stop();
      throw new Error(`local doop did not start:\n${log}`);
    }
    await new Promise((r) => setTimeout(r, 100));
  }

  const jar = new Map();
  const http = async (p, init = {}) => {
    const res = await fetch(base + p, {
      ...init,
      headers: {
        "Content-Type": "application/json",
        Origin: base,
        Cookie: [...jar].map(([k, v]) => `${k}=${v}`).join("; "),
        ...init.headers,
      },
      redirect: "manual",
    });
    for (const c of res.headers.getSetCookie()) {
      const [pair] = c.split(";");
      const i = pair.indexOf("=");
      jar.set(pair.slice(0, i).trim(), pair.slice(i + 1).trim());
    }
    return res;
  };
  try {
    const up = await http("/api/auth/sign-up/email", {
      method: "POST",
      body: JSON.stringify({ email: "live@local.test", password: "password12345", name: "Live" }),
    });
    if (up.status !== 200) throw new Error(`sign-up failed: ${up.status} ${await up.text()}`);
    const minted = await http("/api/agent-keys", { method: "POST", body: JSON.stringify({ name: "live" }) });
    if (minted.status !== 200) throw new Error(`agent key failed: ${minted.status}`);
    var secret = (await minted.json()).secret;
  } catch (e) {
    await stop();
    throw e;
  }

  let rpcId = 1;
  /** Call one MCP tool; the tool's JSON text comes back parsed. A tool error throws. */
  const mcp = async (name, args) => {
    const res = await fetch(`${base}/mcp`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: `Bearer ${secret}`,
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: rpcId++, method: "tools/call", params: { name, arguments: args } }),
    });
    const body = parseRpc(await res.text());
    if (body.error) throw new Error(`${name}: ${body.error.message}`);
    const text = body.result?.content?.find((c) => c.type === "text")?.text ?? "";
    if (body.result?.isError) throw new Error(`${name}: ${text}`);
    try {
      return JSON.parse(text);
    } catch {
      return text;
    }
  };

  return {
    base,
    port,
    mcp,
    stop,
    /** Session cookies for a Playwright context (the player page is behind the session). */
    cookies: () => [...jar].map(([name, value]) => ({ name, value, url: base })),
    async createCanvas(name) {
      const res = await http("/api/canvases", { method: "POST", body: JSON.stringify({ name }) });
      if (res.status !== 200) throw new Error(`create canvas failed: ${res.status}`);
      return res.json();
    },
    async createFrame(canvasId, name, html) {
      const out = await mcp("create_frame", { canvas_id: canvasId, name, html, width: 400, height: 300, agent_name: "Live" });
      return out.frame.id;
    },
    async setFrameHtml(frameId, html) {
      await mcp("set_frame_html", { frame_id: frameId, html, agent_name: "Live" });
    },
  };
}

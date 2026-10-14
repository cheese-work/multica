// @vitest-environment node
import { createServer } from "node:http";
import { spawn, type ChildProcess } from "node:child_process";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

const appDirectory = dirname(fileURLToPath(import.meta.url));
let upstreamServer: ReturnType<typeof createServer>;
let nextProcess: ChildProcess | undefined;
let nextOutput = "";
let appUrl = "";
let upstreamClosed = false;
const upstreamRequests: Array<{
  url: string | undefined;
  cookie: string | undefined;
  authorization: string | undefined;
}> = [];

async function listen(server: ReturnType<typeof createServer>): Promise<number> {
  await new Promise<void>((resolveListen, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolveListen);
  });
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("Missing test-server address");
  return address.port;
}

async function waitForNextReady(): Promise<void> {
  await new Promise<void>((resolveReady, reject) => {
    const timeout = setTimeout(() => {
      clearInterval(poll);
      reject(new Error("Next.js did not become ready within 60 seconds"));
    }, 60_000);
    const poll = setInterval(() => {
      if (nextOutput.includes("Ready in")) {
        clearTimeout(timeout);
        clearInterval(poll);
        resolveReady();
      } else if (nextProcess?.exitCode !== null) {
        clearTimeout(timeout);
        clearInterval(poll);
        reject(new Error(`Next.js exited before readiness: ${nextOutput}`));
      }
    }, 100);
  });
}

async function waitForLog(text: string): Promise<void> {
  await new Promise<void>((resolveLog, reject) => {
    const timeout = setTimeout(() => {
      clearInterval(poll);
      reject(new Error(`Next.js did not log ${text}`));
    }, 5_000);
    const poll = setInterval(() => {
      if (nextOutput.includes(text)) {
        clearTimeout(timeout);
        clearInterval(poll);
        resolveLog();
      }
    }, 50);
  });
}

async function stopNext(): Promise<void> {
  if (!nextProcess?.pid || nextProcess.exitCode !== null || nextProcess.signalCode !== null) return;
  const exited = new Promise<void>((resolveExit) => nextProcess?.once("exit", () => resolveExit()));
  try {
    process.kill(-nextProcess.pid, "SIGTERM");
  } catch {
    return;
  }
  await Promise.race([exited, new Promise((resolveDelay) => setTimeout(resolveDelay, 5_000))]);
  if (nextProcess.exitCode === null && nextProcess.signalCode === null) {
    process.kill(-nextProcess.pid, "SIGKILL");
    await exited;
  }
}

describe("search suggestions proxy failure logging", () => {
  beforeAll(async () => {
    upstreamServer = createServer((request, response) => {
      upstreamRequests.push({
        url: request.url,
        cookie: request.headers.cookie,
        authorization: request.headers.authorization,
      });
      response.writeHead(200, { "content-type": "application/json", "cache-control": "no-store" });
      response.end(JSON.stringify({ url: request.url }));
    });
    const upstreamPort = await listen(upstreamServer);
    const portReservation = createServer();
    const nextPort = await listen(portReservation);
    await new Promise<void>((resolveClose, reject) => {
      portReservation.close((error) => (error ? reject(error) : resolveClose()));
    });
    appUrl = `http://127.0.0.1:${nextPort}`;

    nextProcess = spawn(
      process.execPath,
      [resolve(appDirectory, "node_modules/next/dist/bin/next"), "dev", "--webpack", "--port", String(nextPort)],
      {
        cwd: appDirectory,
        detached: true,
        env: {
          ...process.env,
          NODE_ENV: "development",
          REMOTE_API_URL: `http://127.0.0.1:${upstreamPort}`,
          DOCS_URL: "",
        },
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
    nextProcess.stdout?.on("data", (chunk: Buffer) => (nextOutput += chunk.toString()));
    nextProcess.stderr?.on("data", (chunk: Buffer) => (nextOutput += chunk.toString()));
    await waitForNextReady();
  }, 65_000);

  afterAll(async () => {
    await stopNext();
    if (!upstreamClosed && upstreamServer?.listening) {
      await new Promise<void>((resolveClose) => upstreamServer.close(() => resolveClose()));
    }
  });

  it("redacts query and referrer from failed sensitive proxy logs", async () => {
    const success = await fetch(
      `${appUrl}/api/search/suggestions?workspace_slug=che1593-preview&q=SAFE_QUERY_MARKER_1593`,
      {
        headers: {
          authorization: "Bearer synthetic-pat",
          cookie: "multica_session=synthetic-session",
        },
      },
    );
    expect(success.status).toBe(200);
    expect(await success.json()).toEqual({
      url: "/api/search/suggestions?workspace_slug=che1593-preview&q=SAFE_QUERY_MARKER_1593",
    });
    expect(upstreamRequests).toEqual([
      {
        url: "/api/search/suggestions?workspace_slug=che1593-preview&q=SAFE_QUERY_MARKER_1593",
        cookie: "multica_session=synthetic-session",
        authorization: "Bearer synthetic-pat",
      },
    ]);

    await new Promise<void>((resolveClose, reject) => {
      upstreamServer.close((error) => (error ? reject(error) : resolveClose()));
    });
    upstreamClosed = true;

    const queryMarker = "SUGGESTION_QUERY_MARKER_1593";
    const referrerMarker = "SUGGESTION_REFERRER_MARKER_1593";
    const failedSuggestion = await fetch(`${appUrl}/api/search/suggestions?q=${queryMarker}`, {
      headers: { referer: `https://referrer.invalid/${referrerMarker}` },
    });
    expect(failedSuggestion.status).toBeGreaterThanOrEqual(500);
    expect(failedSuggestion.status).toBeLessThan(600);

    const unrelatedMarker = "UNRELATED_QUERY_MARKER_1593";
    const failedUnrelated = await fetch(`${appUrl}/health?q=${unrelatedMarker}`);
    expect(failedUnrelated.status).toBeGreaterThanOrEqual(500);
    expect(failedUnrelated.status).toBeLessThan(600);
    await waitForLog(unrelatedMarker);

    expect(nextOutput).toContain("ECONNREFUSED");
    expect(nextOutput).toContain(unrelatedMarker);
    expect(nextOutput).not.toContain(queryMarker);
    expect(nextOutput).not.toContain(referrerMarker);
  }, 30_000);
});

// @vitest-environment node
import { randomUUID } from "node:crypto";
import { createServer, type ServerResponse } from "node:http";
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
type Deferred<T> = { promise: Promise<T>; resolve: (value: T) => void };
type SlowUpstreamRequest = {
  started: Deferred<void>;
  closedEarly: Deferred<boolean>;
  response?: ServerResponse;
  timer?: ReturnType<typeof setTimeout>;
};
const slowUpstreamRequests = new Map<string, SlowUpstreamRequest>();

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => (resolve = resolvePromise));
  return { promise, resolve };
}

function waitWithDeadline<T>(promise: Promise<T>, timeoutMs: number, message: string): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(message)), timeoutMs);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (error: unknown) => {
        clearTimeout(timer);
        reject(error);
      },
    );
  });
}

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

describe("search proxy privacy", () => {
  beforeAll(async () => {
    upstreamServer = createServer((request, response) => {
      upstreamRequests.push({
        url: request.url,
        cookie: request.headers.cookie,
        authorization: request.headers.authorization,
      });
      const cancellationId = new URL(request.url ?? "/", "http://localhost").searchParams.get(
        "abort_case",
      );
      const slowRequest = cancellationId ? slowUpstreamRequests.get(cancellationId) : undefined;
      if (slowRequest) {
        slowRequest.response = response;
        response.once("close", () => {
          if (slowRequest.timer) clearTimeout(slowRequest.timer);
          slowRequest.closedEarly.resolve(!response.writableEnded);
        });
        slowRequest.started.resolve();
        slowRequest.timer = setTimeout(() => {
          response.writeHead(200, { "content-type": "text/plain" });
          response.end("normal completion");
        }, 30_000);
        return;
      }
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

  it("preserves response and credentials for all search APIs", async () => {
    const paths = ["/api/search/suggestions", "/api/issues/search", "/api/projects/search"];
    for (const path of paths) {
      const query =
        path === "/api/search/suggestions"
          ? "workspace_slug=che1593-preview&q=SAFE_QUERY_MARKER_1593"
          : "q=SAFE_QUERY_MARKER_1593";
      const response = await fetch(`${appUrl}${path}?${query}`, {
        headers: {
          authorization: "Bearer synthetic-pat",
          cookie: "multica_session=synthetic-session",
        },
      });
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({ url: `${path}?${query}` });
    }

    expect(upstreamRequests).toEqual(
      paths.map((path) => ({
        url:
          path === "/api/search/suggestions"
            ? `${path}?workspace_slug=che1593-preview&q=SAFE_QUERY_MARKER_1593`
            : `${path}?q=SAFE_QUERY_MARKER_1593`,
        cookie: "multica_session=synthetic-session",
        authorization: "Bearer synthetic-pat",
      })),
    );
  }, 90_000);

  it.each(["/api/search/suggestions", "/api/issues/search", "/api/projects/search"])(
    "closes the upstream request after a client abort for %s",
    async (path) => {
      const cancellationId = randomUUID();
      const state: SlowUpstreamRequest = {
        started: deferred<void>(),
        closedEarly: deferred<boolean>(),
      };
      slowUpstreamRequests.set(cancellationId, state);
      const controller = new AbortController();
      const logOffset = nextOutput.length;
      const requestUrl = new URL(`${appUrl}${path}`);
      requestUrl.searchParams.set("q", "cancel-test");
      requestUrl.searchParams.set("abort_case", cancellationId);
      if (path === "/api/search/suggestions") {
        requestUrl.searchParams.set("workspace_slug", "che1593-preview");
      }
      const clientRequest = fetch(requestUrl, { signal: controller.signal });

      try {
        await waitWithDeadline(state.started.promise, 5_000, "The controlled upstream did not start");
        controller.abort();
        await expect(clientRequest).rejects.toMatchObject({ name: "AbortError" });
        await expect(
          waitWithDeadline(
            state.closedEarly.promise,
            2_000,
            "The upstream stayed open after the client aborted",
          ),
        ).resolves.toBe(true);
        await new Promise<void>((resolveImmediate) => setImmediate(resolveImmediate));
        expect(nextOutput.slice(logOffset)).not.toContain("Search proxy failed");
      } finally {
        controller.abort();
        if (state.timer) clearTimeout(state.timer);
        state.response?.destroy();
        slowUpstreamRequests.delete(cancellationId);
      }
    },
    15_000,
  );

  it("redacts failed palette and suggestions proxy logs but keeps unrelated diagnostics", async () => {
    await new Promise<void>((resolveClose, reject) => {
      upstreamServer.close((error) => (error ? reject(error) : resolveClose()));
    });
    upstreamClosed = true;

    const referrerMarker = "PALETTE_REFERRER_QUERY_MARKER_1593";
    const sensitiveRequests = [
      ["/api/search/suggestions", "SUGGESTION_QUERY_MARKER_1593"],
      ["/api/issues/search", "PALETTE_ISSUES_QUERY_MARKER_1593"],
      ["/api/projects/search", "PALETTE_PROJECTS_QUERY_MARKER_1593"],
    ] as const;
    for (const [path, queryMarker] of sensitiveRequests) {
      const failedResponse = await fetch(`${appUrl}${path}?q=${queryMarker}`, {
        headers: { referer: `${appUrl}/che1593-preview/search?q=${referrerMarker}` },
      });
      expect(failedResponse.status).toBe(502);
      expect(failedResponse.headers.get("cache-control")).toBe("no-store");
    }

    const unrelatedMarker = "UNRELATED_QUERY_MARKER_1593";
    const failedUnrelated = await fetch(`${appUrl}/health?q=${unrelatedMarker}`);
    expect(failedUnrelated.status).toBeGreaterThanOrEqual(500);
    expect(failedUnrelated.status).toBeLessThan(600);
    await waitForLog(unrelatedMarker);

    expect(nextOutput).toContain("ECONNREFUSED");
    expect(nextOutput).toContain(unrelatedMarker);
    const sensitiveMarkers = [referrerMarker, ...sensitiveRequests.map(([, marker]) => marker)];
    expect(sensitiveMarkers.filter((marker) => nextOutput.includes(marker))).toEqual([]);
  }, 90_000);
});

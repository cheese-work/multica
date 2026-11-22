import assert from "node:assert/strict";
import { createServer } from "node:http";
import test from "node:test";

import { assertLocalServer, createStubRuntime } from "./stub-runtime.mjs";

const codeOf = (fn) => {
  try {
    fn();
  } catch (e) {
    return e.code;
  }
  return undefined;
};
const codeOfAsync = async (fn) => {
  try {
    await fn();
  } catch (e) {
    return e.code;
  }
  return undefined;
};

// A fake daemon API on 127.0.0.1. It records every call and hands out one task per claim.
async function fakeServer() {
  const calls = [];
  const tasks = [];
  const server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      const parsed = body ? JSON.parse(body) : {};
      calls.push({ method: req.method, path: req.url, auth: req.headers.authorization, body: parsed });
      res.setHeader("content-type", "application/json");
      if (req.url === "/api/daemon/register") return res.end(JSON.stringify({ runtimes: [{ id: "rt-1" }] }));
      if (req.url === "/api/daemon/runtimes/rt-1/tasks/claim") {
        return res.end(JSON.stringify({ task: tasks.shift() ?? null }));
      }
      res.end(JSON.stringify({ status: "ok" }));
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const { port } = server.address();
  return {
    url: `http://127.0.0.1:${port}`,
    calls,
    queue: (id) => tasks.push({ id }),
    paths: () => calls.map((c) => `${c.method} ${c.path}`),
    close: () => new Promise((r) => server.close(r)),
  };
}

test("a server URL that is not 127.0.0.1 is refused (server-url-refused)", () => {
  for (const url of ["https://app.example.com", "http://localhost:8080", "http://10.0.0.5:8080", "http://127.0.0.1.evil.test", "not a url"]) {
    assert.equal(codeOf(() => assertLocalServer(url)), "server-url-refused", url);
  }
  assert.doesNotThrow(() => assertLocalServer("http://127.0.0.1:8080"));
  assert.equal(
    codeOf(() => createStubRuntime({ serverUrl: "https://app.example.com", token: "t", workspaceId: "w" })),
    "server-url-refused",
  );
});

test("a refused URL makes no network call", async () => {
  const srv = await fakeServer();
  try {
    assert.equal(codeOf(() => createStubRuntime({ serverUrl: srv.url.replace("127.0.0.1", "localhost"), token: "t", workspaceId: "w" })), "server-url-refused");
    assert.equal(srv.calls.length, 0);
  } finally {
    await srv.close();
  }
});

test("a run moves queued, running (3 streamed messages), completed; each marker is on the timeline", async () => {
  const srv = await fakeServer();
  try {
    const rt = createStubRuntime({ serverUrl: srv.url, token: "dev-token", workspaceId: "ws-1" });
    await rt.register();
    srv.queue("task-1");
    const task = await rt.claim();
    assert.equal(task.id, "task-1");
    assert.deepEqual(rt.timeline(task.id), ["queued"]);

    await rt.start(task.id);
    assert.deepEqual(rt.timeline(task.id), ["queued", "running"]);
    await rt.stream(task.id, "one");
    await rt.stream(task.id, "two");
    await rt.stream(task.id, "three");
    await rt.complete(task.id, "done");
    assert.deepEqual(rt.timeline(task.id), ["queued", "running", "message:1", "message:2", "message:3", "completed"]);

    const msgs = srv.calls.filter((c) => c.path === "/api/daemon/tasks/task-1/messages").map((c) => c.body.messages[0]);
    assert.deepEqual(msgs.map((m) => [m.seq, m.type, m.content]), [[1, "text", "one"], [2, "text", "two"], [3, "text", "three"]]);
    assert.ok(srv.calls.every((c) => c.auth === "Bearer dev-token"));
    assert.deepEqual(srv.paths().slice(0, 3), [
      "POST /api/daemon/register",
      "POST /api/daemon/runtimes/rt-1/tasks/claim",
      "POST /api/daemon/tasks/task-1/start",
    ]);
    assert.equal(srv.paths().at(-1), "POST /api/daemon/tasks/task-1/complete");
  } finally {
    await srv.close();
  }
});

test("a run can end failed", async () => {
  const srv = await fakeServer();
  try {
    const rt = createStubRuntime({ serverUrl: srv.url, token: "t", workspaceId: "ws-1" });
    await rt.register();
    srv.queue("task-2");
    const { id } = await rt.claim();
    await rt.start(id);
    await rt.fail(id, "stub failure");
    assert.deepEqual(rt.timeline(id), ["queued", "running", "failed"]);
    const last = srv.calls.at(-1);
    assert.equal(last.path, "/api/daemon/tasks/task-2/fail");
    assert.equal(last.body.error, "stub failure");
  } finally {
    await srv.close();
  }
});

test("a run can end cancelled; the stub acknowledges the cancel", async () => {
  const srv = await fakeServer();
  try {
    const rt = createStubRuntime({ serverUrl: srv.url, token: "t", workspaceId: "ws-1" });
    await rt.register();
    srv.queue("task-3");
    const { id } = await rt.claim();
    await rt.start(id);
    await rt.ackCancelled(id);
    assert.deepEqual(rt.timeline(id), ["queued", "running", "cancelled"]);
    assert.equal(srv.calls.at(-1).path, "/api/daemon/tasks/task-3/cancel-ack");
  } finally {
    await srv.close();
  }
});

test("one step per signal: a step out of order is refused and sends nothing", async () => {
  const srv = await fakeServer();
  try {
    const rt = createStubRuntime({ serverUrl: srv.url, token: "t", workspaceId: "ws-1" });
    await rt.register();
    srv.queue("task-4");
    const { id } = await rt.claim();
    const before = srv.calls.length;
    assert.equal(await codeOfAsync(() => rt.stream(id, "early")), "invalid-transition");
    assert.equal(await codeOfAsync(() => rt.complete(id, "early")), "invalid-transition");
    await rt.start(id);
    await rt.complete(id, "ok");
    assert.equal(await codeOfAsync(() => rt.fail(id, "late")), "invalid-transition");
    assert.equal(await codeOfAsync(() => rt.start("unknown-task")), "unknown-task");
    assert.equal(srv.calls.length, before + 2);
  } finally {
    await srv.close();
  }
});

test("claim returns null and records nothing when no task is queued", async () => {
  const srv = await fakeServer();
  try {
    const rt = createStubRuntime({ serverUrl: srv.url, token: "t", workspaceId: "ws-1" });
    await rt.register();
    assert.equal(await rt.claim(), null);
  } finally {
    await srv.close();
  }
});

test("claim before register is refused", async () => {
  const rt = createStubRuntime({ serverUrl: "http://127.0.0.1:1", token: "t", workspaceId: "ws-1" });
  assert.equal(await codeOfAsync(() => rt.claim()), "not-registered");
});

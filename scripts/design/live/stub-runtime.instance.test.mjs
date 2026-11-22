// Runs the stub runtime against a real local instance. Skipped unless MULTICA_STUB_SERVER_URL is set
// to a 127.0.0.1 URL of a dev environment (make up C=api): it logs in with the dev code, one fresh
// email per run, because the send-code limit is per email.
//   MULTICA_STUB_SERVER_URL=http://127.0.0.1:<api port> node --test scripts/design/live/stub-runtime.instance.test.mjs
import assert from "node:assert/strict";
import test from "node:test";

import { createStubRuntime } from "./stub-runtime.mjs";

const SERVER = process.env.MULTICA_STUB_SERVER_URL;
const json = { "content-type": "application/json" };

async function api(token, workspaceId, method, path, body) {
  const res = await fetch(`${SERVER}${path}`, {
    method,
    headers: { ...json, authorization: `Bearer ${token}`, ...(workspaceId ? { "x-workspace-id": workspaceId } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  assert.ok(res.ok, `${method} ${path} -> ${res.status}`);
  return res.json();
}

async function bootstrap() {
  const email = process.env.MULTICA_STUB_EMAIL ?? `stub-${Date.now().toString(36)}@localhost`;
  await fetch(`${SERVER}/auth/send-code`, { method: "POST", headers: json, body: JSON.stringify({ email }) });
  const login = await (
    await fetch(`${SERVER}/auth/verify-code`, { method: "POST", headers: json, body: JSON.stringify({ email, code: process.env.MULTICA_STUB_CODE ?? "888888" }) })
  ).json();
  const token = login.token;
  const slug = `stub-${Date.now().toString(36)}`;
  const ws = await api(token, null, "POST", "/api/workspaces", { name: "Stub runtime", slug });
  const rt = createStubRuntime({ serverUrl: SERVER, token, workspaceId: ws.id });
  const runtimeId = await rt.register();
  const agent = await api(token, ws.id, "POST", "/api/agents", { name: "stub-agent", runtime_id: runtimeId, visibility: "workspace" });
  // One issue per run: each assignment queues one task.
  const queueRun = async (title) => {
    await api(token, ws.id, "POST", "/api/issues", { title, assignee_type: "agent", assignee_id: agent.id });
    for (let i = 0; i < 20; i++) {
      const task = await rt.claim();
      if (task) return task;
      await new Promise((r) => setTimeout(r, 250));
    }
    throw new Error(`no task queued for ${title}`);
  };
  return { rt, queueRun, token, ws };
}

test("a real local instance: completed, failed and cancelled runs", { skip: SERVER ? false : "MULTICA_STUB_SERVER_URL not set", timeout: 60_000 }, async () => {
  const { rt, queueRun, token, ws } = await bootstrap();
  const messages = (id) => api(token, ws.id, "GET", `/api/daemon/tasks/${id}/messages`).then((r) => (Array.isArray(r) ? r : (r.messages ?? [])));

  // Run 1: queued -> running (3 streamed messages) -> completed.
  const one = await queueRun("run one");
  assert.equal(await rt.status(one.id), "dispatched");
  await rt.start(one.id);
  assert.equal(await rt.status(one.id), "running");
  for (const t of ["first", "second", "third"]) await rt.stream(one.id, t);
  assert.deepEqual((await messages(one.id)).map((m) => m.content), ["first", "second", "third"]);
  await rt.complete(one.id, "done");
  assert.equal(await rt.status(one.id), "completed");
  assert.deepEqual(rt.timeline(one.id), ["queued", "running", "message:1", "message:2", "message:3", "completed"]);

  // Run 2: failed.
  const two = await queueRun("run two");
  await rt.start(two.id);
  await rt.fail(two.id, "stub failure");
  assert.equal(await rt.status(two.id), "failed");
  assert.deepEqual(rt.timeline(two.id), ["queued", "running", "failed"]);

  // Run 3: cancelled by the user, then acknowledged with the claim proof.
  const three = await queueRun("run three");
  await rt.start(three.id);
  await rt.cancel(three.id);
  assert.equal(await rt.status(three.id), "cancelled");
  await rt.ackCancelled(three.id);
  assert.equal(await rt.status(three.id), "cancelled");
  assert.deepEqual(rt.timeline(three.id), ["queued", "running", "cancelled", "cancel-acked"]);
});

/**
 * Stub runtime for the Live record (CHE-6753 A1, CHE-5793 C9: a repo fixture that counts as seed).
 *
 * It speaks the daemon API of a local instance with a dev token and moves runs through
 * queued, running (streamed messages), completed, failed and cancelled. It never advances on
 * its own: each call is one step, so a test signal decides the state and runs are deterministic.
 */

const fail = (code, message) => Object.assign(new Error(`${code}: ${message}`), { code });

// Only a literal 127.0.0.1 host is local enough: "localhost" can resolve elsewhere.
export function assertLocalServer(serverUrl) {
  let url;
  try {
    url = new URL(serverUrl);
  } catch {
    throw fail("server-url-refused", `not a URL: ${serverUrl}`);
  }
  if (!["http:", "https:"].includes(url.protocol) || url.hostname !== "127.0.0.1") {
    throw fail("server-url-refused", `the stub runtime talks to 127.0.0.1 only, got ${url.hostname}`);
  }
  return url.origin;
}

// What each step needs the run to be in, and what it leaves behind.
const NEXT = {
  start: { from: ["queued"], to: "running" },
  stream: { from: ["running"], to: "running" },
  complete: { from: ["running"], to: "completed" },
  fail: { from: ["running"], to: "failed" },
  // A user cancel is what ends the run on the server; the ack only confirms the daemon saw it.
  cancel: { from: ["queued", "running"], to: "cancelled" },
  ackCancelled: { from: ["cancelled"], to: "cancel-acked" },
};

export function createStubRuntime({ serverUrl, token, workspaceId, deviceName = "live-stub-runtime", fetchImpl = fetch }) {
  const origin = assertLocalServer(serverUrl);
  let runtimeId = null;
  const runs = new Map();

  const call = async (method, path, body) => {
    const res = await fetchImpl(`${origin}${path}`, {
      method,
      headers: { authorization: `Bearer ${token}`, "x-workspace-id": workspaceId, "content-type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) throw fail("request-failed", `${method} ${path} -> ${res.status}`);
    return res.json();
  };
  const post = (path, body) => call("POST", path, body);

  // Check the transition before any call, so a refused step sends nothing.
  const step = async (taskId, name, send, marker) => {
    const run = runs.get(taskId);
    if (!run) throw fail("unknown-task", taskId);
    const rule = NEXT[name];
    if (!rule.from.includes(run.state)) throw fail("invalid-transition", `${name} on a ${run.state} run`);
    await send();
    run.state = rule.to;
    run.timeline.push(marker ?? rule.to);
  };

  return {
    async register() {
      const res = await post("/api/daemon/register", {
        workspace_id: workspaceId,
        daemon_id: deviceName,
        device_name: deviceName,
        cli_version: "stub",
        runtimes: [{ name: deviceName, type: "stub", version: "stub", status: "online" }],
      });
      runtimeId = res.runtimes?.[0]?.id;
      if (!runtimeId) throw fail("register-failed", "no runtime id in the response");
      return runtimeId;
    },

    async claim() {
      if (!runtimeId) throw fail("not-registered", "call register() first");
      const { task } = await post(`/api/daemon/runtimes/${runtimeId}/tasks/claim`, {});
      if (!task) return null;
      runs.set(task.id, { state: "queued", seq: 0, timeline: ["queued"], issueId: task.issue_id, proof: task.cancel_ack_proof });
      return task;
    },

    start: (id) => step(id, "start", () => post(`/api/daemon/tasks/${id}/start`, {})),

    stream(id, text) {
      const run = runs.get(id);
      const seq = (run?.seq ?? 0) + 1;
      return step(id, "stream", () => post(`/api/daemon/tasks/${id}/messages`, { messages: [{ seq, type: "text", content: text }] }), `message:${seq}`).then(() => {
        run.seq = seq;
      });
    },

    complete: (id, output) => step(id, "complete", () => post(`/api/daemon/tasks/${id}/complete`, { output })),
    fail: (id, error) => step(id, "fail", () => post(`/api/daemon/tasks/${id}/fail`, { error })),
    // The same user action the UI sends; the server then holds the run as cancelled.
    cancel(id) {
      const issueId = runs.get(id)?.issueId;
      const path = issueId ? `/api/issues/${issueId}/tasks/${id}/cancel` : `/api/tasks/${id}/cancel`;
      return step(id, "cancel", () => post(path, {}));
    },
    ackCancelled: (id) => step(id, "ackCancelled", () => post(`/api/daemon/tasks/${id}/cancel-ack`, { cancel_ack_proof: runs.get(id).proof }), "cancel-acked"),

    // The server's view of the run, not the stub's.
    status: async (id) => (await call("GET", `/api/daemon/tasks/${id}/status`)).status,

    timeline: (id) => [...(runs.get(id)?.timeline ?? [])],
  };
}

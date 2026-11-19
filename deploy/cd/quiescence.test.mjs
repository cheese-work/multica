// Unit tests for the passive preflight's handling of advisory locks (CHE-7008).
import test from "node:test";
import assert from "node:assert/strict";
import {
  evaluatePassivePreflight,
  advisoryLockHolderKeys,
  runPassivePreflight,
  evaluateFinalGate,
} from "./quiescence.mjs";

const NOW = Date.parse("2026-11-17T15:50:35Z");

function activity(pid, overrides = {}) {
  return {
    pid, backendStart: "2026-11-17 15:00:00+00", datname: "multica", usename: "multica",
    backendType: "client backend", xactStart: new Date(NOW - 50).toISOString(),
    state: "active", backendXid: null, backendXmin: null, waitEventType: null, waitEvent: null,
    query: "SELECT pg_advisory_xact_lock(1)", applicationName: "server", ...overrides,
  };
}
const lock = (pid) => ({ pid, datname: "multica", mode: "ExclusiveLock", detail: "advisory::true" });
function state(pids, activityOverrides = {}) {
  return {
    ok: true, observedAtMs: NOW, observationEndMs: NOW,
    activity: pids.map((p) => activity(p, activityOverrides)),
    locks: pids.map(lock), prepared: [], createIndex: [],
  };
}

test("a holder that persists across the whole window denies with diagnostic evidence", () => {
  const s = state(["10"]);
  const r = evaluatePassivePreflight(s, { persistentHolderKeys: advisoryLockHolderKeys(s) });
  assert.equal(r.admit, false);
  assert.equal(r.reason, "a foreign advisory-lock holder is present");
  const h = r.evidence.advisoryLockHolders[0];
  assert.equal(h.pid, "10");
  assert.equal(h.state, "active");
  assert.ok(h.xactStart && h.backendStart && h.applicationName && h.backendType);
  assert.equal(h.query, "SELECT pg_advisory_xact_lock(1)");
});

test("evidence truncates the query to 120 characters", () => {
  const s = state(["10"], { query: "x".repeat(500) });
  const r = evaluatePassivePreflight(s, { persistentHolderKeys: advisoryLockHolderKeys(s) });
  assert.equal(r.evidence.advisoryLockHolders[0].query.length, 120);
});

test("a holder missing from the persistent set is transient and admits", () => {
  const s = state(["10"]);
  const r = evaluatePassivePreflight(s, { persistentHolderKeys: new Set() });
  assert.equal(r.admit, true);
});

test("runPassivePreflight admits when a short xact-lock holder is gone on re-sample", () => {
  const samples = [state(["10"]), state([])];
  let t = NOW;
  const r = runPassivePreflight({
    observe: () => samples.shift(), now: () => t, sleep: (ms) => { t += ms; },
  });
  assert.equal(r.admit, true);
});

test("runPassivePreflight admits when each sample sees a different transient holder", () => {
  const samples = [state(["10"]), state(["11"]), state([])];
  let t = NOW;
  const r = runPassivePreflight({ observe: () => samples.shift(), now: () => t, sleep: (ms) => { t += ms; } });
  assert.equal(r.admit, true);
});

test("runPassivePreflight denies a holder still present after the 15s threshold", () => {
  let t = NOW;
  const r = runPassivePreflight({
    observe: () => ({ ...state(["10"], { xactStart: null, state: "idle" }), observedAtMs: t }),
    now: () => t, sleep: (ms) => { t += ms; },
  });
  assert.equal(r.admit, false);
  assert.equal(r.reason, "a foreign advisory-lock holder is present");
  assert.ok(t - NOW >= 15000);
});

test("a same-pid holder with a different backend_start is a different holder", () => {
  const a = state(["10"]);
  const b = state(["10"], { backendStart: "2026-11-17 15:50:36+00" });
  const r = evaluatePassivePreflight(b, { persistentHolderKeys: advisoryLockHolderKeys(a) });
  assert.equal(r.admit, true);
});

test("runPassivePreflight denies persistent holder B that replaces transient holder A", () => {
  let t = NOW;
  const r = runPassivePreflight({
    observe: () => {
      // A (pid 10) is gone after the first sample; idle session holder B (pid 11) stays.
      const pid = t === NOW ? "10" : "11";
      return { ...state([pid], { xactStart: null, state: "idle" }), observedAtMs: t };
    },
    now: () => t, sleep: (ms) => { t += ms; },
  });
  assert.equal(r.admit, false);
  assert.equal(r.reason, "a foreign advisory-lock holder is present");
  assert.equal(r.evidence.advisoryLockHolders[0].pid, "11");
});

test("runPassivePreflight denies when new transient holders keep appearing past the window", () => {
  let t = NOW;
  let n = 0;
  const r = runPassivePreflight({
    observe: () => ({ ...state([String(100 + n++)], { xactStart: null, state: "idle" }), observedAtMs: t }),
    now: () => t, sleep: (ms) => { t += ms; },
  });
  assert.equal(r.admit, false);
  assert.equal(r.reason, "a foreign advisory-lock holder is present");
});

// CHE-7425: Postgres parallel workers belong to a normal query that went
// parallel on the still-serving slot. They are short-lived and take the same
// transient-holder rule as advisory locks.
function stateWithWorkers(pids, overrides = {}) {
  return {
    ok: true, observedAtMs: NOW, observationEndMs: NOW,
    activity: pids.map((p) => activity(p, {
      backendType: "parallel worker", state: "", query: "", applicationName: "", ...overrides,
    })),
    locks: [], prepared: [], createIndex: [],
  };
}

test("a parallel worker missing from the persistent set is transient and admits", () => {
  const r = evaluatePassivePreflight(stateWithWorkers(["20", "21"]), { persistentHolderKeys: new Set() });
  assert.equal(r.admit, true);
});

test("without a persistent set a parallel worker still denies (fail closed)", () => {
  const r = evaluatePassivePreflight(stateWithWorkers(["20"]));
  assert.equal(r.admit, false);
  assert.equal(r.reason, "unknown/unfenceable consumer backend type present");
});

test("runPassivePreflight admits when parallel workers are gone on re-sample", () => {
  const samples = [stateWithWorkers(["20", "21"]), state([])];
  let t = NOW;
  const r = runPassivePreflight({ observe: () => samples.shift(), now: () => t, sleep: (ms) => { t += ms; } });
  assert.equal(r.admit, true);
});

test("runPassivePreflight denies a parallel worker still present after the 15s threshold", () => {
  let t = NOW;
  const r = runPassivePreflight({
    observe: () => ({ ...stateWithWorkers(["20"], { xactStart: null }), observedAtMs: t }),
    now: () => t, sleep: (ms) => { t += ms; },
  });
  assert.equal(r.admit, false);
  assert.equal(r.reason, "unknown/unfenceable consumer backend type present");
  assert.ok(t - NOW >= 15000);
});

test("runPassivePreflight denies a persistent unknown backend type that replaces a transient worker", () => {
  let t = NOW;
  const r = runPassivePreflight({
    observe: () => {
      const s = t === NOW ? stateWithWorkers(["20"]) : stateWithWorkers(["30"], { backendType: "logical replication launcher", xactStart: null });
      return { ...s, observedAtMs: t };
    },
    now: () => t, sleep: (ms) => { t += ms; },
  });
  assert.equal(r.admit, false);
  assert.equal(r.evidence.unknownBackendTypes[0].pid, "30");
});

test("unknown backend evidence keeps the CHE-7008 diagnostic fields and truncates the query", () => {
  const s = stateWithWorkers(["20"], { query: "x".repeat(500) });
  const r = evaluatePassivePreflight(s, { persistentHolderKeys: new Set(["20|2026-11-17 15:00:00+00"]) });
  const h = r.evidence.unknownBackendTypes[0];
  assert.equal(h.backendType, "parallel worker");
  assert.ok(h.backendStart && h.xactStart);
  assert.equal(h.state, "");
  assert.equal(h.query.length, 120);
});

test("a same-pid parallel worker with a different backend_start is a different holder", () => {
  const a = stateWithWorkers(["20"]);
  const b = stateWithWorkers(["20"], { backendStart: "2026-11-17 15:50:36+00" });
  const r = evaluatePassivePreflight(b, { persistentHolderKeys: new Set(["20|" + a.activity[0].backendStart]) });
  assert.equal(r.admit, true);
});

test("the final gate still denies a parallel worker", () => {
  const r = evaluateFinalGate(stateWithWorkers(["20"]));
  assert.equal(r.admit, false);
});

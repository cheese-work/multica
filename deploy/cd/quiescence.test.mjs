// Unit tests for the passive preflight's handling of advisory locks (CHE-7008).
import test from "node:test";
import assert from "node:assert/strict";
import {
  evaluatePassivePreflight,
  advisoryLockHolderKeys,
  runPassivePreflight,
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
  const r = evaluatePassivePreflight(s, { persistentAdvisoryKeys: advisoryLockHolderKeys(s) });
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
  const r = evaluatePassivePreflight(s, { persistentAdvisoryKeys: advisoryLockHolderKeys(s) });
  assert.equal(r.evidence.advisoryLockHolders[0].query.length, 120);
});

test("a holder missing from the persistent set is transient and admits", () => {
  const s = state(["10"]);
  const r = evaluatePassivePreflight(s, { persistentAdvisoryKeys: new Set() });
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
  const r = evaluatePassivePreflight(b, { persistentAdvisoryKeys: advisoryLockHolderKeys(a) });
  assert.equal(r.admit, true);
});

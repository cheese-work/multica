#!/usr/bin/env node
// Durable post-deploy health journal (CHE-5175). Runs ON C00.
//
//   arm    CD readback: record T0, sample slot 0, and require a sample from the
//          native systemd timer before the readback job is released.
//   tick   multica-health-observer.timer, every 5 s: take the sample for the
//          current 10 s slot of the armed observation. Holds no CI runner and
//          no agent run.
//   verify CD confirm: the journal must hold one healthy sample for every
//          scheduled slot T0..T0+window, or the deploy rolls back.
//
// A failed sample stays a failure after recovery. Missing, truncated, corrupt,
// stale or foreign evidence fails closed. A newer arm supersedes the open
// journal; journals are never mixed. Confirm leaves a newer descendant deploy
// to that deploy's own journal before it calls verify.
//
// The readback runs outside the deploy lock, so a delayed older readback can
// reach arm after a newer deploy. Under the tick lock, arm reads the commit C00
// runs now (--live-sha-script) and refuses to touch the journals unless it is
// the commit being armed: exit 3, nothing created, `active` untouched. The same
// lock covers the journal switch, so an arm cannot interleave with another.
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SHA_RE = /^[0-9a-f]{40}$/;
const ID_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const LOCK_STALE_SECONDS = 120;
const HEALTH_TIMEOUT_MS = 60_000;

const now = () => (process.env.HW_NOW ? Number(process.env.HW_NOW) : Math.floor(Date.now() / 1000));
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

class UsageError extends Error {}
// A newer deploy owns C00 now: this arm must leave its journal alone (exit 3).
class StaleDeployError extends Error {}
const EXIT_STALE_DEPLOY = 3;

function parseArgs(argv) {
  const opts = {};
  for (let i = 0; i < argv.length; i += 2) {
    if (!argv[i].startsWith("--") || argv[i + 1] === undefined) throw new UsageError(`bad argument ${argv[i]}`);
    opts[argv[i].slice(2)] = argv[i + 1];
  }
  return opts;
}

function need(opts, ...names) {
  for (const name of names) if (!opts[name]) throw new UsageError(`--${name} is required`);
}

function posInt(value, name) {
  if (!/^[0-9]+$/.test(String(value)) || Number(value) < 1) throw new UsageError(`--${name} must be a positive integer`);
  return Number(value);
}

function writeFileAtomic(file, text) {
  const tmp = `${file}.${process.pid}.tmp`;
  const fd = fs.openSync(tmp, "w");
  try {
    fs.writeSync(fd, text);
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
  fs.renameSync(tmp, file);
}

function appendDurable(file, text) {
  const fd = fs.openSync(file, "a");
  try {
    fs.writeSync(fd, text);
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
}

async function withLock(dir, waitMs, fn) {
  const lock = path.join(dir, "tick.lock");
  const deadline = Date.now() + waitMs;
  for (;;) {
    try {
      fs.mkdirSync(lock);
      break;
    } catch (error) {
      if (error.code !== "EEXIST") throw error;
      const age = Date.now() / 1000 - fs.statSync(lock).mtimeMs / 1000;
      if (age > LOCK_STALE_SECONDS) {
        fs.rmSync(lock, { recursive: true, force: true });
        continue;
      }
      if (Date.now() >= deadline) return null;
      await sleep(100);
    }
  }
  try {
    return await fn();
  } finally {
    fs.rmSync(lock, { recursive: true, force: true });
  }
}

function readJson(file) {
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

function activeColour(stateDir) {
  try {
    return String(readJson(path.join(stateDir, "cutover-state.json")).active_colour || "");
  } catch {
    return "";
  }
}

function containersOf(composeDir, colour) {
  // The colour services exist only in the A/B overlay; without it Compose
  // answers "no such service: backend-blue".
  const files = ["docker-compose.selfhost.yml", "deploy/cd/docker-compose.ab.yml"].flatMap((file) => ["-f", file]);
  const result = spawnSync("docker", ["compose", ...files, "ps", "-q", `backend-${colour}`, `frontend-${colour}`], {
    cwd: composeDir,
    encoding: "utf8",
    timeout: 30_000,
  });
  if (result.status !== 0) return [];
  return result.stdout.split("\n").map((line) => line.trim()).filter(Boolean).sort();
}

function readSamples(file) {
  const text = fs.readFileSync(file, "utf8");
  if (text !== "" && !text.endsWith("\n")) throw new Error("journal is truncated: last line has no newline");
  return text.split("\n").filter(Boolean).map((line, index) => {
    try {
      return JSON.parse(line);
    } catch {
      throw new Error(`journal line ${index + 1} is not valid JSON`);
    }
  });
}

function runHealth(od, meta) {
  const result = spawnSync(
    "bash",
    [path.join(od, "ab-health-check.sh"), "--window", "0", "--router-state-dir", meta.router_state_dir, "--cutover-state-dir", meta.cutover_state_dir],
    { encoding: "utf8", timeout: HEALTH_TIMEOUT_MS },
  );
  if (result.status === 0) return { ok: true, detail: "" };
  const detail = `${result.stdout || ""}${result.stderr || ""}`.replace(/\s+/g, " ").trim().slice(0, 400);
  return { ok: false, detail: detail || `health check exited ${result.status ?? result.signal}` };
}

function setState(od, state) {
  writeFileAtomic(path.join(od, "state.json"), `${JSON.stringify({ state, at: now() })}\n`);
}

function stateOf(od) {
  try {
    return readJson(path.join(od, "state.json")).state;
  } catch {
    return "open";
  }
}

// One sample for the slot that `at` falls in. Returns the row, or null when
// there is nothing to do (journal closed, slot already sampled, window over).
function sampleOnce(od, source, forcedSlot) {
  const meta = readJson(path.join(od, "meta.json"));
  if (stateOf(od) !== "open") return null;
  const start = now();
  const slot = forcedSlot ?? Math.floor((start - meta.t0) / meta.interval);
  const last = meta.window / meta.interval;
  if (start < meta.t0 && forcedSlot === undefined) return null;
  if (slot > last) {
    setState(od, "complete");
    return null;
  }
  const journal = path.join(od, "samples.jsonl");
  if (readSamples(journal).some((row) => row.slot === slot)) return null;

  const base = { slot, scheduled: meta.t0 + slot * meta.interval, start, source, invocation: source === "timer" ? process.env.INVOCATION_ID : undefined };
  const colour = activeColour(meta.cutover_state_dir);
  const containers = colour === meta.colour ? containersOf(meta.compose_dir, meta.colour) : [];
  let row;
  if (colour !== meta.colour && colour !== "") {
    row = { ...base, kind: "identity", colour, containers: [], end: now() };
    appendDurable(journal, `${JSON.stringify(row)}\n`);
    setState(od, "replaced");
    return row;
  }
  if (containers.length === meta.containers.length && containers.join() !== meta.containers.join()) {
    row = { ...base, kind: "identity", colour: meta.colour, containers, end: now() };
    appendDurable(journal, `${JSON.stringify(row)}\n`);
    setState(od, "replaced");
    return row;
  }
  if (containers.length !== meta.containers.length) {
    row = { ...base, kind: "sample", ok: false, detail: `expected ${meta.containers.length} backend/frontend containers, found ${containers.length}`, colour: meta.colour, containers, end: now() };
  } else {
    const health = runHealth(od, meta);
    row = { ...base, kind: "sample", ...health, colour: meta.colour, containers, end: now() };
  }
  appendDurable(journal, `${JSON.stringify(row)}\n`);
  if (slot === last) setState(od, "complete");
  return row;
}

async function tick(opts) {
  need(opts, "dir");
  const activeFile = path.join(opts.dir, "active");
  if (!fs.existsSync(activeFile)) return 0;
  const od = path.join(opts.dir, fs.readFileSync(activeFile, "utf8").trim());
  if (!fs.existsSync(path.join(od, "meta.json"))) return 0;
  const source = opts.source === "timer" && process.env.INVOCATION_ID ? "timer" : "manual";
  await withLock(opts.dir, 1_000, () => sampleOnce(od, source));
  return 0;
}

function assertStillLive(opts) {
  const result = spawnSync("bash", [opts["live-sha-script"]], {
    encoding: "utf8",
    timeout: 60_000,
    env: { ...process.env, C00_COMPOSE_DIR: opts["compose-dir"], C00_CUTOVER_STATE_DIR: opts["cutover-state-dir"] },
  });
  const live = result.status === 0 ? result.stdout.trim() : "";
  if (!SHA_RE.test(live)) throw new Error(`cannot read the commit C00 runs now (${opts["live-sha-script"]}); not arming`);
  if (live !== opts.sha) throw new StaleDeployError(`C00 no longer runs ${opts.sha} (it runs ${live}); not arming ${opts.id}`);
}

async function arm(opts) {
  need(opts, "dir", "id", "sha", "window", "interval", "compose-dir", "cutover-state-dir", "router-state-dir", "health-script", "live-sha-script");
  if (!ID_RE.test(opts.id)) throw new UsageError("--id is not a safe name");
  if (!SHA_RE.test(opts.sha)) throw new UsageError("--sha must be a 40-character commit SHA");
  const window = posInt(opts.window, "window");
  const interval = posInt(opts.interval, "interval");
  if (window % interval !== 0) throw new UsageError("--window must be a multiple of --interval");
  const verifySeconds = Number(opts["verify-seconds"] ?? 0);

  const od = path.join(opts.dir, opts.id);
  if (fs.existsSync(od)) throw new Error(`observation ${opts.id} already exists`);
  const colour = activeColour(opts["cutover-state-dir"]);
  if (!colour) throw new Error("no active colour in cutover-state.json");
  const containers = containersOf(opts["compose-dir"], colour);
  if (containers.length !== 2) throw new Error(`expected 2 backend/frontend containers for ${colour}, found ${containers.length}`);

  fs.mkdirSync(opts.dir, { recursive: true });
  const first = await withLock(opts.dir, 30_000, async () => {
    assertStillLive(opts);
    // T0 starts here, not before the lock wait and the live-commit read: those can take longer
    // than one interval, which would put slot 0 outside its schedule.
    const t0 = now();
    const meta = {
      id: opts.id, sha: opts.sha, t0, window, interval, colour, containers,
      compose_dir: path.resolve(opts["compose-dir"]),
      cutover_state_dir: path.resolve(opts["cutover-state-dir"]),
      router_state_dir: path.resolve(opts["router-state-dir"]),
    };
    fs.mkdirSync(path.join(opts.dir, "bin"), { recursive: true });
    fs.mkdirSync(od);
    fs.copyFileSync(opts["health-script"], path.join(od, "ab-health-check.sh"));
    fs.copyFileSync(fileURLToPath(import.meta.url), path.join(opts.dir, "bin/health-window.mjs.new"));
    fs.renameSync(path.join(opts.dir, "bin/health-window.mjs.new"), path.join(opts.dir, "bin/health-window.mjs"));
    writeFileAtomic(path.join(od, "meta.json"), `${JSON.stringify(meta)}\n`);
    writeFileAtomic(path.join(od, "samples.jsonl"), "");
    setState(od, "open");

    const activeFile = path.join(opts.dir, "active");
    if (fs.existsSync(activeFile)) {
      const previous = path.join(opts.dir, fs.readFileSync(activeFile, "utf8").trim());
      if (fs.existsSync(path.join(previous, "meta.json")) && stateOf(previous) === "open") setState(previous, "superseded");
    }
    writeFileAtomic(activeFile, `${opts.id}\n`);
    if (process.env.GITHUB_OUTPUT) fs.appendFileSync(process.env.GITHUB_OUTPUT, `observation=${opts.id}\nt0=${t0}\n`);
    return sampleOnce(od, "arm", 0);
  });
  if (!first) throw new Error("could not take the first sample");
  if (!first.ok) throw new Error(`first sample is unhealthy: ${first.detail}`);

  const deadline = Date.now() + verifySeconds * 1000;
  for (;;) {
    const rows = readSamples(path.join(od, "samples.jsonl"));
    const failed = rows.find((row) => row.kind !== "sample" || row.ok === false);
    if (failed) throw new Error(`observation sample failed: ${failed.detail || failed.kind}`);
    if (rows.some((row) => row.source === "timer" && row.invocation)) return 0;
    if (Date.now() >= deadline) break;
    await sleep(500);
  }
  throw new Error(`no sample from the native timer (multica-health-observer.timer) within ${verifySeconds}s: it is not installed or not running`);
}

function verify(opts) {
  need(opts, "dir", "id", "sha");
  const od = path.join(opts.dir, opts.id);
  const problems = [];
  let meta;
  let rows;
  try {
    meta = readJson(path.join(od, "meta.json"));
    rows = readSamples(path.join(od, "samples.jsonl"));
  } catch (error) {
    throw new Error(`journal ${opts.id} is unreadable: ${error.message}`);
  }
  if (meta.id !== opts.id) throw new Error(`journal id ${meta.id} is not ${opts.id}`);
  if (meta.sha !== opts.sha) throw new Error(`journal was armed for commit ${meta.sha}, not ${opts.sha}`);
  const state = stateOf(od);
  if (state === "superseded") throw new Error(`journal ${opts.id} was superseded by a newer deploy; it needs its own observation`);
  if (now() < meta.t0 + meta.window) throw new Error(`window has not ended: ${meta.t0 + meta.window - now()}s left`);

  const last = meta.window / meta.interval;
  const identity = rows.find((row) => row.kind === "identity");
  if (state === "replaced" || identity) {
    throw new Error(`containers were replaced at slot ${identity?.slot ?? "?"}: ${opts.sha} was not the commit under observation`);
  }
  const through = last;
  for (let slot = 0; slot <= through; slot += 1) {
    const found = rows.filter((row) => row.kind === "sample" && row.slot === slot);
    if (found.length === 0) {
      problems.push(`slot ${slot}: sample missing`);
      continue;
    }
    if (found.length > 1) problems.push(`slot ${slot}: ${found.length} samples`);
    const row = found[0];
    const scheduled = meta.t0 + slot * meta.interval;
    if (row.scheduled !== scheduled || row.start < scheduled || row.start >= scheduled + meta.interval) {
      problems.push(`slot ${slot}: sample is not in its scheduled slot`);
    }
    const native = row.source === "timer" && row.invocation;
    if (!native && !(slot === 0 && row.source === "arm")) {
      problems.push(`slot ${slot}: sample source is ${row.source}, not the native timer`);
    }
    if (row.ok !== true) problems.push(`slot ${slot}: failed: ${row.detail || "no detail"}`);
    else if (row.colour !== meta.colour || (row.containers || []).join() !== meta.containers.join()) problems.push(`slot ${slot}: identity differs from the armed deploy`);
  }
  if (problems.length > 0) {
    throw new Error(`${problems.length} problem(s) in journal ${opts.id}:\n${problems.slice(0, 8).join("\n")}`);
  }
  console.log(`==> health journal ok: ${rows.filter((r) => r.kind === "sample").length} samples, slots 0..${through} (${meta.interval}s) healthy for ${opts.id}`);
  return 0;
}

async function main() {
  const [command, ...rest] = process.argv.slice(2);
  try {
    const opts = parseArgs(rest);
    if (command === "arm") return await arm(opts);
    if (command === "tick") return await tick(opts);
    if (command === "verify") return verify(opts);
    throw new UsageError(`usage: health-window.mjs arm|tick|verify ...`);
  } catch (error) {
    console.error(`::error title=Multica health journal::${error.message}`);
    if (error instanceof StaleDeployError) return EXIT_STALE_DEPLOY;
    return error instanceof UsageError ? 2 : 1;
  }
}

process.exit(await main());

// Behavioural tests for launch.sh and cleanup.sh ownership (CHE-6602, review P1):
// cleanup may remove only an environment this run launched, and may stop only
// processes this run started. A fake `make` stands in for the real stack.
// Run: node --test .agents/skills/verify-multica/scripts/ownership.test.mjs
import { strict as assert } from "node:assert";
import { spawn, spawnSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync, chmodSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

// A throwaway checkout holding a copy of the scripts, a fake registry and a fake `make`.
function sandbox() {
  const dir = mkdtempSync(join(tmpdir(), "vm-own-"));
  const root = join(dir, "checkout");
  const scripts = join(root, ".agents/skills/verify-multica/scripts");
  mkdirSync(scripts, { recursive: true });
  for (const f of ["launch.sh", "cleanup.sh", "stop-pid.sh", "ownership.sh"]) {
    if (existsSync(join(here, f))) cpSync(join(here, f), join(scripts, f));
  }
  const envs = join(dir, "envs");
  const state = join(dir, "state");
  const bin = join(dir, "bin");
  const calls = join(dir, "make-calls.log");
  for (const d of [envs, state, bin]) mkdirSync(d, { recursive: true });
  writeFileSync(calls, "");
  // Fake make: `up` registers an agent environment for this checkout, `destroy` removes it.
  writeFileSync(
    join(bin, "make"),
    `#!/usr/bin/env bash
echo "$*" >> "${calls}"
name=checkout-77
case "$1" in
  up)
    case "$2$3" in *--ephemeral*) owner=agent;; *) owner=human;; esac
    [ -d "${envs}/$name" ] && exit 0
    mkdir -p "${envs}/$name"
    { printf 'NAME=%q\\n' $name; printf 'DIR=%q\\n' "${root}"; printf 'OWNER=%q\\n' $owner
      printf 'CREATED_AT=%q\\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"; printf 'BACKEND_PORT=59991\\nFRONTEND_PORT=59992\\n'
      printf 'DB_NAME=%q\\n' multica_checkout_77; } > "${envs}/$name/manifest.env" ;;
  status)
    f=$(ls "${envs}"/*/manifest.env 2>/dev/null | head -1); [ -n "$f" ] || exit 1
    ( . "$f"; printf '{"name":"%s","owner":"%s","backend_port":%s,"frontend_port":%s,"database":"%s","components":{"web":{"state":"running"}}}' "$NAME" "$OWNER" "$BACKEND_PORT" "$FRONTEND_PORT" "$DB_NAME" ) ;;
  destroy) rm -rf "${envs}"/*/ ;;
esac
exit 0
`,
  );
  chmodSync(join(bin, "make"), 0o755);
  const env = {
    ...process.env,
    PATH: `${bin}:${process.env.PATH}`,
    VERIFY_MULTICA_ENVS: envs,
    VERIFY_MULTICA_STATE: state,
    VERIFY_MULTICA_SKIP_HOST_CHECK: "1",
  };
  delete env.VERIFY_MULTICA_RUN_ID;
  const run = (script, args = [], extra = {}) =>
    spawnSync(join(scripts, script), args, { cwd: root, env: { ...env, ...extra }, encoding: "utf8", timeout: 60000 });
  const makeCalls = () => readFileSync(calls, "utf8").trim().split("\n").filter(Boolean);
  const clean = () => rmSync(dir, { recursive: true, force: true });
  return { dir, root, envs, state, run, makeCalls, clean };
}

// A foreground-less helper process with a chosen cwd and extra environment.
const idle = (cwd, extra = {}) =>
  spawn(process.execPath, ["-e", "setTimeout(() => {}, 60000)"], { cwd, env: { ...process.env, ...extra }, stdio: "ignore" });
const alive = (pid) => spawnSync("kill", ["-0", String(pid)]).status === 0;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function preexistingEnv(sb) {
  mkdirSync(join(sb.envs, "checkout-77"), { recursive: true });
  const manifest = join(sb.envs, "checkout-77/manifest.env");
  writeFileSync(
    manifest,
    `NAME=checkout-77\nDIR=${sb.root}\nOWNER=human\nCREATED_AT=2026-01-01T00:00:00Z\nBACKEND_PORT=59991\nFRONTEND_PORT=59992\nDB_NAME=multica_checkout_77\n`,
  );
  return manifest;
}

test("launch refuses a checkout that already has an environment and leaves it untouched", () => {
  const sb = sandbox();
  try {
    const manifest = preexistingEnv(sb);
    const before = readFileSync(manifest, "utf8");
    const r = sb.run("launch.sh");
    assert.notEqual(r.status, 0, "launch must refuse");
    assert.match(r.stderr, /already|not (started|launched) by this run|refus/i);
    assert.equal(readFileSync(manifest, "utf8"), before, "owner and TTL of the existing environment are unchanged");
    assert.ok(!sb.makeCalls().some((c) => c.startsWith("up")), "make up must not run");
  } finally {
    sb.clean();
  }
});

test("cleanup refuses an environment this run did not launch and keeps its data and processes", async () => {
  const sb = sandbox();
  const bystander = idle(sb.root);
  try {
    preexistingEnv(sb);
    const r = sb.run("cleanup.sh");
    assert.notEqual(r.status, 0, "cleanup must refuse");
    assert.match(r.stderr, /refus|ownership|not (started|launched)/i);
    assert.ok(existsSync(join(sb.envs, "checkout-77/manifest.env")), "the environment registry entry survives");
    assert.ok(!sb.makeCalls().some((c) => /^(down|destroy)/.test(c)), "no down or destroy");
    await sleep(300);
    assert.ok(alive(bystander.pid), "a process in the same checkout is untouched");
  } finally {
    bystander.kill();
    sb.clean();
  }
});

test("cleanup refuses when the registered environment is no longer the one this run launched", () => {
  const sb = sandbox();
  try {
    assert.equal(sb.run("launch.sh").status, 0);
    // Someone replaced the environment after launch: different creation time and owner.
    const manifest = join(sb.envs, "checkout-77/manifest.env");
    writeFileSync(manifest, readFileSync(manifest, "utf8").replace(/CREATED_AT=.*/, "CREATED_AT=2020-01-01T00:00:00Z").replace(/OWNER=.*/, "OWNER=human"));
    const r = sb.run("cleanup.sh");
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /refus|ownership/i);
    assert.ok(!sb.makeCalls().some((c) => /^(down|destroy)/.test(c)));
    assert.ok(existsSync(manifest));
  } finally {
    sb.clean();
  }
});

test("launch then cleanup removes the launched environment and only this run's processes", async () => {
  const sb = sandbox();
  let owned;
  let bystander;
  try {
    const launch = sb.run("launch.sh");
    assert.equal(launch.status, 0, launch.stderr);
    assert.ok(sb.makeCalls().some((c) => c.startsWith("up") && c.includes("--ephemeral")));
    const marker = readFileSync(join(sb.state, readdirOne(sb.state)), "utf8");
    const runId = /RUN_ID=(\S+)/.exec(marker)?.[1];
    assert.ok(runId, "launch records a run id");
    owned = idle(sb.root, { VERIFY_MULTICA_RUN_ID: runId.replace(/^'|'$/g, "") });
    bystander = idle(sb.root);
    await sleep(300);
    const r = sb.run("cleanup.sh");
    assert.equal(r.status, 0, r.stderr);
    assert.ok(sb.makeCalls().some((c) => c.startsWith("destroy")), "destroy ran for the launched environment");
    assert.ok(!existsSync(join(sb.envs, "checkout-77")), "the launched environment is gone");
    assert.deepEqual(readdirSyncSafe(sb.state), [], "the ownership record is removed");
    await sleep(500);
    assert.ok(!alive(owned.pid), "the process this run started is stopped");
    assert.ok(alive(bystander.pid), "an unrelated process in the same checkout survives");
  } finally {
    owned?.kill();
    bystander?.kill();
    sb.clean();
  }
});

import { readdirSync } from "node:fs";
function readdirOne(d) {
  const [f] = readdirSync(d);
  assert.ok(f, "an ownership record exists");
  return f;
}
function readdirSyncSafe(d) {
  return existsSync(d) ? readdirSync(d) : [];
}

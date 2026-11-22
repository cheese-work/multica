import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { AUDIENCES_ENV, SIGNING_KEYS_ENV, buildManifest, canonicalJSON, render } from "./manifest.mjs";

const SEED = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=";
const d = (c) => `sha256:${c.repeat(64)}`;
const facts = () => ({
  server_image_digest: d("a"),
  gateway_image_digests: { c00: d("b"), x99: d("b") },
  schema_version: "123_browser_lease",
  credential_mode: "per-gateway",
  listener: "172.30.0.2:9000",
  environment: {
    events_url: "http://172.30.0.2:9000/internal/browser/instances/x/events",
    jwks_url: "http://172.30.0.2:9000/internal/browser/jwks",
    edge_urls: { c00: "https://c00.example", x99: "https://x99.example" },
    instance_ids: ["x99-1", "c00-1"],
    networks: ["control"],
    gateways_present: ["c00", "x99"],
  },
});
const env = () => ({
  [SIGNING_KEYS_ENV]: `k2:${SEED},k1:${SEED.replace("AQ", "Ag")}`,
  [AUDIENCES_ENV]: "steel-gateway/x99, steel-gateway/c00",
});

test("canonical JSON sorts keys at every depth", () => {
  assert.equal(canonicalJSON({ b: 1, a: { d: [2, { z: 1, y: 0 }], c: null } }), '{"a":{"c":null,"d":[2,{"y":0,"z":1}]},"b":1}');
});

test("manifest holds key ids and a sorted allowlist, never a seed", () => {
  const m = buildManifest(facts(), env());
  assert.deepEqual(m.invariant.signing_key_ids, ["k2", "k1"]);
  assert.deepEqual(m.invariant.allowlist, ["steel-gateway/c00", "steel-gateway/x99"]);
  assert.ok(!render(m).includes(SEED));
  assert.ok(!render(m).includes(SEED.replace("AQ", "Ag")));
});

test("unset keys and allowlist give empty lists", () => {
  const m = buildManifest(facts(), {});
  assert.deepEqual(m.invariant.signing_key_ids, []);
  assert.deepEqual(m.invariant.allowlist, []);
});

test("invariant hash ignores the environment section; manifest hash does not", () => {
  const a = buildManifest(facts(), env());
  const f = facts();
  f.environment.edge_urls.x99 = "https://staging-x99.example";
  const b = buildManifest(f, env());
  assert.equal(a.invariant_sha256, b.invariant_sha256);
  assert.notEqual(a.manifest_sha256, b.manifest_sha256);
});

test("an invariant change moves both hashes", () => {
  const a = buildManifest(facts(), env());
  const b = buildManifest(facts(), { ...env(), [AUDIENCES_ENV]: "steel-gateway/c00" });
  assert.notEqual(a.invariant_sha256, b.invariant_sha256);
  assert.notEqual(a.manifest_sha256, b.manifest_sha256);
});

test("output is stable across fact key order and allowlist order", () => {
  const a = render(buildManifest(facts(), env()));
  const f = facts();
  const reordered = { environment: f.environment, listener: f.listener, credential_mode: f.credential_mode, schema_version: f.schema_version, gateway_image_digests: { x99: d("b"), c00: d("b") }, server_image_digest: f.server_image_digest };
  const b = render(buildManifest(reordered, { ...env(), [AUDIENCES_ENV]: "steel-gateway/c00,steel-gateway/x99" }));
  assert.equal(a, b);
});

test("bad input is refused without echoing a seed", () => {
  const cases = [
    [{ ...facts(), server_image_digest: "latest" }, env()],
    [{ ...facts(), credential_mode: "token" }, env()],
    [facts(), { ...env(), [AUDIENCES_ENV]: "c00" }],
    [facts(), { ...env(), [AUDIENCES_ENV]: "steel-gateway/c00,steel-gateway/c00" }],
    [facts(), { ...env(), [SIGNING_KEYS_ENV]: `bad kid:${SEED}` }],
  ];
  for (const [f, e] of cases) {
    assert.throws(() => buildManifest(f, e), (err) => !String(err.message).includes(SEED));
  }
});

test("CLI prints the manifest and exits 1 on bad facts", () => {
  const dir = mkdtempSync(join(tmpdir(), "bcm-"));
  const ok = join(dir, "ok.json");
  const bad = join(dir, "bad.json");
  writeFileSync(ok, JSON.stringify(facts()));
  writeFileSync(bad, JSON.stringify({ ...facts(), schema_version: "" }));
  const script = fileURLToPath(new URL("./index.mjs", import.meta.url));
  const run = (file) => spawnSync(process.execPath, [script, "--facts", file], { env: { ...process.env, ...env() }, encoding: "utf8" });
  const good = run(ok);
  assert.equal(good.status, 0);
  assert.equal(good.stdout, render(buildManifest(facts(), env())));
  assert.ok(!good.stdout.includes(SEED));
  assert.equal(run(bad).status, 1);
});

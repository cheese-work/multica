import { createHash } from "node:crypto";

// CHE-8829 A1a: the non-secret browser config manifest. Two sections: `invariant` (what a staging
// manifest must share byte for byte with the serving one) and `environment` (listed and accepted
// per field in the security receipt). Seeds never enter the output: only key ids do.

export const SIGNING_KEYS_ENV = "MULTICA_BROWSER_SIGNING_KEYS";
export const AUDIENCES_ENV = "MULTICA_BROWSER_GATEWAY_AUDIENCES";

const KID = /^[A-Za-z0-9_-]{1,64}$/;
const AUD = /^steel-gateway\/[a-z0-9][a-z0-9-]{0,62}$/;
const DIGEST = /^sha256:[0-9a-f]{64}$/;
const CREDENTIAL_MODES = new Set(["none", "per-gateway", "install-token"]);

export function canonicalJSON(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (value && typeof value === "object") {
    const keys = Object.keys(value).sort();
    return `{${keys.map((k) => `${JSON.stringify(k)}:${canonicalJSON(value[k])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

export const sha256 = (text) => createHash("sha256").update(text).digest("hex");

// Key ids in signing order. Throws without echoing any part of a seed.
export function keyIds(spec) {
  const text = (spec ?? "").trim();
  if (text === "") return [];
  return text.split(",").map((entry, i) => {
    const kid = entry.trim().split(":")[0];
    if (!KID.test(kid)) throw new Error(`${SIGNING_KEYS_ENV} entry ${i + 1}: bad key id`);
    return kid;
  });
}

// Sorted audience allowlist. Same grammar as browserlease.parseAudiences.
export function audiences(spec) {
  const text = (spec ?? "").trim();
  if (text === "") return [];
  const out = text.split(",").map((a, i) => {
    const aud = a.trim();
    if (!AUD.test(aud)) throw new Error(`${AUDIENCES_ENV} entry ${i + 1}: want steel-gateway/<host>`);
    return aud;
  });
  if (new Set(out).size !== out.length) throw new Error(`${AUDIENCES_ENV}: duplicate audience`);
  return out.sort();
}

function need(facts, key, ok, what) {
  if (!ok(facts[key])) throw new Error(`facts.${key}: ${what}`);
  return facts[key];
}

const isDigest = (v) => typeof v === "string" && DIGEST.test(v);
const isStr = (v) => typeof v === "string" && v !== "";
const isStrList = (v) => Array.isArray(v) && v.every(isStr);

// facts: operator-supplied, non-secret. env: the process environment of the deployment.
export function buildManifest(facts, env) {
  const gateways = facts.gateway_image_digests ?? {};
  for (const [name, d] of Object.entries(gateways)) {
    if (!isDigest(d)) throw new Error(`facts.gateway_image_digests.${name}: want sha256:<64 hex>`);
  }
  const invariant = {
    server_image_digest: need(facts, "server_image_digest", isDigest, "want sha256:<64 hex>"),
    gateway_image_digests: gateways,
    schema_version: need(facts, "schema_version", isStr, "want the last applied migration"),
    allowlist: audiences(env[AUDIENCES_ENV]),
    signing_key_ids: keyIds(env[SIGNING_KEYS_ENV]),
    credential_mode: need(facts, "credential_mode", (v) => CREDENTIAL_MODES.has(v), "want none, per-gateway or install-token"),
    // Listener set: the internal listener address, or null when unset.
    listener: facts.listener ?? null,
  };
  const e = facts.environment ?? {};
  const environment = {
    events_url: e.events_url ?? null,
    jwks_url: e.jwks_url ?? null,
    edge_urls: need(e, "edge_urls", (v) => v && typeof v === "object", "want an object"),
    instance_ids: need(e, "instance_ids", isStrList, "want a list"),
    networks: need(e, "networks", isStrList, "want a list"),
    gateways_present: need(e, "gateways_present", isStrList, "want a list"),
  };
  const body = { invariant, environment };
  return {
    ...body,
    invariant_sha256: sha256(canonicalJSON(invariant)),
    manifest_sha256: sha256(canonicalJSON(body)),
  };
}

export const render = (manifest) => `${canonicalJSON(manifest)}\n`;

#!/usr/bin/env bash
set -euo pipefail

# Render test for deploy/cd/docker-compose.flags.yml + docker-compose.jev.yml:
# the optional overlays keep a feature-flag file and a scoped Jev runtime (deployment key,
# gateway network, collision-safe DB hostname) across blue/green recreation.
# Uses the real `docker compose config` (no daemon, no containers) with
# synthetic values only — no real key, workspace or path is read.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# 32 bytes of 0x41, base64 — the shape credential.DecodeKey accepts.
synthetic_key="$(printf 'A%.0s' {1..32} | base64)"
flags_file="$work_dir/flags.yaml"
cp deploy/cd/fixtures/jev-feature-flags.synthetic.yaml "$flags_file"

render() { # render <jev:0|1|flags> [VAR=value ...] -> JSON on stdout
  local jev=$1; shift
  local -a files=(-f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml)
  [ "$jev" = 0 ] || files+=(-f deploy/cd/docker-compose.flags.yml)
  [ "$jev" = 1 ] && files+=(-f deploy/cd/docker-compose.jev.yml)
  env -i HOME="$HOME" PATH="$PATH" JWT_SECRET=synthetic-jwt-secret "$@" \
    docker compose "${files[@]}" config --format json
}

jev_env=(
  MULTICA_JEV_SECRET_KEY="$synthetic_key"
  MULTICA_JEV_FLAGS_FILE="$flags_file"
  MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway
)

fail() { echo "test-jev-overlay: $*" >&2; exit 1; }

off="$(render 0)"
on="$(render 1 "${jev_env[@]}")"
flags_only="$(render flags MULTICA_FEATURE_FLAGS_FILE="$flags_file" MULTICA_JEV_FLAGS_FILE=/nonexistent/legacy.yaml)"
legacy_flags="$(render flags MULTICA_JEV_FLAGS_FILE="$flags_file")"

# Compare the rendered models with node: assertions read both renders.
printf '%s' "$off" >"$work_dir/off.json"
printf '%s' "$on" >"$work_dir/on.json"
printf '%s' "$flags_only" >"$work_dir/flags-only.json"
printf '%s' "$legacy_flags" >"$work_dir/legacy-flags.json"
node - "$work_dir/off.json" "$work_dir/on.json" "$flags_file" "$synthetic_key" "$work_dir/flags-only.json" "$work_dir/legacy-flags.json" <<'JS'
const fs = require("node:fs");
const [off, on] = process.argv.slice(2, 4).map((p) => JSON.parse(fs.readFileSync(p, "utf8")));
const [flagsFile, key] = process.argv.slice(4);
const eq = (a, b, what) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${what}: ${JSON.stringify(a)} != ${JSON.stringify(b)}`); };
const ok = (c, what) => { if (!c) throw new Error(what); };
const colours = ["blue", "green"];
const flagsOnly = JSON.parse(fs.readFileSync(process.argv[6], "utf8"));
const legacyFlags = JSON.parse(fs.readFileSync(process.argv[7], "utf8"));
eq(flagsOnly, legacyFlags, "generic and legacy flag paths render identically");
for (const colour of colours) {
  const service = flagsOnly.services[`backend-${colour}`];
  const baseline = off.services[`backend-${colour}`];
  eq(service.environment.MULTICA_FEATURE_FLAGS_FILE, "/app/feature-flags.yaml", `${colour} generic flags env`);
  const mount = service.volumes.find((volume) => volume.target === "/app/feature-flags.yaml");
  ok(mount?.type === "bind" && mount.source === flagsFile && mount.read_only === true, `${colour} generic flags mount`);
  const withoutFlags = structuredClone(service);
  delete withoutFlags.environment.MULTICA_FEATURE_FLAGS_FILE;
  withoutFlags.volumes = withoutFlags.volumes.filter((volume) => volume.target !== "/app/feature-flags.yaml");
  eq(withoutFlags, baseline, `${colour} flags-only leaves other fields unchanged`);
}
ok(!("jev_gateway" in flagsOnly.networks), "flags-only declares no Jev network");
for (const service of ["postgres", "frontend-blue", "frontend-green"]) {
  eq(flagsOnly.services[service], off.services[service], `${service} unchanged by flags-only`);
}
eq(flagsOnly.volumes, off.volumes, "flags-only named volumes");

// Unconfigured installs: no Jev surface at all.
for (const c of colours) {
  const s = off.services[`backend-${c}`];
  ok(!("MULTICA_FEATURE_FLAGS_FILE" in s.environment), `unconfigured ${c} carries a flags file env`);
  ok(Object.keys(s.networks).join() === "default", `unconfigured ${c} networks: ${Object.keys(s.networks)}`);
  ok(s.environment.DATABASE_URL.includes("@postgres:5432/"), `unconfigured ${c} DB host changed`);
}
ok(!Object.keys(off.networks).includes("jev_gateway"), "unconfigured render declares the gateway network");

for (const c of colours) {
  const s = on.services[`backend-${c}`];
  const env = s.environment;
  // Deployment key + scoped flag file survive recreation.
  eq(env.MULTICA_JEV_SECRET_KEY, key, `${c} key`);
  eq(env.MULTICA_FEATURE_FLAGS_FILE, "/app/feature-flags.yaml", `${c} flags env`);
  const flagMount = s.volumes.find((v) => v.target === "/app/feature-flags.yaml");
  ok(flagMount && flagMount.type === "bind" && flagMount.source === flagsFile && flagMount.read_only === true, `${c} flags mount: ${JSON.stringify(flagMount)}`);
  // Existing volumes, ports and data bindings untouched.
  eq(s.volumes.filter((v) => v.target !== "/app/feature-flags.yaml"), off.services[`backend-${c}`].volumes, `${c} volumes`);
  eq(s.ports, off.services[`backend-${c}`].ports, `${c} ports`);
  // Gateway membership alongside the project network; no static pins.
  eq(Object.keys(s.networks).sort(), ["default", "jev_gateway"], `${c} networks`);
  eq(on.networks.jev_gateway.name, "synthetic_gateway", "gateway network name");
  eq(on.networks.jev_gateway.external, true, "gateway network is external");
  ok(!s.extra_hosts, `${c} carries extra_hosts pins`);
  // DB hostname resolves only on the project network, with the same credentials/port/db.
  eq(env.DATABASE_URL, off.services[`backend-${c}`].environment.DATABASE_URL.replace("@postgres:5432/", `@${on.name}-postgres-1:5432/`), `${c} DATABASE_URL`);
  // Jev stays scoped: no global override, no legacy credential.
  ok(!("FF_JEV_ENABLED" in env) && !("JEV_API_KEY" in env), `${c} carries a global Jev override`);
  // Everything else on the service is unchanged.
  const strip = (x) => { const y = structuredClone(x); for (const k of ["MULTICA_JEV_SECRET_KEY", "MULTICA_FEATURE_FLAGS_FILE", "DATABASE_URL"]) delete y.environment[k]; delete y.volumes; delete y.networks; return y; };
  eq(strip(s), strip(off.services[`backend-${c}`]), `${c} other fields`);
  eq(on.services[`frontend-${c}`], off.services[`frontend-${c}`], `frontend-${c}`);
}
eq(on.services.postgres, off.services.postgres, "postgres service");
eq(on.volumes, off.volumes, "named volumes");
JS

# A configured overlay with a missing prerequisite must fail to render.
for var in MULTICA_JEV_SECRET_KEY MULTICA_JEV_FLAGS_FILE MULTICA_JEV_GATEWAY_NETWORK; do
  args=()
  for kv in "${jev_env[@]}"; do [ "${kv%%=*}" = "$var" ] || args+=("$kv"); done
  if render 1 "${args[@]}" >/dev/null 2>"$work_dir/err"; then
    fail "overlay rendered without $var"
  fi
  grep -q "$var" "$work_dir/err" || fail "render error does not name $var"
  grep -qF "$synthetic_key" "$work_dir/err" && fail "render error leaks the key"
done

# The DB host follows the effective Compose project (COMPOSE_PROJECT_NAME or
# -p), because the generated postgres container name is <project>-postgres-1.
for form in env flag; do
  if [ "$form" = env ]; then
    project_json="$(render 1 "${jev_env[@]}" COMPOSE_PROJECT_NAME=tenant_synthetic)"
  else
    project_json="$(env -i HOME="$HOME" PATH="$PATH" JWT_SECRET=synthetic-jwt-secret "${jev_env[@]}" \
      docker compose -p tenant_synthetic -f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml -f deploy/cd/docker-compose.flags.yml -f deploy/cd/docker-compose.jev.yml config --format json)"
  fi
  printf '%s' "$project_json" | node -e '
    let d = ""; process.stdin.on("data", (c) => (d += c)).on("end", () => {
      const m = JSON.parse(d);
      for (const c of ["blue", "green"]) {
        const url = m.services[`backend-${c}`].environment.DATABASE_URL;
        if (m.name !== "tenant_synthetic" || !url.includes("@tenant_synthetic-postgres-1:5432/")) {
          console.error(`project ${m.name}: backend-${c} DB host is not tenant_synthetic-postgres-1`);
          process.exit(1);
        }
      }
    });
  ' || fail "DB host does not follow the effective project ($form form)"
done

echo "jev overlay render fixtures passed"

#!/usr/bin/env node
// Deploy drift receipt (CHE-2396). A C00 cutover must run the released
// compose files, with the env keys they render, on the manifest's image
// digests. `sync` copies the released files over the deployed ones; `check`
// compares deployed bytes, env key NAMES (never values) and running image
// digests against that source and exits nonzero on any difference.
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { pathToFileURL } from "node:url";

export const COMPOSE_FILES = [
  "docker-compose.selfhost.yml",
  "deploy/cd/docker-compose.ab.yml",
  "deploy/cd/docker-compose.flags.yml",
  "deploy/cd/docker-compose.jev.yml",
];

const sha256 = (path) => (existsSync(path) ? createHash("sha256").update(readFileSync(path)).digest("hex") : null);

export function syncComposeFiles(sourceRoot, composeDir) {
  for (const name of COMPOSE_FILES) {
    const source = join(sourceRoot, name);
    const target = join(composeDir, name);
    if (sha256(source) === sha256(target)) continue;
    mkdirSync(dirname(target), { recursive: true });
    copyFileSync(source, target);
  }
}

export function compare({ compose, services }) {
  const differences = [];
  if (compose.length === 0) differences.push("compose: no files were compared");
  if (Object.keys(services).length === 0) differences.push("services: none were checked");
  for (const { name, source_sha256: want, deployed_sha256: got } of compose) {
    if (want === null) differences.push(`compose ${name}: released source file is missing`);
    else if (want !== got) differences.push(`compose ${name}: deployed bytes differ from released source`);
  }
  for (const [name, s] of Object.entries(services)) {
    const have = new Set(s.container_env);
    const effective = new Set([...s.expected_env, ...s.image_env]);
    for (const key of effective) if (!have.has(key)) differences.push(`${name} env: missing key ${key}`);
    for (const key of have) if (!effective.has(key)) differences.push(`${name} env: unexpected key ${key}`);
    if (!s.repo_digests.some((d) => d.endsWith(`@${s.expected_digest}`))) {
      differences.push(`${name} image: running digest ${s.repo_digests.join(",") || "<none>"} does not include manifest digest ${s.expected_digest}`);
    }
  }
  return { schema_version: 1, status: differences.length ? "drift" : "match", differences, compose, services };
}

const keyNames = (env) => (env ?? []).map((entry) => entry.split("=")[0]);
const inspect = (format, target) => JSON.parse(execFileSync("docker", ["inspect", "--format", format, target], { encoding: "utf8" }));

function gatherService(name, containerId, configEnv, manifestImage) {
  const imageId = execFileSync("docker", ["inspect", "--format", "{{.Image}}", containerId], { encoding: "utf8" }).trim();
  const rendered = configEnv ?? {};
  return {
    expected_env: Array.isArray(rendered) ? keyNames(rendered) : Object.keys(rendered),
    container_env: keyNames(inspect("{{json .Config.Env}}", containerId)),
    image_env: keyNames(inspect("{{json .Config.Env}}", imageId)),
    expected_digest: manifestImage.split("@")[1],
    repo_digests: inspect("{{json .RepoDigests}}", imageId) ?? [],
  };
}

function main(argv) {
  const [command, ...rest] = argv;
  const opts = { service: [] };
  for (let i = 0; i < rest.length; i += 2) {
    const key = rest[i].replace(/^--/, "").replaceAll("-", "_");
    if (key === "service") opts.service.push(rest[i + 1]);
    else opts[key] = rest[i + 1];
  }
  if (command === "sync") {
    syncComposeFiles(opts.source_root, opts.compose_dir);
    return 0;
  }
  if (command !== "check") {
    console.error("usage: drift-receipt.mjs sync|check --source-root DIR --compose-dir DIR [--config FILE --manifest FILE --service NAME=CONTAINER_ID ... --output FILE]");
    return 2;
  }
  const config = JSON.parse(readFileSync(opts.config, "utf8"));
  const manifest = JSON.parse(readFileSync(opts.manifest, "utf8"));
  const services = {};
  for (const spec of opts.service) {
    const [name, containerId] = spec.split("=");
    if (!containerId || !config.services?.[name]) throw new Error(`service ${name}: no running container or no compose render`);
    const image = name.startsWith("backend") ? manifest.images.backend : manifest.images.web;
    services[name] = gatherService(name, containerId, config.services[name].environment, image);
  }
  const compose = COMPOSE_FILES.map((name) => ({
    name,
    source_sha256: sha256(join(opts.source_root, name)),
    deployed_sha256: sha256(join(opts.compose_dir, name)),
  }));
  const receipt = compare({ compose, services });
  const text = `${JSON.stringify(receipt, null, 2)}\n`;
  if (opts.output) writeFileSync(opts.output, text);
  process.stdout.write(text);
  return receipt.status === "match" ? 0 : 1;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) process.exitCode = main(process.argv.slice(2));

#!/usr/bin/env node
// Usage: browser-config-manifest --facts <facts.json>
// Reads MULTICA_BROWSER_SIGNING_KEYS and MULTICA_BROWSER_GATEWAY_AUDIENCES from the environment,
// prints the canonical manifest JSON to stdout. Never prints a seed.
import { readFileSync } from "node:fs";
import { buildManifest, render } from "./manifest.mjs";

const i = process.argv.indexOf("--facts");
if (i < 0 || !process.argv[i + 1]) {
  console.error("usage: browser-config-manifest --facts <facts.json>");
  process.exit(2);
}
try {
  const facts = JSON.parse(readFileSync(process.argv[i + 1], "utf8"));
  process.stdout.write(render(buildManifest(facts, process.env)));
} catch (err) {
  console.error(`browser-config-manifest: ${err.message}`);
  process.exit(1);
}

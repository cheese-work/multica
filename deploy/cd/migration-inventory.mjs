#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";

const migrationsDir = resolve(process.argv[2] ?? "server/migrations");
const names = readdirSync(migrationsDir)
  .filter((name) => name.endsWith(".sql"))
  .sort();

const hash = createHash("sha256");
for (const name of names) {
  hash.update(name);
  hash.update("\0");
  hash.update(readFileSync(join(migrationsDir, name)));
  hash.update("\0");
}

process.stdout.write(`sha256:${hash.digest("hex")}\n`);

// CHE-6563 (CHE-6210 A2c): order check for an upstream-merge renumber map.
//
// The runner applies `up` files in byte order of the file name, so an imported
// migration only applies after every migration already deployed if its stem sorts
// after all of them. The numeric suggestion of migration-prefix-check.mjs does not:
// 9999919_ sorts before 999j_.
//
// Map: JSON array of { from, to } in upstream order. `to` is the stem on this tree.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

export const MAP_PATH = "server/cmd/migrate/renumber-map.json";
const MIGRATIONS = "server/migrations";
const REGISTRY = "server/cmd/migrate/main.go";

// Map keys and list entries in the migrate registries: `"<stem>":` or `"<stem>",`.
export function registryKeys(source) {
  return [...source.matchAll(/^\s*"(\d+[a-z]*_\w+)"\s*[:,]/gm)].map((m) => m[1]);
}

export function checkOrder({ map, upFiles, downFiles, registryKeys: keys }) {
  const errors = [];
  const mapped = new Set(map.map((m) => m.to));
  const stems = new Set(upFiles.map((f) => f.replace(/\.up\.sql$/, "")));
  const last = upFiles.filter((f) => !mapped.has(f.replace(/\.up\.sql$/, ""))).sort().at(-1) ?? "";
  for (const { to } of map) {
    if (`${to}.up.sql` <= last) errors.push(`${to} does not sort after ${last.replace(/\.up\.sql$/, "")}, the last unmapped migration`);
    if (!stems.has(to)) errors.push(`${to} has no up file in the tree`);
    if (!downFiles.includes(`${to}.down.sql`)) errors.push(`${to} has no down file`);
  }
  for (let i = 1; i < map.length; i++) {
    if (map[i].to <= map[i - 1].to) errors.push(`${map[i].to} is out of order: it must sort after ${map[i - 1].to} (upstream order)`);
  }
  for (const key of keys) {
    if (!stems.has(key)) errors.push(`registry key ${key} names no existing migration`);
  }
  return errors;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const map = JSON.parse(readFileSync(process.argv[2] ?? MAP_PATH, "utf8"));
  const files = readdirSync(MIGRATIONS);
  const errors = checkOrder({
    map,
    upFiles: files.filter((f) => f.endsWith(".up.sql")),
    downFiles: files.filter((f) => f.endsWith(".down.sql")),
    registryKeys: registryKeys(readFileSync(REGISTRY, "utf8")),
  });
  for (const e of errors) console.error(`::error::${e}`);
  if (errors.length) process.exit(1);
  console.log(`migration order ok: ${map.length} mapped`);
}

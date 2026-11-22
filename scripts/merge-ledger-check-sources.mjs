// Helper for scripts/merge-ledger-check (CHE-6562): which paths do the Source
// pointers of the fork feature ledger protect? Reuses the fork-sync guard's
// parser (directory pointers, `*` within a filename, abbreviated filenames).
// Usage: node merge-ledger-check-sources.mjs FEATURES.md PATHS.txt
// Prints "path<TAB>feature,feature" for every path a pointer covers.
// Exit 3 when the ledger has no pointer or an entry is not a plain path
// (it may cover paths nobody can name, so the check must not guess).
import { readFileSync } from "node:fs";
import { sourcePaths } from "./fork-sync-guard.mjs";

const [featuresFile, pathsFile] = process.argv.slice(2);
let sources;
try {
  sources = sourcePaths(readFileSync(featuresFile, "utf8"));
} catch (error) {
  console.error(`merge-ledger-check: ${error.message}`);
  process.exit(3);
}
const plain = /^[A-Za-z0-9_.@+()[\]/*-]+$/;
const bad = sources.filter(({ entry }) => !plain.test(entry) || entry.startsWith("/") || entry.split("/").includes(".."));
if (bad.length > 0) {
  for (const { feature, entry } of bad) console.error(`merge-ledger-check: unresolvable FORK-FEATURES Source entry (${feature}): ${entry}`);
  process.exit(3);
}
for (const path of readFileSync(pathsFile, "utf8").split("\n").filter(Boolean)) {
  const features = [...new Set(sources.filter(({ pattern }) => pattern.test(path)).map(({ feature }) => feature))];
  if (features.length > 0) console.log(`${path}\t${features.join(",")}`);
}

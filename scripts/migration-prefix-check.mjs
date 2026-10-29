import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const MIGRATION = /^server\/migrations\/(\d+)_(.+)\.(up|down)\.sql$/;

function migrations(files) {
  return files.flatMap((path) => {
    const match = path.match(MIGRATION);
    return match ? [{ path, prefix: match[1], number: Number(match[1]), stem: `${match[1]}_${match[2]}` }] : [];
  });
}

export function checkMigrationPrefixes({ files, main, pulls }) {
  const candidate = migrations(files);
  const reservations = [
    ...migrations(main).map((file) => ({ ...file, owner: "origin/main" })),
    ...pulls.flatMap((pull) => migrations(pull.files).map((file) => ({ ...file, owner: `PR #${pull.number}` }))),
  ];
  const collisions = [];
  const commands = [];
  let nextFree = Math.max(0, ...candidate.map((file) => file.number), ...reservations.map((file) => file.number)) + 1;
  const firstFree = nextFree;
  const quote = (value) => `'${value.replaceAll("'", "'\\''")}'`;
  for (const stem of new Set(candidate.map((file) => file.stem))) {
    const group = candidate.filter((file) => file.stem === stem);
    const conflicts = [
      ...reservations.filter((file) => file.number === group[0].number && file.stem !== stem),
      ...candidate.filter((file) => file.number === group[0].number && file.stem !== stem)
        .map((file) => ({ ...file, owner: "this PR" })),
    ];
    if (conflicts.length === 0) continue;
    for (const conflict of new Set(conflicts.map((file) => `${file.owner} (${file.stem})`))) {
      collisions.push(`${stem} collides with ${conflict}`);
    }
    const replacement = String(nextFree++).padStart(group[0].prefix.length, "0");
    for (const file of group) {
      const destination = file.path.replace(/\/\d+_/, `/${replacement}_`);
      commands.push(`git mv -- ${quote(file.path)} ${quote(destination)}`);
    }
  }
  return { collisions, commands, nextFree: firstFree };
}

export function addedMigrations(files) {
  return files.filter((file) => ["added", "copied", "renamed"].includes(file.status) && MIGRATION.test(file.filename))
    .map((file) => file.filename);
}

export async function collectMigrations({ repository, number, token, expectedHead, fetcher = fetch, signal = AbortSignal.timeout(25_000) }) {
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository) || !Number.isSafeInteger(number) || number < 1) {
    throw new Error("Expected an owner/repository and a positive PR number");
  }
  async function request(endpoint) {
    const response = await fetcher(`https://api.github.com/repos/${repository}/${endpoint}`, {
      headers: { Accept: "application/vnd.github+json", ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      signal,
    });
    if (!response.ok) throw new Error(`GitHub ${endpoint}: HTTP ${response.status}`);
    return response.json();
  }
  async function pages(endpoint, limit = Infinity) {
    const entries = [];
    for (let page = 1; ; page++) {
      const batch = await request(`${endpoint}${endpoint.includes("?") ? "&" : "?"}per_page=100&page=${page}`);
      if (!Array.isArray(batch)) throw new Error(`Invalid GitHub list: ${endpoint}`);
      entries.push(...batch);
      if (entries.length >= limit) throw new Error(`GitHub ${endpoint} reached its ${limit}-file limit; cannot verify all migrations`);
      if (batch.length < 100) return entries;
    }
  }
  const files = addedMigrations(await pages(`pulls/${number}/files`, 3000));
  if (files.length === 0) return { files, main: [], pulls: [] };
  const [tree, openPulls] = await Promise.all([
    request("git/trees/main?recursive=1"),
    pages("pulls?state=open"),
  ]);
  if (tree.truncated !== false || !Array.isArray(tree.tree)) throw new Error("Incomplete origin/main tree; cannot verify all migrations");
  if (expectedHead && openPulls.find((pull) => pull.number === number)?.head?.sha !== expectedHead) {
    throw new Error("PR head changed or closed; rerun the check on the current head");
  }
  const pending = openPulls.filter((pull) => pull.number !== number);
  const pulls = [];
  await Promise.all(Array.from({ length: Math.min(8, pending.length) }, async () => {
    for (let pull = pending.shift(); pull; pull = pending.shift()) {
      pulls.push({ number: pull.number, files: addedMigrations(await pages(`pulls/${pull.number}/files`, 3000)) });
    }
  }));
  pulls.sort((left, right) => left.number - right.number);
  return { files, main: tree.tree.filter((entry) => entry.type === "blob").map((entry) => entry.path), pulls };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const event = JSON.parse(readFileSync(process.env.GITHUB_EVENT_PATH, "utf8"));
    const input = await collectMigrations({
      repository: process.env.GITHUB_REPOSITORY,
      number: event.pull_request.number,
      expectedHead: event.pull_request.head.sha,
      token: process.env.GH_TOKEN,
    });
    const result = checkMigrationPrefixes(input);
    if (result.collisions.length) {
      console.error(`Migration prefix check failed:\n${result.collisions.join("\n")}\nNext free number: ${result.nextFree}\nRun:\n${result.commands.join("\n")}`);
      process.exitCode = 1;
    } else {
      console.log(`Migration prefixes are free (${input.files.length} new files checked).`);
    }
  } catch (error) {
    console.error(`Migration prefix check failed: ${error.message}`);
    process.exitCode = 1;
  }
}

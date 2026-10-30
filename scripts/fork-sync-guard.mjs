import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { posix } from "node:path";
import { pathToFileURL } from "node:url";

export function isUpstreamSync({ title = "", headRef = "", changes = [] }) {
  return headRef.startsWith("upstream-sync/")
    || /\b(?:sync|merge)\s+(?:with\s+)?upstream\b/i.test(title)
    || changes.some((change) => change.path === "UPSTREAM_VERSION" || change.previousPath === "UPSTREAM_VERSION");
}

function sourcePaths(ledger) {
  const sources = [];
  for (const line of ledger.split("\n")) {
    const feature = /^\|\s*([A-Z]\d+)\s/.exec(line)?.[1];
    const source = line.split("Source:")[1];
    if (!feature || !source) continue;
    let directory = ".";
    for (const match of source.matchAll(/`([^`]+)`/g)) {
      const path = match[1].includes("/") ? match[1] : posix.join(directory, match[1]);
      directory = path.endsWith("/") ? path.slice(0, -1) : posix.dirname(path);
      const escaped = path.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replaceAll("*", "[^/]*");
      sources.push({ feature, pattern: new RegExp(`^${escaped}${path.endsWith("/") ? ".*" : ""}$`) });
    }
  }
  if (sources.length === 0) throw new Error("No Source paths found in the base fork feature ledger");
  return sources;
}

function portedPaths(body, headFiles) {
  const ports = new Set();
  const files = new Set(headFiles);
  let fence = null;
  for (const line of body.replace(/\r\n?/g, "\n").replace(/<!--[\s\S]*?(-->|$)/g, "").split("\n")) {
    if (fence) {
      const close = /^ {0,3}(`{3,}|~{3,})\s*$/.exec(line);
      if (close && close[1][0] === fence[0] && close[1].length >= fence.length) fence = null;
      continue;
    }
    const open = /^ {0,3}(`{3,}|~{3,})/.exec(line);
    if (open) {
      fence = open[1];
      continue;
    }
    const port = /^ {0,3}(?:[-*] )?Ported:\s+`?([^`\s]+)`?\s+->\s+`?([^`\s]+)`?\s*$/.exec(line);
    if (port && port[1] !== port[2] && files.has(port[2])) ports.add(port[1]);
  }
  return ports;
}

export function checkForkSync(input) {
  if (!isUpstreamSync(input)) return { sync: false, violations: [] };
  const sources = sourcePaths(input.ledger);
  const ports = portedPaths(input.body ?? "", input.headFiles);
  const forkTests = new Set(input.forkTests);
  const violations = [];
  for (const change of input.changes) {
    const lostPath = change.status === "D" ? change.path : change.status.startsWith("R") ? change.previousPath : null;
    if (!lostPath || ports.has(lostPath)) continue;
    const features = [...new Set(sources.filter((source) => source.pattern.test(lostPath)).map((source) => source.feature))];
    const owners = [...features, ...(forkTests.has(lostPath) ? ["fork-authored test"] : [])];
    if (owners.length) violations.push(`${owners.join(", ")}: ${lostPath} was ${change.status === "D" ? "deleted" : `renamed to ${change.path}`}`);
  }
  return { sync: true, violations };
}

export function collectForkSync(pullRequest, cwd = process.cwd()) {
  const git = (...args) => execFileSync("git", args, { cwd, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], maxBuffer: 64 * 1024 * 1024 });
  const base = pullRequest.base.sha;
  const head = pullRequest.head.sha;
  if (![base, head].every((sha) => /^[a-f0-9]{40}$/.test(sha))) throw new Error("Expected full PR base and head SHAs");
  const fields = git("diff", "--find-renames", "--name-status", "-z", `${base}...${head}`).split("\0");
  const changes = [];
  while (fields[0]) {
    const status = fields.shift();
    const path = fields.shift();
    changes.push(status.startsWith("R") || status.startsWith("C")
      ? { status, previousPath: path, path: fields.shift() }
      : { status, path });
  }
  const input = { title: pullRequest.title, body: pullRequest.body, headRef: pullRequest.head.ref, changes };
  if (!isUpstreamSync(input)) return input;
  const version = git("show", `${base}:UPSTREAM_VERSION`).trim();
  if (!/^\d+\.\d+\.\d+$/.test(version)) throw new Error("Invalid base UPSTREAM_VERSION; cannot identify fork-authored tests");
  const upstream = `refs/tags/v${version}`;
  git("rev-parse", "--verify", `${upstream}^{commit}`);
  return {
    ...input,
    ledger: git("show", `${base}:docs/fork/FORK-FEATURES.md`),
    forkTests: git("diff", "--no-renames", "--name-only", "--diff-filter=AM", "-z", upstream, base).split("\0")
      .filter((path) => /[._-](?:test|spec)\.[^/]+$|(?:^|\/)test_[^/]+\.py$/.test(path)),
    headFiles: git("ls-tree", "-r", "--name-only", "-z", head).split("\0").filter(Boolean),
  };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const event = JSON.parse(readFileSync(process.env.GITHUB_EVENT_PATH, "utf8"));
    const result = checkForkSync(collectForkSync(event.pull_request));
    if (result.violations.length) {
      console.error(`Fork sync guard FAILED:\n${result.violations.join("\n")}\nAdd one Ported: <old path> -> <new path> line per file to the PR body; each destination must exist at the PR head.`);
      process.exitCode = 1;
    } else {
      console.log(result.sync ? "Fork sync guard PASSED: no unacknowledged fork file losses." : "Not an upstream sync PR; guard not applicable.");
    }
  } catch (error) {
    console.error(`Fork sync guard could not verify the PR: ${error.message}`);
    process.exitCode = 1;
  }
}

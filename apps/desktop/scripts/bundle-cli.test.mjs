import { execFileSync } from "node:child_process";
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { afterEach, expect, it } from "vitest";

const roots = [];

afterEach(() => {
  while (roots.length) rmSync(roots.pop(), { recursive: true, force: true });
});

function initRepo(parent, name, tag) {
  const root = join(parent, name);
  mkdirSync(root);
  const git = (...args) => execFileSync("git", args, { cwd: root, encoding: "utf-8" }).trim();
  git("init", "-q");
  git("config", "user.name", "test");
  git("config", "user.email", "test@multica.ai");
  git("config", "commit.gpgsign", "false");
  git("commit", "-q", "--allow-empty", "-m", name);
  git("tag", tag);
  return { root, commit: git("rev-parse", "--short", "HEAD") };
}

it.skipIf(process.platform === "win32")("bundles repository version and commit when launched from another repository", () => {
  const parent = mkdtempSync(join(tmpdir(), "multica-bundle-cli-"));
  roots.push(parent);
  const repo = initRepo(parent, "bundle", "v1.4.2");
  const other = initRepo(parent, "other", "v9.9.9");
  const scripts = join(repo.root, "apps", "desktop", "scripts");
  mkdirSync(scripts, { recursive: true });
  for (const name of ["bundle-cli.mjs", "package.mjs"]) {
    copyFileSync(new URL(name, import.meta.url), join(scripts, name));
  }
  const tools = join(parent, "tools");
  mkdirSync(tools);
  const capture = join(parent, "build.json");
  writeFileSync(join(tools, "go"), `#!${process.execPath}
import { writeFileSync } from "node:fs";
const args = process.argv.slice(2);
if (args[0] === "version") {
  console.log("go version go1.26.6");
} else {
  writeFileSync(${JSON.stringify(capture)}, JSON.stringify({ args, cwd: process.cwd() }));
  writeFileSync(args[args.indexOf("-o") + 1], "fixture CLI");
}
`, { mode: 0o755 });
  execFileSync(process.execPath, [join(scripts, "bundle-cli.mjs")], {
    cwd: other.root,
    env: { ...process.env, PATH: tools + delimiter + process.env.PATH },
  });
  const build = JSON.parse(readFileSync(capture, "utf-8"));
  const ldflags = build.args[build.args.indexOf("-ldflags") + 1];
  expect(ldflags).toContain("-X main.version=1.4.2 ");
  expect(ldflags).toContain(`-X main.commit=${repo.commit} `);
  expect(ldflags).not.toContain(`-X main.commit=${other.commit} `);
  expect(build.cwd).toBe(join(repo.root, "server"));
  expect(readFileSync(join(repo.root, "apps", "desktop", "resources", "bin", "multica"), "utf-8")).toBe("fixture CLI");
});

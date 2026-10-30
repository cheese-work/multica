import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";

test("image checks need only a shallow head and the exact base, not full history", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "image-budget-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const source = join(dir, "source");
  const checkout = join(dir, "checkout");
  mkdirSync(source);
  const env = {
    ...process.env,
    GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_AUTHOR_NAME: "CI test", GIT_AUTHOR_EMAIL: "ci@example.invalid",
    GIT_COMMITTER_NAME: "CI test", GIT_COMMITTER_EMAIL: "ci@example.invalid",
  };
  const git = (cwd, ...args) => execFileSync("git", args, { cwd, env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  git(source, "init", "-b", "main");
  writeFileSync(join(source, "existing.png"), Buffer.alloc(400 * 1024));
  git(source, "add", ".");
  git(source, "commit", "-m", "base");
  const base = git(source, "rev-parse", "HEAD");
  // Put an intermediate commit between base and head to prove no ancestry is needed.
  writeFileSync(join(source, "README.md"), "intermediate\n");
  git(source, "add", ".");
  git(source, "commit", "-m", "intermediate");
  const intermediate = git(source, "rev-parse", "HEAD");
  writeFileSync(join(source, "new.png"), Buffer.alloc(350 * 1024));
  git(source, "add", ".");
  git(source, "commit", "-m", "head");
  git(dir, "clone", "--depth=1", pathToFileURL(source).href, checkout);
  git(checkout, "fetch", "--no-tags", "--depth=1", "origin", base);
  assert.equal(git(checkout, "rev-parse", "--is-shallow-repository"), "true");
  assert.notEqual(spawnSync("git", ["cat-file", "-e", intermediate], { cwd: checkout, env }).status, 0);

  const script = fileURLToPath(new URL("./check-image-budget.mjs", import.meta.url));
  const check = (body = "") => spawnSync(process.execPath, [script, "--base", base], {
    cwd: checkout, env: { ...env, PR_BODY: body }, encoding: "utf8",
  });
  const oversized = check();
  assert.equal(oversized.status, 1, oversized.stderr);
  assert.match(oversized.stdout, /new\.png/);
  assert.doesNotMatch(oversized.stdout, /existing\.png/);
  assert.equal(check("Oversized image exemption: required test fixture").status, 0);
  writeFileSync(join(checkout, "new.png"), Buffer.alloc(100));
  writeFileSync(join(checkout, "existing.png"), Buffer.alloc(350 * 1024));
  assert.equal(check().status, 0, "unchanged or shrinking oversized files should pass");
});

// A pull_request job checks out GitHub's merge of the PR head into the base
// branch *as it is now*, while github.event.pull_request.base.sha is the base
// as it was when the event fired. Comparing the merge checkout with that older
// base charges the PR for every image `main` gained in between.
test("--base merge-parent compares against the base the merge was made with", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "image-budget-merge-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const env = {
    ...process.env,
    GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_AUTHOR_NAME: "CI test", GIT_AUTHOR_EMAIL: "ci@example.invalid",
    GIT_COMMITTER_NAME: "CI test", GIT_COMMITTER_EMAIL: "ci@example.invalid",
  };
  const git = (...args) => execFileSync("git", args, { cwd: dir, env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  const script = fileURLToPath(new URL("./check-image-budget.mjs", import.meta.url));
  const check = (base, body = "") => spawnSync(process.execPath, [script, "--base", base], {
    cwd: dir, env: { ...env, PR_BODY: body }, encoding: "utf8",
  });

  git("init", "-b", "main");
  writeFileSync(join(dir, "README.md"), "base\n");
  git("add", ".");
  git("commit", "-m", "base");
  const staleBase = git("rev-parse", "HEAD");
  // The PR branches here and changes no image.
  git("checkout", "-b", "pr");
  writeFileSync(join(dir, "feature.txt"), "feature\n");
  git("add", ".");
  git("commit", "-m", "feature");
  // Meanwhile main gains a large image the PR knows nothing about.
  git("checkout", "main");
  writeFileSync(join(dir, "design.png"), Buffer.alloc(350 * 1024));
  git("add", ".");
  git("commit", "-m", "main gains an image");
  const newBase = git("rev-parse", "HEAD");
  // GitHub's merge ref: first parent is the base, second is the PR head.
  git("merge", "--no-ff", "-m", "merge ref", "pr");
  assert.equal(git("rev-parse", "HEAD^1"), newBase);

  const stale = check(staleBase);
  assert.equal(stale.status, 1, "the stale event base charges the PR for main's image");
  assert.match(stale.stdout, /design\.png/);
  const fresh = check("merge-parent");
  assert.equal(fresh.status, 0, fresh.stdout + fresh.stderr);
  assert.match(fresh.stdout, new RegExp(newBase.slice(0, 7)));

  // The PR's own oversized image is still caught.
  git("checkout", "-b", "pr2", "pr");
  writeFileSync(join(dir, "mine.png"), Buffer.alloc(350 * 1024));
  git("add", ".");
  git("commit", "-m", "pr adds an image");
  git("checkout", "main");
  git("merge", "--no-ff", "-m", "merge ref 2", "pr2");
  const own = check("merge-parent");
  assert.equal(own.status, 1);
  assert.match(own.stdout, /mine\.png/);
  assert.doesNotMatch(own.stdout, /design\.png/);

  // Not a merge commit: refuse rather than compare against an arbitrary parent.
  git("checkout", "pr");
  const notMerge = check("merge-parent");
  assert.notEqual(notMerge.status, 0);
  assert.match(notMerge.stderr, /merge commit/);
});

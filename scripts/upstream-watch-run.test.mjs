import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const RUN = new URL('../deploy/upstream-watch/run.sh', import.meta.url).pathname;
const git = (cwd, ...args) => execFileSync('git', ['-c', 'user.name=t', '-c', 'user.email=t@t', ...args], { cwd, stdio: 'pipe' }).toString();

// Fake origin: main, and a branch. The "watcher script" just prints which ref it came from.
const origin = ({ mainHasScript }) => {
  const root = mkdtempSync(join(tmpdir(), 'uw-run-'));
  const repo = join(root, 'origin');
  mkdirSync(join(repo, 'scripts'), { recursive: true });
  git(root, 'init', '-q', '-b', 'main', repo);
  const put = (ref) => { mkdirSync(join(repo, 'scripts'), { recursive: true }); writeFileSync(join(repo, 'scripts/upstream-pr-watch.mjs'), `console.log('ran from ${ref}');\n`); git(repo, 'add', '.'); git(repo, 'commit', '-qm', ref); };
  writeFileSync(join(repo, 'README'), 'x');
  git(repo, 'add', '.'); git(repo, 'commit', '-qm', 'base');
  git(repo, 'checkout', '-qb', 'feature');
  put('feature');
  git(repo, 'checkout', '-q', 'main');
  if (mainHasScript) put('main');
  return { root, repo };
};
const poll = ({ root, repo }, ref = 'feature') => execFileSync('bash', [RUN], {
  env: { ...process.env, UPSTREAM_WATCH_REMOTE: repo, UPSTREAM_WATCH_REF: ref, UPSTREAM_WATCH_DIR: join(root, 'checkout') }, stdio: 'pipe',
}).toString().trim();

test('run.sh: before the PR lands, a pinned branch is what runs', () => {
  assert.equal(poll(origin({ mainHasScript: false })), 'ran from feature');
});

test('run.sh: once main carries the watcher, the pinned branch is dropped and main runs, even if the branch still exists', () => {
  const o = origin({ mainHasScript: false });
  assert.equal(poll(o), 'ran from feature');
  mkdirSync(join(o.repo, 'scripts'), { recursive: true });
  writeFileSync(join(o.repo, 'scripts/upstream-pr-watch.mjs'), "console.log('ran from main');\n");
  git(o.repo, 'add', '.'); git(o.repo, 'commit', '-qm', 'main lands');
  assert.equal(poll(o), 'ran from main');
});

test('run.sh: a deleted pinned branch falls back to main', () => {
  const o = origin({ mainHasScript: true });
  git(o.repo, 'branch', '-D', 'feature');
  assert.equal(poll(o), 'ran from main');
});

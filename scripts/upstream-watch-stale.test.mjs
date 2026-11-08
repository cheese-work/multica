import test from 'node:test';
import assert from 'node:assert/strict';
import { execFile, execFileSync } from 'node:child_process';
import { chmodSync, mkdtempSync, readFileSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';

// CHE-3960: X99's installed run.sh only ever runs scripts/upstream-pr-watch.mjs from the fresh
// checkout, so the weekly ledger stale check has to start from that entry, not from run.sh.
const WATCHER = new URL('./upstream-pr-watch.mjs', import.meta.url).pathname;
const git = (cwd, ...args) => execFileSync('git', ['-c', 'user.name=t', '-c', 'user.email=t@t', ...args], { cwd, stdio: 'pipe' }).toString();

// Fake `multica`: logs every call; `issue search` answers with SEARCH (a JSON file) when set.
const world = ({ existing = [] } = {}) => {
  const root = mkdtempSync(join(tmpdir(), 'uw-stale-'));
  const upstream = join(root, 'upstream');
  git(root, 'init', '-q', '-b', 'main', upstream);
  git(upstream, 'commit', '-q', '--allow-empty', '-m', 'upstream moved');
  writeFileSync(join(root, 'ledger.md'), '# no PR rows\n');
  writeFileSync(join(root, 'search.json'), JSON.stringify({ issues: existing }));
  const bin = join(root, 'bin');
  execFileSync('mkdir', ['-p', bin]);
  writeFileSync(join(bin, 'multica'), `#!/bin/sh\necho "$*" | tr "\\n" " " >> "${root}/calls.log"; echo >> "${root}/calls.log"\n[ "$2" = search ] && cat "${root}/search.json"\nexit 0\n`);
  chmodSync(join(bin, 'multica'), 0o755);
  const run = () => promisify(execFile)('node', [WATCHER, '--ledger', join(root, 'ledger.md'), '--state', join(root, 'state.json')], {
    env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, FORK_UPSTREAM_REMOTE: upstream }, timeout: 30_000,
  });
  const calls = () => (existsSync(join(root, 'calls.log')) ? readFileSync(join(root, 'calls.log'), 'utf8').trim().split('\n') : []);
  return { run, calls };
};

test('watcher entry: a stale ledger files one refresh issue for the dev team in the setup project', async () => {
  const w = world();
  await w.run();
  const creates = w.calls().filter((c) => c.startsWith('issue create'));
  assert.equal(creates.length, 1);
  assert.match(creates[0], /--title chore\(fork\): refresh docs\/fork ledger \(weekly stale check\)/);
  assert.match(creates[0], /--assignee-id a1073a68-4965-44ff-a8fb-244341935110/);
  assert.match(creates[0], /--project 5e621e12-eeb4-40a5-9571-25657d5b0ec5/);
});

test('watcher entry: a recent refresh issue with the same title means no second one', async () => {
  const w = world({ existing: [{ title: 'chore(fork): refresh docs/fork ledger (weekly stale check)', created_at: new Date().toISOString() }] });
  await w.run();
  assert.equal(w.calls().filter((c) => c.startsWith('issue create')).length, 0);
});

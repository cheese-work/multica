import test from 'node:test';
import assert from 'node:assert/strict';
import { check } from './fork-ledger-stale.mjs';

const SHA = 'a'.repeat(40);
const ledger = (sha = SHA, when = '2026-11-02 19:02') =>
  `- **Upstream state**, checked against upstream \`main\` \`${sha}\` (${when} ICT):\n` +
  '| 3 | FX131 | Task tokens can edit | #573 | 10 | yes | none | security-class; details withheld until upstream fixes it (private report pending) | - |\n' +
  '| 4 | FX120 | Codex effort | #538 | 17 | yes | none | upstream/main x.go:1 | - |\n';
const NOW = new Date('2026-11-05T12:00:00+07:00');

test('fresh pin equal to upstream head is not stale', () => {
  const r = check({ ledger: ledger(), upstreamHead: SHA, now: NOW });
  assert.equal(r.stale, false);
  assert.deepEqual(r.reasons, []);
});

test('pin behind upstream head is stale', () => {
  const r = check({ ledger: ledger(), upstreamHead: 'b'.repeat(40), now: NOW });
  assert.equal(r.stale, true);
  assert.match(r.reasons.join('\n'), /upstream main moved/);
});

test('pin older than 7 days is stale even when upstream did not move', () => {
  const r = check({ ledger: ledger(), upstreamHead: SHA, now: new Date('2026-11-10T00:00:00+07:00') });
  assert.equal(r.stale, true);
  assert.match(r.reasons.join('\n'), /older than 7 days/);
});

test('missing pin is stale', () => {
  const r = check({ ledger: '# nothing', upstreamHead: SHA, now: NOW });
  assert.equal(r.stale, true);
  assert.match(r.reasons.join('\n'), /no upstream pin/);
});

test('security-class rows are listed by id only', () => {
  const r = check({ ledger: ledger(), upstreamHead: SHA, now: NOW });
  assert.deepEqual(r.withheld, ['FX131']);
});

// Terra review P1: PROCESS.md names a weekly run, so a scheduled workflow must invoke the script. A red scheduled run on main is
// what the Monday brief's CI-health scan (red_scheduled) turns into a dev-team issue; nothing else calls the check.
import { readFileSync } from 'node:fs';
const wf = readFileSync(new URL('../.github/workflows/fork-ledger-stale.yml', import.meta.url), 'utf8');

test('weekly workflow runs the stale check on a schedule only', () => {
  assert.match(wf, /^on:\n {2}schedule:\n {4}- cron: "[^"]+"/m);
  assert.doesNotMatch(wf, /pull_request|workflow_dispatch|push:/);
  assert.match(wf, /run: node scripts\/fork-ledger-stale\.mjs/);
  assert.match(wf, /^permissions:\n {2}contents: read$/m);
});

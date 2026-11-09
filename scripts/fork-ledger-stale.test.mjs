import test from 'node:test';
import assert from 'node:assert/strict';
import { check, fileRefresh, TITLE } from './fork-ledger-stale.mjs';

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

// Terra review P1 (rounds 1-2): the stale result must reach an owner without depending on repo activity, so the X99 watcher
// (every 15 min, Multica credentials) files one issue. Dedup: skip when an issue with the exact title is under 7 days old,
// open or closed, so a cancelled refresh issue is not refiled every poll.
const NOW2 = new Date('2026-11-09T09:00:00+07:00');
const ids = { assigneeId: 'squad-id', project: 'project-id', now: NOW2 };
const stale = { stale: true, reasons: ['upstream main moved: pin aaaaaaaaaa, now bbbbbbbbbb'], withheld: ['FX131'] };
const recorder = (issues) => {
  const calls = [];
  return { calls, run: (args) => { calls.push(args); return args[1] === 'search' ? JSON.stringify({ issues }) : ''; } };
};

test('stale ledger files one refresh issue for the squad in the project', () => {
  const { calls, run } = recorder([]);
  assert.equal(fileRefresh(stale, { ...ids, run }), 'filed');
  const create = calls.find((a) => a[1] === 'create');
  assert.ok(create.includes(TITLE));
  assert.equal(create[create.indexOf('--assignee-id') + 1], 'squad-id');
  assert.equal(create[create.indexOf('--project') + 1], 'project-id');
  assert.match(create[create.indexOf('--description') + 1], /upstream main moved/);
});

test('fresh ledger files nothing', () => {
  const { calls, run } = recorder([]);
  assert.equal(fileRefresh({ stale: false, reasons: [], withheld: [] }, { ...ids, run }), 'fresh');
  assert.equal(calls.length, 0);
});

test('an issue with the exact title under 7 days old, even closed, blocks a second one', () => {
  const { calls, run } = recorder([{ title: TITLE, created_at: '2026-11-05T00:00:00Z' }]);
  assert.equal(fileRefresh(stale, { ...ids, run }), 'recent');
  assert.equal(calls.some((a) => a[1] === 'create'), false);
});

test('an older issue, or a different title, does not block', () => {
  const { run } = recorder([{ title: TITLE, created_at: '2026-10-20T00:00:00Z' }, { title: `${TITLE} x`, created_at: '2026-11-08T00:00:00Z' }]);
  assert.equal(fileRefresh(stale, { ...ids, run }), 'filed');
});

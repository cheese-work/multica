import test from 'node:test';
import assert from 'node:assert/strict';
import { currentWindow, eligible, renderAsk } from './tier2-merge-card.mjs';

const HEAD = 'a'.repeat(40);
const pr = (over = {}) => ({
  number: 10, url: 'https://github.com/cheese-work/multica/pull/10', title: 'feat(x): y [squash]',
  headSha: HEAD, isDraft: false,
  receipt: { tier: 2, head: HEAD, error: null, ci: { head_sha: HEAD, passed: true } },
  securityGate: { sha: HEAD, state: 'success' },
  ...over,
});

test('lists a ready Tier 2 PR with green checks and a PASS on its exact head', () => {
  assert.deepEqual(eligible([pr()]).map((p) => p.number), [10]);
});

test('drops a PR whose head moved after the receipt, checks or PASS', () => {
  const moved = 'b'.repeat(40);
  assert.deepEqual(eligible([pr({ headSha: moved })]), []);
  assert.deepEqual(eligible([pr({ receipt: { tier: 2, head: HEAD, ci: { head_sha: moved, passed: true } } })]), []);
  assert.deepEqual(eligible([pr({ securityGate: { sha: moved, state: 'success' } })]), []);
});

test('drops drafts, Tier 0/1, red or missing checks, and a missing or failed PASS', () => {
  assert.deepEqual(eligible([pr({ isDraft: true })]), []);
  for (const tier of [0, 1]) assert.deepEqual(eligible([pr({ receipt: { tier, head: HEAD, ci: { head_sha: HEAD, passed: true } } })]), []);
  assert.deepEqual(eligible([pr({ receipt: { tier: 2, head: HEAD, ci: { head_sha: HEAD, passed: false } } })]), []);
  assert.deepEqual(eligible([pr({ receipt: null })]), []);
  assert.deepEqual(eligible([pr({ securityGate: { sha: HEAD, state: 'pending' } })]), []);
  assert.deepEqual(eligible([pr({ securityGate: null })]), []);
});

test('a merged PR is never listed: merged PRs are not open and drop out of the input', () => {
  assert.deepEqual(eligible([pr({ state: 'MERGED' })]), []);
});

test('card shows exact heads, per-PR links, merge all and merge none', () => {
  const ask = renderAsk([pr(), pr({ number: 11, url: 'https://github.com/cheese-work/multica/pull/11', headSha: 'c'.repeat(40) })], '07:30');
  assert.match(ask.decision, /2 Tier 2 PRs/);
  assert.ok(ask.context_eli10.includes(`[#10](https://github.com/cheese-work/multica/pull/10) \`${HEAD}\``));
  assert.ok(ask.context_eli10.includes('`' + 'c'.repeat(40) + '`'));
  assert.deepEqual(ask.options.map((o) => o.label), ['Merge all 2', 'Merge none now']);
  assert.ok(ask.options.length >= 2 && ask.recommendation && ask.default_if_no_reply);
});

test('picks the window nearest at or before now, in ICT', () => {
  const at = (iso) => currentWindow(new Date(iso));
  assert.equal(at('2026-11-04T00:30:00Z'), '07:30'); // 07:30 ICT
  assert.equal(at('2026-11-04T05:30:00Z'), '12:30');
  assert.equal(at('2026-11-04T13:00:00Z'), '20:00');
  assert.equal(at('2026-11-04T02:00:00Z'), '07:30'); // 09:00 ICT, between windows: latest passed window
  assert.equal(at('2026-11-03T18:00:00Z'), '20:00'); // 01:00 ICT, overnight: last evening's window
});

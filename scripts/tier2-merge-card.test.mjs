import test from 'node:test';
import assert from 'node:assert/strict';
import * as card from './tier2-merge-card.mjs';

const { currentWindow, eligible, renderAsk, sendCard } = card;

const HEAD = 'a'.repeat(40);
const pr = (over = {}) => ({
  number: 10, url: 'https://github.com/cheese-work/multica/pull/10', title: 'feat(x): y [squash]',
  headSha: HEAD, isDraft: false,
  receipt: { tier: 2, head: HEAD, error: null, classifier: { trusted: true }, ci: { head_sha: HEAD, passed: true } },
  securityGate: { sha: HEAD, state: 'success' },
  ...over,
});

test('lists a ready Tier 2 PR with green checks and a PASS on its exact head', () => {
  assert.deepEqual(eligible([pr()]).map((p) => p.number), [10]);
});

test('drops a PR whose head moved after the receipt, checks or PASS', () => {
  const moved = 'b'.repeat(40);
  assert.deepEqual(eligible([pr({ headSha: moved })]), []);
  assert.deepEqual(eligible([pr({ receipt: { ...pr().receipt, ci: { head_sha: moved, passed: true } } })]), []);
  assert.deepEqual(eligible([pr({ securityGate: { sha: moved, state: 'success' } })]), []);
});

test('drops drafts, Tier 0/1, red or missing checks, and a missing or failed PASS', () => {
  assert.deepEqual(eligible([pr({ isDraft: true })]), []);
  for (const tier of [0, 1]) assert.deepEqual(eligible([pr({ receipt: { ...pr().receipt, tier } })]), []);
  assert.deepEqual(eligible([pr({ receipt: { ...pr().receipt, ci: { head_sha: HEAD, passed: false } } })]), []);
  assert.deepEqual(eligible([pr({ receipt: null })]), []);
  assert.deepEqual(eligible([pr({ securityGate: { sha: HEAD, state: 'pending' } })]), []);
  assert.deepEqual(eligible([pr({ securityGate: null })]), []);
});

test('a merged PR is never listed: merged PRs are not open and drop out of the input', () => {
  assert.deepEqual(eligible([pr({ state: 'MERGED' })]), []);
});

test('card shows exact heads and per-PR links', () => {
  const ask = renderAsk([pr(), pr({ number: 11, url: 'https://github.com/cheese-work/multica/pull/11', headSha: 'c'.repeat(40) })], '07:30');
  assert.match(ask.decision, /2 Tier 2 PRs/);
  // The link text and URL name the head, so a link followed after a push shows a different commit.
  assert.ok(ask.context_eli10.includes(`[#10 @ ${HEAD.slice(0, 7)}](https://github.com/cheese-work/multica/pull/10/commits/${HEAD}) \`${HEAD}\``));
  assert.ok(ask.context_eli10.includes('`' + 'c'.repeat(40) + '`'));
  assert.ok(ask.options.length >= 2 && ask.recommendation && ask.default_if_no_reply);
  // `multica decision request` refuses two or more options without the exact label of the recommended one.
  assert.ok(ask.options.some((o) => o.label === ask.recommended_option));
  // ...and a UUID `source_issue_id` (seen live: "ask.source_issue_id must be a UUID").
  const sent = renderAsk([pr()], '07:30', 'ISSUE-UUID', 'COMMENT-UUID');
  assert.deepEqual([sent.source_issue_id, sent.source_comment_id], ['ISSUE-UUID', 'COMMENT-UUID']);
});

test('picks the window nearest at or before now, in ICT', () => {
  const at = (iso) => currentWindow(new Date(iso));
  assert.equal(at('2026-11-04T00:30:00Z'), '07:30'); // 07:30 ICT
  assert.equal(at('2026-11-04T05:30:00Z'), '12:30');
  assert.equal(at('2026-11-04T13:00:00Z'), '20:00');
  assert.equal(at('2026-11-04T02:00:00Z'), '07:30'); // 09:00 ICT, between windows: latest passed window
  assert.equal(at('2026-11-03T18:00:00Z'), '20:00'); // 01:00 ICT, overnight: last evening's window
});

test('rejects a receipt that failed to classify or came from an untrusted classifier', () => {
  const r = pr().receipt;
  assert.deepEqual(eligible([pr({ receipt: { ...r, error: 'RuntimeError: boom' } })]), []);
  assert.deepEqual(eligible([pr({ receipt: { ...r, classifier: { trusted: false } } })]), []);
  assert.deepEqual(eligible([pr({ receipt: { ...r, classifier: undefined } })]), []);
});

test('the card only lists: no agent merge step exists and no option has an agent merge', () => {
  for (const name of ['mergeListed', 'planMerge', 'mergeMethod', 'parseListed']) assert.equal(card[name], undefined, name);
  const ask = renderAsk([pr()], '07:30');
  const text = JSON.stringify(ask.options) + ask.decision + ask.recommendation + ask.default_if_no_reply;
  assert.doesNotMatch(text, /agent runs|--match-head-commit|tier2-merge-card\.mjs merge|gh pr merge/i);
  assert.match(ask.default_if_no_reply, /Nothing merges/);
});

// A sent card cannot change, so each run withdraws the still-open earlier card, re-reads the heads and posts a fresh one.
const OLD_CARD = { id: 'card-1', status: 'open', ask: renderAsk([pr()], '07:30').context_eli10 };
const harness = ({ cards = [OLD_CARD], collected, heads }) => {
  const calls = [];
  const io = {
    collect: async () => collected,
    heads: async () => new Map(heads),
    openCards: async () => cards,
    withdraw: async (id) => calls.push(['withdraw', id]),
    post: async (ask) => calls.push(['post', ask]),
  };
  return { calls, run: () => sendCard('cheese-work/multica', io) };
};

test('a push after the card was sent: next run withdraws the old card and drops the moved PR', async () => {
  const moved = 'b'.repeat(40);
  const h = harness({ collected: [pr(), pr({ number: 11, url: 'https://github.com/cheese-work/multica/pull/11' })], heads: [[10, HEAD], [11, moved]] });
  await h.run();
  assert.deepEqual(h.calls.map((c) => c[0]), ['withdraw', 'post']);
  assert.equal(h.calls[0][1], 'card-1');
  assert.doesNotMatch(h.calls[1][1].context_eli10, /pull\/11/);
  assert.match(h.calls[1][1].context_eli10, /pull\/10\/commits/);
});

test('with no PR left ready the old open card is still withdrawn and no new card is sent', async () => {
  const h = harness({ collected: [pr({ securityGate: { sha: HEAD, state: 'pending' } })], heads: [[10, HEAD]] });
  await h.run();
  assert.deepEqual(h.calls, [['withdraw', 'card-1']]);
});

test('only open merge cards are withdrawn: answered and unrelated asks stay', async () => {
  const cards = [OLD_CARD, { ...OLD_CARD, id: 'done', status: 'answered' }, { id: 'other', status: 'open', ask: 'Which option should the PR take?' }];
  const h = harness({ cards, collected: [pr()], heads: [[10, HEAD]] });
  await h.run();
  assert.deepEqual(h.calls.filter((c) => c[0] === 'withdraw'), [['withdraw', 'card-1']]);
});

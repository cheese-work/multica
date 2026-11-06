import test from 'node:test';
import assert from 'node:assert/strict';
import * as card from './tier2-merge-card.mjs';

const { currentWindow, eligible, renderAsk, sendCard, sweepCards, openCardsArgs, armSweepArgs, replaceSweep } = card;

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
    comment: async () => 'COMMENT-UUID',
    post: async (ask) => calls.push(['post', ask]),
    armSweep: async () => calls.push(['arm']),
    disarmSweep: async () => calls.push(['disarm']),
  };
  return { calls, run: () => sendCard('cheese-work/multica', io) };
};

test('a push after the card was sent: next run withdraws the old card and drops the moved PR', async () => {
  const moved = 'b'.repeat(40);
  const h = harness({ collected: [pr(), pr({ number: 11, url: 'https://github.com/cheese-work/multica/pull/11' })], heads: [[10, HEAD], [11, moved]] });
  await h.run();
  assert.deepEqual(h.calls.map((c) => c[0]), ['withdraw', 'arm', 'post']);
  assert.equal(h.calls[0][1], 'card-1');
  assert.doesNotMatch(h.calls[2][1].context_eli10, /pull\/11/);
  assert.match(h.calls[2][1].context_eli10, /pull\/10\/commits/);
});

test('with no PR left ready the old open card is still withdrawn and no new card is sent', async () => {
  const h = harness({ collected: [pr({ securityGate: { sha: HEAD, state: 'pending' } })], heads: [[10, HEAD]] });
  await h.run();
  assert.deepEqual(h.calls, [['withdraw', 'card-1'], ['disarm']]);
});

test('only open merge cards are withdrawn: answered and unrelated asks stay', async () => {
  const cards = [OLD_CARD, { ...OLD_CARD, id: 'done', status: 'answered' }, { id: 'other', status: 'open', ask: 'Which option should the PR take?' }];
  const h = harness({ cards, collected: [pr()], heads: [[10, HEAD]] });
  await h.run();
  assert.deepEqual(h.calls.filter((c) => c[0] === 'withdraw'), [['withdraw', 'card-1']]);
});

// The server lets an agent withdraw a card only from a running task on the card's own issue (decision_withdraw.go):
// every card hangs on one standing issue, and each window and sweep run is a wakeup run on that issue.
test('cards are listed on the standing issue, not by member, so each run may withdraw what it lists', () => {
  const args = openCardsArgs('ISSUE-UUID');
  assert.deepEqual(args.slice(0, 2), ['decision', 'list']);
  assert.ok(args.includes('--issue') && args.includes('ISSUE-UUID'));
  assert.ok(!args.includes('--for') && !args.includes('--state'));
});

test('a posted card carries the run comment id; the sweep is armed before the card is posted', async () => {
  const h = harness({ cards: [], collected: [pr()], heads: [[10, HEAD]] });
  await h.run();
  assert.deepEqual(h.calls.map((c) => c[0]), ['arm', 'post']);
  assert.equal(h.calls[1][1].source_comment_id, 'COMMENT-UUID');
});

// A card must never be open without a sweep watching it: arm first, and post nothing if arming fails.
test('if arming the sweep fails, no card is posted', async () => {
  const calls = [];
  const io = { collect: async () => [pr()], heads: async () => new Map([[10, HEAD]]), openCards: async () => [],
    withdraw: async () => {}, comment: async () => 'C', post: async () => calls.push('post'),
    armSweep: async () => { throw new Error('wakeup create failed'); }, disarmSweep: async () => {} };
  await assert.rejects(() => sendCard('cheese-work/multica', io), /wakeup create failed/);
  assert.deepEqual(calls, []);
});

test('if posting fails after arming, the armed sweep is kept and ends itself when it finds no card', async () => {
  const calls = [];
  const io = { collect: async () => [pr()], heads: async () => new Map([[10, HEAD]]), openCards: async () => [],
    withdraw: async () => {}, comment: async () => 'C', post: async () => { throw new Error('post failed'); },
    armSweep: async () => calls.push('arm'), disarmSweep: async () => calls.push('disarm') };
  await assert.rejects(() => sendCard('cheese-work/multica', io), /post failed/);
  assert.deepEqual(calls, ['arm']); // no disarm here: the 15-min sweep finds no open card and disables itself
});

// Replacing the sweep rule must not leave a gap: create the new rule first, then disable the old ones.
const rulesIo = ({ failCreate = false } = {}) => {
  const calls = [];
  const rules = [{ id: 'old', kind: 'every', enabled: true }, { id: 'cron', kind: 'cron', enabled: true }, { id: 'off', kind: 'every', enabled: false }];
  return { calls, io: {
    list: async () => rules,
    create: async () => { calls.push('create'); if (failCreate) throw new Error('create failed'); return { id: 'new' }; },
    disable: async (id) => calls.push('disable ' + id),
  } };
};

test('replacing the sweep creates the new rule first, then disables only the old enabled sweep rules', async () => {
  const { calls, io } = rulesIo();
  await replaceSweep(io);
  assert.deepEqual(calls, ['create', 'disable old']);
});

test('if creating the new sweep rule fails, the old rule stays enabled', async () => {
  const { calls, io } = rulesIo({ failCreate: true });
  await assert.rejects(() => replaceSweep(io), /create failed/);
  assert.deepEqual(calls, ['create']);
});

test('the sweep wakeup repeats on the card issue, ends by itself and runs only the sweep', () => {
  const args = armSweepArgs('ISSUE-UUID', 'MEMBER-UUID');
  assert.deepEqual(args.slice(0, 3), ['issue', 'wakeup', 'create']);
  assert.equal(args[3], 'ISSUE-UUID');
  const at = (flag) => args[args.indexOf(flag) + 1];
  assert.equal(at('--every'), '15m');
  assert.ok(at('--expires-in'));
  assert.match(at('--instruction'), /CARD_ISSUE=ISSUE-UUID CARD_FOR=MEMBER-UUID node scripts\/tier2-merge-card\.mjs sweep/);
});

// Between runs a push leaves the listed head on the open card: a frequent sweep withdraws a card whose listed head moved.
const sweepHarness = ({ cards, heads }) => {
  const calls = [];
  const io = { heads: async () => new Map(heads), openCards: async () => cards, withdraw: async (id) => calls.push(id), disarmSweep: async () => calls.push('disarm') };
  return { calls, run: () => sweepCards('cheese-work/multica', io) };
};
const cardFor = (...prs) => ({ id: 'card-' + prs.map((x) => x.number).join('-'), status: 'open', ask: renderAsk(prs, '07:30').context_eli10 });

test('sweep: a push to a listed PR withdraws the open card', async () => {
  const h = sweepHarness({ cards: [cardFor(pr())], heads: [[10, 'b'.repeat(40)]] });
  await h.run();
  assert.deepEqual(h.calls, ['card-10', 'disarm']);
});

test('sweep: unchanged heads keep the card, and a PR merged from the card does not withdraw it', async () => {
  const two = cardFor(pr(), pr({ number: 11, url: 'https://github.com/cheese-work/multica/pull/11', headSha: 'c'.repeat(40) }));
  const h = sweepHarness({ cards: [two], heads: [[10, HEAD], [11, 'c'.repeat(40)]] });
  await h.run();
  const merged = sweepHarness({ cards: [two], heads: [[10, HEAD]] }); // #11 merged: not open any more
  await merged.run();
  assert.deepEqual([h.calls, merged.calls], [[], []]); // cards still open: the sweep keeps running
});

test('sweep ignores answered and unrelated asks, and stops the wakeup once no merge card is open', async () => {
  const h = sweepHarness({ cards: [{ ...cardFor(pr()), status: 'answered' }, { id: 'x', status: 'open', ask: 'Pick one' }], heads: [[10, 'b'.repeat(40)]] });
  await h.run();
  assert.deepEqual(h.calls, ['disarm']);
});

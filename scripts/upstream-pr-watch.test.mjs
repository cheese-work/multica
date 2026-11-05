import test from 'node:test';
import assert from 'node:assert/strict';
import { watch, makeDeliver } from './upstream-pr-watch.mjs';

const HEAD = '| PR | Issue | Head SHA | State | Watermark | Convergence PR | Notes |\n| --- | --- | --- | --- | --- | --- | --- |\n';
const ledgerOf = (row) => `# Tracked upstream PRs\n\n${HEAD}${row}\n`;
const OPEN = '| multica-ai/multica#10 | CHE-1 | aaa | open | 2026-11-01T00:00:00Z | - | - |';
const NOW = new Date('2026-11-02T00:00:00Z');
const API = 'https://api.github.com/repos/multica-ai/multica';
const PR = 'multica-ai/multica#10';
const url = (path, page = 1) => `${API}/${path}?per_page=100&page=${page}`;
const COMMENTS = url('issues/10/comments');
const INLINE = url('pulls/10/comments');
const REVIEWS = url('pulls/10/reviews');

// Fake GitHub: world maps url -> { body, etag }; honours If-None-Match. Missing url = 404.
const fakeGet = (world, calls = []) => async (u, { etag } = {}) => {
  calls.push(u);
  const hit = world[u];
  if (!hit) return { status: 404, body: {} };
  if (hit.status) return hit;
  if (etag && etag === hit.etag) return { status: 304 };
  return { status: 200, etag: hit.etag, body: hit.body };
};

const pr = (over = {}) => ({ state: 'open', merged: false, merged_at: null, merge_commit_sha: null,
  created_at: '2026-10-31T00:00:00Z', head: { sha: 'aaa' }, html_url: `${API}/pull/10`, ...over });
const world = (over = {}) => ({
  [`${API}/pulls/10`]: { etag: 'p1', body: pr() },
  [COMMENTS]: { etag: 'i1', body: [] },
  [INLINE]: { etag: 'c1', body: [] },
  [REVIEWS]: { etag: 'r1', body: [] },
  ...over,
});
const review = (over = {}) => ({ id: 1, user: { login: 'bohan' }, state: 'CHANGES_REQUESTED', body: 'please rename', submitted_at: '2026-11-01T10:00:00Z', ...over });
const inline = (over = {}) => ({ id: 2, user: { login: 'bohan' }, body: 'nit: use const', path: 'a.go', updated_at: '2026-11-01T11:00:00Z', ...over });
const fullPage = (n, over = {}) => Array.from({ length: 100 }, (_, i) => ({ id: n * 1000 + i, user: { login: 'bohan' }, body: `c${i}`, path: 'a.go', updated_at: '2026-10-31T01:00:00Z', ...over }));

// The poller never writes the ledger: its observations live in state.rows (overlay).
const run = async (w, { md = ledgerOf(OPEN), state = {}, now = NOW, deliver } = {}) => {
  const dispatched = [];
  let saved = state;
  const calls = [];
  const result = await watch({
    md, state, now, self: ['congvc-dev'], get: fakeGet(w, calls),
    deliver: deliver ?? (async (issue, text, key) => { dispatched.push({ issue, text, key }); }),
    save: (s) => { saved = JSON.parse(JSON.stringify(s)); },
  });
  return { ...result, dispatched, saved, calls, md, row: saved.rows?.[PR] };
};
const again = (r, over = {}) => ({ md: r.md, state: r.saved, ...over });

test('new submitted review: exactly one continuation on the owning issue, scope A wording', async () => {
  const r = await run(world({ [REVIEWS]: { etag: 'r2', body: [review()] } }));
  assert.equal(r.dispatched.length, 1);
  assert.equal(r.dispatched[0].issue, 'CHE-1');
  assert.match(r.dispatched[0].text, /please rename/);
  assert.match(r.dispatched[0].text, /new approval/);
  assert.equal(r.row.watermark, '2026-11-01T10:00:00Z');
});

test('new inline comment and a review in one poll still make one continuation', async () => {
  const r = await run(world({ [REVIEWS]: { etag: 'r2', body: [review()] }, [INLINE]: { etag: 'c2', body: [inline()] } }));
  assert.equal(r.dispatched.length, 1);
  assert.match(r.dispatched[0].text, /nit: use const/);
  assert.match(r.dispatched[0].text, /please rename/);
  assert.equal(r.row.watermark, '2026-11-01T11:00:00Z');
});

test('no change: 304 everywhere, one request per list, no dispatch', async () => {
  const w = world();
  const first = await run(w);
  const second = await run(w, again(first));
  assert.equal(second.dispatched.length, 0);
  assert.deepEqual(second.saved, first.saved);
  assert.equal(second.calls.length, 4);
});

test('the ledger file is never rewritten by the poller', async () => {
  const r = await run(world({ [REVIEWS]: { etag: 'r2', body: [review()] } }));
  assert.equal(r.md, ledgerOf(OPEN));
});

test('same event seen twice (overlapping watermark) dispatches once', async () => {
  const first = await run(world({ [REVIEWS]: { etag: 'r2', body: [review()] } }));
  const second = await run(world({ [REVIEWS]: { etag: 'r3', body: [review()] } }), again(first));
  assert.equal(second.dispatched.length, 0);
});

test('an edited comment (new updated_at) is a new content version', async () => {
  const first = await run(world({ [INLINE]: { etag: 'c2', body: [inline()] } }));
  const second = await run(world({ [INLINE]: { etag: 'c3', body: [inline({ body: 'edited', updated_at: '2026-11-01T12:00:00Z' })] } }), again(first));
  assert.equal(second.dispatched.length, 1);
  assert.match(second.dispatched[0].text, /edited/);
});

test('our own comments and empty approvals do not wake an agent', async () => {
  const r = await run(world({
    [COMMENTS]: { etag: 'i2', body: [{ id: 3, user: { login: 'congvc-dev' }, body: 'thanks', updated_at: '2026-11-01T09:00:00Z' }] },
    [REVIEWS]: { etag: 'r2', body: [review({ id: 4, state: 'APPROVED', body: '' })] },
  }));
  assert.equal(r.dispatched.length, 0);
});

test('bot comments (vercel, CI) do not wake an agent', async () => {
  const r = await run(world({ [COMMENTS]: { etag: 'i2', body: [{ id: 6, user: { login: 'vercel[bot]' }, body: 'deploy needs auth', updated_at: '2026-11-01T09:00:00Z' }] } }));
  assert.equal(r.dispatched.length, 0);
});

test('a full page makes the next page get requested; its comment is found', async () => {
  const r = await run(world({
    [INLINE]: { etag: 'c2', body: fullPage(1) },
    [url('pulls/10/comments', 2)]: { etag: 'c2b', body: [inline()] },
  }));
  assert.equal(r.dispatched.length, 1);
  assert.match(r.dispatched[0].text, /nit: use const/);
});

test('page 1 unchanged (304) while a later page gained or edited a comment: still found', async () => {
  const page2 = url('pulls/10/comments', 2);
  const w1 = world({ [INLINE]: { etag: 'c2', body: fullPage(1) }, [page2]: { etag: 'p2a', body: [] } });
  const first = await run(w1);
  assert.equal(first.dispatched.length, 0);
  const w2 = world({ [INLINE]: { etag: 'c2', body: fullPage(1) }, [page2]: { etag: 'p2b', body: [inline({ updated_at: '2026-11-01T13:00:00Z' })] } });
  const second = await run(w2, again(first));
  assert.equal(second.dispatched.length, 1);
  assert.ok(second.calls.includes(page2));
});

test('a full unchanged page still probes the next page for new comments', async () => {
  const page2 = url('pulls/10/comments', 2);
  const w = world({ [INLINE]: { etag: 'c2', body: fullPage(1) }, [page2]: { etag: 'p2a', body: [] } });
  const first = await run(w);
  const second = await run(w, again(first));
  assert.ok(second.calls.includes(page2));
});

test('merged upstream: overlay records PR and merge SHA, one continuation with the exact ledger row to commit', async () => {
  const r = await run(world({ [`${API}/pulls/10`]: { etag: 'p2', body: pr({ state: 'closed', merged: true, merged_at: '2026-11-01T20:00:00Z', merge_commit_sha: 'deadbeef' }) } }));
  assert.equal(r.row.state, 'merged upstream');
  assert.match(r.row.notes, /deadbeef/);
  assert.equal(r.dispatched.length, 1);
  assert.match(r.dispatched[0].text, /convergence PR/);
  assert.match(r.dispatched[0].text, /migration gate/);
  assert.match(r.dispatched[0].text, /\| multica-ai\/multica#10 \| CHE-1 \| aaa \| merged upstream \|/);
});

test('closed unmerged: close reason recorded from the last reviewer comment', async () => {
  const r = await run(world({
    [`${API}/pulls/10`]: { etag: 'p2', body: pr({ state: 'closed' }) },
    [COMMENTS]: { etag: 'i2', body: [{ id: 5, user: { login: 'bohan' }, body: 'out of scope, closing', updated_at: '2026-11-01T18:00:00Z' }] },
  }));
  assert.equal(r.row.state, 'closed unmerged');
  assert.match(r.row.notes, /out of scope, closing/);
  assert.match(r.dispatched[0].text, /\| closed unmerged \|/);
});

test('merged row with a landed convergence PR becomes integrated into fork main (one continuation to commit the row)', async () => {
  const md = ledgerOf('| multica-ai/multica#10 | CHE-1 | aaa | merged upstream | 2026-11-01T20:00:00Z | cheese-work/multica#99 | merged |');
  const r = await run({ 'https://api.github.com/repos/cheese-work/multica/pulls/99': { etag: 'f1', body: { state: 'closed', merged: true } } }, { md });
  assert.equal(r.row.state, 'integrated into fork main');
  assert.equal(r.dispatched.length, 1);
  assert.match(r.dispatched[0].text, /integrated into fork main/);
  const again2 = await run({}, again(r));
  assert.equal(again2.calls.length, 0);
});

test('an owner-committed ledger state ahead of the overlay wins; terminal rows are not polled', async () => {
  const md = ledgerOf('| multica-ai/multica#10 | CHE-1 | aaa | closed unmerged | 2026-11-01T20:00:00Z | - | x |');
  const r = await run({}, { md, state: { rows: { [PR]: { head: 'aaa', state: 'open', watermark: '-', notes: '-' } } } });
  assert.equal(r.calls.length, 0);
});

test('7 days without a response: one follow-up proposal, never repeated', async () => {
  const w = world();
  const first = await run(w, { now: new Date('2026-11-09T00:00:01Z') });
  assert.equal(first.dispatched.length, 1);
  assert.match(first.dispatched[0].text, /follow-up/);
  const second = await run(w, again(first, { now: new Date('2026-11-12T00:00:00Z') }));
  assert.equal(second.dispatched.length, 0);
});

test('a failing request marks monitoring stale and keeps going; recovery clears it', async () => {
  const bad = await run(world({ [REVIEWS]: { status: 500, body: {} } }));
  assert.equal(bad.ok, false);
  assert.match(bad.saved.stale.error, /500/);
  const good = await run(world(), again(bad));
  assert.equal(good.ok, true);
  assert.equal(good.saved.stale, undefined);
});

test('rate limit: backoff until reset, later polls make no requests', async () => {
  const w = world({ [`${API}/pulls/10`]: { status: 403, remaining: '0', reset: 1793577600, body: {} } });
  const limited = await run(w, { now: new Date('2026-11-01T23:00:00Z') });
  assert.equal(limited.ok, false);
  assert.equal(limited.saved.backoffUntil, '2026-11-02T00:00:00.000Z');
  const skipped = await run(world(), again(limited, { now: new Date('2026-11-01T23:30:00Z') }));
  assert.equal(skipped.calls.length, 0);
  assert.equal(skipped.ok, false);
});

test('delivery failure: nothing is committed, the next run retries from the saved outbox without re-polling', async () => {
  const w = world({ [REVIEWS]: { etag: 'r2', body: [review()] } });
  const failing = await run(w, { deliver: async () => { throw new Error('multica down'); } });
  assert.equal(failing.ok, false);
  assert.equal(failing.saved.pending.length, 1);
  assert.deepEqual(failing.saved.rows ?? {}, {});
  assert.deepEqual(failing.saved.prs ?? {}, {});
  const retry = await run(world(), again(failing));
  assert.equal(retry.dispatched.length, 1);
  assert.match(retry.dispatched[0].text, /please rename/);
  assert.equal(retry.saved.pending.length, 0);
  assert.equal(retry.row.watermark, '2026-11-01T10:00:00Z');
  assert.equal(retry.calls.length, 4);
  const third = await run(world({ [REVIEWS]: { etag: 'r2', body: [review()] } }), again(retry));
  assert.equal(third.dispatched.length, 0);
});

test('a crash after delivery but before the commit re-sends with the same key (deliver dedupes)', async () => {
  const w = world({ [REVIEWS]: { etag: 'r2', body: [review()] } });
  const keys = [];
  const first = await run(w, { deliver: async (_i, _t, key) => { keys.push(key); throw new Error('crashed after create'); } });
  const second = await run(world(), { ...again(first), deliver: async (_i, _t, key) => { keys.push(key); } });
  assert.equal(keys.length, 2);
  assert.equal(keys[0], keys[1]);
  assert.match(keys[0], /^[0-9a-f]{12}$/);
  assert.equal(second.ok, true);
});

// ---- real delivery contract (injected `multica`) ---------------------------
const cli = (existing = []) => {
  const calls = [];
  const exec = async (args, input) => {
    calls.push({ args, input });
    if (args[1] === 'get') return JSON.stringify({ assignee_type: 'agent', assignee_id: 'agent-1' });
    if (args[2] === 'list') return JSON.stringify(existing);
    return '{}';
  };
  return { calls, exec };
};

test('deliver: one wakeup, kind at, aimed at the issue assignee, marker in the instruction', async () => {
  const { calls, exec } = cli();
  await makeDeliver(exec)('CHE-1', 'Upstream PR x has new review activity', 'abc123abc123');
  const create = calls.find((c) => c.args[2] === 'create');
  assert.deepEqual(create.args.slice(0, 4), ['issue', 'wakeup', 'create', 'CHE-1']);
  for (const [flag, value] of [['--kind', 'at'], ['--mode', 'once'], ['--after', '1m'], ['--agent-id', 'agent-1']]) {
    assert.equal(create.args[create.args.indexOf(flag) + 1], value, flag);
  }
  assert.match(create.input, /\[upstream-watch:abc123abc123\]$/);
});

test('deliver: marker already on the issue means no second wakeup', async () => {
  const { calls, exec } = cli([{ id: 'w1', instruction: 'old text\n\n[upstream-watch:abc123abc123]' }]);
  await makeDeliver(exec)('CHE-1', 'text', 'abc123abc123');
  assert.equal(calls.filter((c) => c.args[2] === 'create').length, 0);
});

test('deliver: an issue not assigned to an agent is an error, not a silent wakeup of the wrong target', async () => {
  const exec = async (args) => (args[1] === 'get' ? JSON.stringify({ assignee_type: 'member', assignee_id: 'u1' }) : '[]');
  await assert.rejects(makeDeliver(exec)('CHE-1', 'text', 'k'), /not assigned to an agent/);
});

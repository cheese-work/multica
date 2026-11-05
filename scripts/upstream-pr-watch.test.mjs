import test from 'node:test';
import assert from 'node:assert/strict';
import { parseLedger, watch } from './upstream-pr-watch.mjs';

const HEAD = '| PR | Issue | Head SHA | State | Watermark | Convergence PR | Notes |\n| --- | --- | --- | --- | --- | --- | --- |\n';
const ledgerOf = (row) => `# Tracked upstream PRs\n\n${HEAD}${row}\n`;
const OPEN = '| multica-ai/multica#10 | CHE-1 | aaa | open | 2026-11-01T00:00:00Z | - | - |';
const NOW = new Date('2026-11-02T00:00:00Z');
const API = 'https://api.github.com/repos/multica-ai/multica';

// Fake GitHub: world maps url -> { body, etag }; honours If-None-Match.
const fakeGet = (world, calls = []) => async (url, { etag } = {}) => {
  calls.push(url);
  const hit = world[url];
  if (!hit) return { status: 404, body: {} };
  if (hit.status) return hit;
  if (etag && etag === hit.etag) return { status: 304 };
  return { status: 200, etag: hit.etag, body: hit.body, next: hit.next };
};

const pr = (over = {}) => ({ state: 'open', merged: false, merged_at: null, merge_commit_sha: null,
  created_at: '2026-10-31T00:00:00Z', head: { sha: 'aaa' }, html_url: `${API}/pull/10`, ...over });
const world = (over = {}) => ({
  [`${API}/pulls/10`]: { etag: 'p1', body: pr() },
  [`${API}/issues/10/comments?per_page=100`]: { etag: 'i1', body: [] },
  [`${API}/pulls/10/comments?per_page=100`]: { etag: 'c1', body: [] },
  [`${API}/pulls/10/reviews?per_page=100`]: { etag: 'r1', body: [] },
  ...over,
});
const review = (over = {}) => ({ id: 1, user: { login: 'bohan' }, state: 'CHANGES_REQUESTED', body: 'please rename', submitted_at: '2026-11-01T10:00:00Z', ...over });
const inline = (over = {}) => ({ id: 2, user: { login: 'bohan' }, body: 'nit: use const', path: 'a.go', updated_at: '2026-11-01T11:00:00Z', ...over });

const run = async (w, { md = ledgerOf(OPEN), state = {}, now = NOW } = {}) => {
  const dispatched = [];
  let saved = { md, state };
  const calls = [];
  const result = await watch({
    md, state, now, self: ['congvc-dev'], get: fakeGet(w, calls),
    dispatch: async (issue, text) => { dispatched.push({ issue, text }); },
    save: (m, s) => { saved = { md: m, state: s }; },
  });
  return { ...result, dispatched, saved, calls, row: parseLedger(saved.md).rows[0] };
};

test('new submitted review: exactly one continuation on the owning issue, scope A wording', async () => {
  const r = await run(world({ [`${API}/pulls/10/reviews?per_page=100`]: { etag: 'r2', body: [review()] } }));
  assert.equal(r.dispatched.length, 1);
  assert.equal(r.dispatched[0].issue, 'CHE-1');
  assert.match(r.dispatched[0].text, /please rename/);
  assert.match(r.dispatched[0].text, /new approval/);
  assert.equal(r.row.watermark, '2026-11-01T10:00:00Z');
});

test('new inline comment and a review in one poll still make one continuation', async () => {
  const r = await run(world({
    [`${API}/pulls/10/reviews?per_page=100`]: { etag: 'r2', body: [review()] },
    [`${API}/pulls/10/comments?per_page=100`]: { etag: 'c2', body: [inline()] },
  }));
  assert.equal(r.dispatched.length, 1);
  assert.match(r.dispatched[0].text, /nit: use const/);
  assert.match(r.dispatched[0].text, /please rename/);
  assert.equal(r.row.watermark, '2026-11-01T11:00:00Z');
});

test('no change: 304 everywhere, no dispatch, ledger untouched', async () => {
  const w = world();
  const first = await run(w);
  assert.equal(first.dispatched.length, 0);
  const second = await run(w, { md: first.saved.md, state: first.saved.state });
  assert.equal(second.dispatched.length, 0);
  assert.equal(second.saved.md, first.saved.md);
  assert.ok(Object.keys(w).length > 0);
});

test('same event seen twice (overlapping watermark) dispatches once', async () => {
  const w = world({ [`${API}/pulls/10/reviews?per_page=100`]: { etag: 'r2', body: [review()] } });
  const first = await run(w);
  const w2 = world({ [`${API}/pulls/10/reviews?per_page=100`]: { etag: 'r3', body: [review()] } });
  const second = await run(w2, { md: first.saved.md, state: first.saved.state });
  assert.equal(second.dispatched.length, 0);
});

test('an edited comment (new updated_at) is a new content version', async () => {
  const w = world({ [`${API}/pulls/10/comments?per_page=100`]: { etag: 'c2', body: [inline()] } });
  const first = await run(w);
  const w2 = world({ [`${API}/pulls/10/comments?per_page=100`]: { etag: 'c3', body: [inline({ body: 'edited', updated_at: '2026-11-01T12:00:00Z' })] } });
  const second = await run(w2, { md: first.saved.md, state: first.saved.state });
  assert.equal(second.dispatched.length, 1);
  assert.match(second.dispatched[0].text, /edited/);
});

test('our own comments and empty approvals do not wake an agent', async () => {
  const r = await run(world({
    [`${API}/issues/10/comments?per_page=100`]: { etag: 'i2', body: [{ id: 3, user: { login: 'congvc-dev' }, body: 'thanks', updated_at: '2026-11-01T09:00:00Z' }] },
    [`${API}/pulls/10/reviews?per_page=100`]: { etag: 'r2', body: [review({ id: 4, state: 'APPROVED', body: '' })] },
  }));
  assert.equal(r.dispatched.length, 0);
});

test('bot comments (vercel, CI) do not wake an agent', async () => {
  const r = await run(world({ [`${API}/issues/10/comments?per_page=100`]: { etag: 'i2', body: [{ id: 6, user: { login: 'vercel[bot]' }, body: 'deploy needs auth', updated_at: '2026-11-01T09:00:00Z' }] } }));
  assert.equal(r.dispatched.length, 0);
});

test('follows pagination', async () => {
  const page2 = `${API}/pulls/10/comments?per_page=100&page=2`;
  const r = await run(world({
    [`${API}/pulls/10/comments?per_page=100`]: { etag: 'c2', body: [], next: page2 },
    [page2]: { etag: 'c2b', body: [inline()] },
  }));
  assert.equal(r.dispatched.length, 1);
});

test('merged upstream: ledger row records PR and merge SHA, one continuation for the convergence PR', async () => {
  const r = await run(world({ [`${API}/pulls/10`]: { etag: 'p2', body: pr({ state: 'closed', merged: true, merged_at: '2026-11-01T20:00:00Z', merge_commit_sha: 'deadbeef' }) } }));
  assert.equal(r.row.state, 'merged upstream');
  assert.match(r.row.notes, /deadbeef/);
  assert.equal(r.dispatched.length, 1);
  assert.match(r.dispatched[0].text, /convergence PR/);
  assert.match(r.dispatched[0].text, /migration gate/);
});

test('closed unmerged: close reason recorded from the last reviewer comment', async () => {
  const r = await run(world({
    [`${API}/pulls/10`]: { etag: 'p2', body: pr({ state: 'closed' }) },
    [`${API}/issues/10/comments?per_page=100`]: { etag: 'i2', body: [{ id: 5, user: { login: 'bohan' }, body: 'out of scope, closing', updated_at: '2026-11-01T18:00:00Z' }] },
  }));
  assert.equal(r.row.state, 'closed unmerged');
  assert.match(r.row.notes, /out of scope, closing/);
});

test('merged upstream row with a landed convergence PR becomes integrated into fork main', async () => {
  const md = ledgerOf('| multica-ai/multica#10 | CHE-1 | aaa | merged upstream | 2026-11-01T20:00:00Z | cheese-work/multica#99 | merged |');
  const r = await run({ 'https://api.github.com/repos/cheese-work/multica/pulls/99': { etag: 'f1', body: { state: 'closed', merged: true } } }, { md });
  assert.equal(r.row.state, 'integrated into fork main');
  assert.equal(r.dispatched.length, 0);
});

test('terminal rows are not polled', async () => {
  const md = ledgerOf('| multica-ai/multica#10 | CHE-1 | aaa | closed unmerged | 2026-11-01T20:00:00Z | - | x |');
  const r = await run({}, { md });
  assert.equal(r.calls.length, 0);
});

test('7 days without a response: one follow-up proposal, never repeated', async () => {
  const later = new Date('2026-11-09T00:00:01Z');
  const w = world();
  const first = await run(w, { now: later });
  assert.equal(first.dispatched.length, 1);
  assert.match(first.dispatched[0].text, /follow-up/);
  const second = await run(w, { md: first.saved.md, state: first.saved.state, now: new Date('2026-11-12T00:00:00Z') });
  assert.equal(second.dispatched.length, 0);
});

test('a failing request marks monitoring stale and keeps going; recovery clears it', async () => {
  const w = world({ [`${API}/pulls/10/reviews?per_page=100`]: { status: 500, body: {} } });
  const bad = await run(w);
  assert.equal(bad.ok, false);
  assert.match(bad.saved.state.stale.error, /500/);
  const good = await run(world(), { md: bad.saved.md, state: bad.saved.state });
  assert.equal(good.ok, true);
  assert.equal(good.saved.state.stale, undefined);
});

test('rate limit: backoff until reset, later polls make no requests', async () => {
  const w = world({ [`${API}/pulls/10`]: { status: 403, remaining: '0', reset: 1793577600, body: {} } }); // 2026-11-02T00:00:00Z... + test uses NOW below
  const limited = await run(w, { now: new Date('2026-11-01T23:00:00Z') });
  assert.equal(limited.ok, false);
  assert.equal(limited.saved.state.backoffUntil, '2026-11-02T00:00:00.000Z');
  const skipped = await run(world(), { md: limited.saved.md, state: limited.saved.state, now: new Date('2026-11-01T23:30:00Z') });
  assert.equal(skipped.calls.length, 0);
  assert.equal(skipped.ok, false);
});

test('dispatch failure leaves the event unseen so the next poll retries it', async () => {
  const w = world({ [`${API}/pulls/10/reviews?per_page=100`]: { etag: 'r2', body: [review()] } });
  let saved;
  const failing = await watch({ md: ledgerOf(OPEN), state: {}, now: NOW, self: [], get: fakeGet(w),
    dispatch: async () => { throw new Error('multica down'); }, save: (m, s) => { saved = { md: m, state: s }; } });
  assert.equal(failing.ok, false);
  const retry = await run(w, saved ?? { md: ledgerOf(OPEN), state: {} });
  assert.equal(retry.dispatched.length, 1);
});

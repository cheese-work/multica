// Upstream PR watcher (CHE-3958). One poll over the PRs in docs/fork/UPSTREAM-PRS.md:
// public GitHub API, conditional requests, no agent run unless something actionable
// happened. Publication scope A (CHE-3676): the continuation only prepares work for
// the C00 publisher; it never publishes. Poller state (ETags, seen event keys,
// backoff, stale flag) lives in a state file; the ledger row keeps the watermark.
//
// Usage: node scripts/upstream-pr-watch.mjs [--ledger docs/fork/UPSTREAM-PRS.md] [--state FILE]
// Env: GITHUB_TOKEN (optional, raises the rate limit), UPSTREAM_WATCH_SELF (comma list, default congvc-dev)
import { execFile } from 'node:child_process';
import { mkdtempSync, readFileSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { promisify } from 'node:util';

const API = 'https://api.github.com/repos';
const OVERLAP_MS = 10 * 60_000;
const FOLLOWUP_MS = 7 * 24 * 3_600_000;
const SEEN_MAX = 500;
const COLS = ['pr', 'issue', 'head', 'state', 'watermark', 'convergence', 'notes'];
const REF = /^([\w.-]+\/[\w.-]+)#(\d+)$/;

export function parseLedger(md) {
  const rows = [];
  md.split('\n').forEach((line, index) => {
    const cells = line.startsWith('|') ? line.slice(1, line.lastIndexOf('|')).split('|').map((c) => c.trim()) : [];
    if (cells.length === COLS.length && REF.test(cells[0])) {
      rows.push({ ...Object.fromEntries(COLS.map((c, i) => [c, cells[i]])), index });
    }
  });
  return { rows };
}

const cell = (v) => String(v).replace(/[|\r\n]+/g, ' ').trim() || '-';
const withRows = (md, rows) => {
  const lines = md.split('\n');
  for (const r of rows) lines[r.index] = `| ${COLS.map((c) => cell(r[c])).join(' | ')} |`;
  return lines.join('\n');
};

class RateLimited extends Error {
  constructor(until) { super(`rate limited until ${until.toISOString()}`); this.until = until; }
}

function check(r, url, now) {
  if (r.status === 200 || r.status === 304) return;
  if ((r.status === 403 || r.status === 429) && (r.remaining === '0' || r.retryAfter)) {
    throw new RateLimited(r.retryAfter ? new Date(now.getTime() + Number(r.retryAfter) * 1000) : new Date(Number(r.reset) * 1000));
  }
  throw new Error(`HTTP ${r.status} ${url}`);
}

// One list, all pages. The ETag applies to page 1 only; a 304 there means nothing changed.
async function list(get, url, etags, now) {
  const first = await get(url, { etag: etags[url] });
  check(first, url, now);
  if (first.status === 304) return { items: null, etags: {} };
  const items = [...first.body];
  for (let next = first.next; next;) {
    const page = await get(next);
    check(page, next, now);
    items.push(...page.body);
    next = page.next;
  }
  return { items, etags: { [url]: first.etag } };
}

const login = (x) => (x.user?.login ?? '').toLowerCase();
const events = {
  comment: (c) => ({ key: `ic:${c.id}:${c.updated_at}`, at: c.updated_at, who: login(c), kind: 'comment', body: c.body, actionable: true }),
  inline: (c) => ({ key: `rc:${c.id}:${c.updated_at}`, at: c.updated_at, who: login(c), kind: `inline comment on ${c.path}`, body: c.body, actionable: true }),
  review: (r) => ({ key: `rv:${r.id}:${r.submitted_at}`, at: r.submitted_at, who: login(r), kind: `review (${r.state})`, body: r.body ?? '',
    actionable: !['PENDING', 'DISMISSED'].includes(r.state) && (Boolean(r.body) || r.state === 'CHANGES_REQUESTED'), pending: r.state === 'PENDING' }),
};

const SCOPE_A = 'Scope A: prepare only the same-scope fix or factual reply for the C00 publisher. Do not push, post or contact upstream yourself. A new PR, a bigger change, anything security-related, a force-push or closing the PR needs a new approval from Cheese; say so in your reply.';
const quote = (e) => `- ${e.at} ${e.who} ${e.kind}: ${e.body.slice(0, 600) || '(no text)'}`;
const instructions = {
  review: (row, evs) => `Upstream PR ${row.pr} has new review activity:\n${evs.map(quote).join('\n')}\n\n${SCOPE_A}`,
  merged: (row, evs, pr) => `Upstream PR ${row.pr} merged (merge SHA ${pr.merge_commit_sha}). Open the fork convergence PR: take upstream's version, validate it with the migration gate, then remove the fork equivalent. Put the convergence PR in the ledger row (docs/fork/UPSTREAM-PRS.md, Convergence PR column) and commit the ledger update (State is now "merged upstream"); the watcher marks "integrated into fork main" after it lands.${evs.length ? `\nReview activity seen with the merge:\n${evs.map(quote).join('\n')}` : ''}`,
  closed: (row, evs, pr, reason) => `Upstream PR ${row.pr} was closed without a merge. Recorded reason: ${reason}. Commit the ledger update (docs/fork/UPSTREAM-PRS.md, State "closed unmerged"). Reopening or a new PR needs a new approval from Cheese.`,
  followup: (row) => `Upstream PR ${row.pr} has had no substantive response for 7 days. Draft one concise follow-up comment proposal for the C00 publisher (no repeated pings). ${SCOPE_A}`,
};

// Poll one open row. Returns the ledger patch, the dispatch (if any) and the state to
// commit once the dispatch worked.
async function pollOpen(row, prev, { get, now, self }) {
  const m = REF.exec(row.pr);
  const [, repo, number] = m;
  const base = `${API}/${repo}`;
  const etags = { ...prev.etags };
  const fresh = {};
  const prRes = await get(`${base}/pulls/${number}`, { etag: etags[`${base}/pulls/${number}`] });
  check(prRes, `${base}/pulls/${number}`, now);
  const pr = prRes.status === 304 ? prev.pr : prRes.body;
  if (prRes.status === 200) fresh[`${base}/pulls/${number}`] = prRes.etag;
  const closed = pr.state === 'closed';

  const all = [];
  for (const [name, path] of [['comment', `issues/${number}/comments`], ['inline', `pulls/${number}/comments`], ['review', `pulls/${number}/reviews`]]) {
    const url = `${base}/${path}?per_page=100`;
    const res = await list(get, url, closed ? {} : etags, now);
    Object.assign(fresh, res.etags);
    all.push(...(res.items ?? []).map(events[name]));
  }
  const watermark = row.watermark === '-' ? pr.created_at : row.watermark;
  const cutoff = new Date(Date.parse(watermark) - OVERLAP_MS).toISOString();
  const seen = new Set(prev.seen);
  const incoming = all.filter((e) => e.at >= cutoff && !seen.has(e.key));
  const theirs = incoming.filter((e) => !self.includes(e.who) && !e.who.endsWith('[bot]'));
  const actionable = theirs.filter((e) => e.actionable);
  const responded = theirs.filter((e) => !e.pending);
  const lastResponseAt = [prev.lastResponseAt ?? pr.created_at, ...responded.map((e) => e.at)].reduce((a, b) => (a > b ? a : b));
  const patch = { head: pr.head.sha, watermark: incoming.reduce((a, e) => (e.at > a ? e.at : a), watermark) };
  const next = { etags: { ...etags, ...fresh }, seen: [...prev.seen, ...incoming.map((e) => e.key)].slice(-SEEN_MAX), lastResponseAt, pr, followedUp: prev.followedUp };

  let text;
  if (pr.merged) {
    patch.state = 'merged upstream';
    patch.notes = `merged upstream: ${pr.html_url} merge ${pr.merge_commit_sha}`;
    text = instructions.merged(row, actionable, pr);
  } else if (closed) {
    const last = all.filter((e) => !self.includes(e.who) && !e.who.endsWith('[bot]') && e.body).sort((a, b) => (a.at < b.at ? -1 : 1)).at(-1);
    const reason = last ? last.body.replace(/\s+/g, ' ').slice(0, 200) : 'no comment recorded';
    patch.state = 'closed unmerged';
    patch.notes = `closed: ${reason}`;
    text = instructions.closed(row, actionable, pr, reason);
  } else if (actionable.length) {
    text = instructions.review(row, actionable);
  } else if (!prev.followedUp && now.getTime() - Date.parse(lastResponseAt) >= FOLLOWUP_MS) {
    next.followedUp = now.toISOString();
    text = instructions.followup(row);
  }
  return { patch, next, text };
}

async function pollIntegration(row, { get, now }) {
  const [, repo, number] = REF.exec(row.convergence) ?? [];
  if (!repo) return {};
  const url = `${API}/${repo}/pulls/${number}`;
  const res = await get(url);
  check(res, url, now);
  return res.body.merged ? { patch: { state: 'integrated into fork main' } } : {};
}

export async function watch({ md, state, now, self, get, dispatch, save }) {
  state = { ...state, prs: { ...state.prs } };
  if (state.backoffUntil && Date.parse(state.backoffUntil) > now.getTime()) return { ok: false, skipped: true, md, state };
  delete state.backoffUntil;
  const norm = self.map((s) => s.toLowerCase());
  let errors = [];
  for (const row of parseLedger(md).rows) {
    const live = row.state === 'open' || (row.state === 'merged upstream' && REF.test(row.convergence));
    if (!live) continue;
    try {
      const prev = { etags: {}, seen: [], ...state.prs[row.pr] };
      const result = row.state === 'open'
        ? await pollOpen(row, prev, { get, now, self: norm })
        : await pollIntegration(row, { get, now });
      if (result.text) await dispatch(row.issue, result.text);
      if (result.next) state.prs[row.pr] = result.next;
      if (result.patch) md = withRows(md, [{ ...row, ...result.patch }]);
      save?.(md, state);
    } catch (error) {
      errors.push(`${row.pr}: ${error.message}`);
      if (error instanceof RateLimited) { state.backoffUntil = error.until.toISOString(); break; }
    }
  }
  if (errors.length) state.stale = { since: state.stale?.since ?? now.toISOString(), error: errors.join('; ') };
  else delete state.stale;
  save?.(md, state);
  return { ok: errors.length === 0, md, state };
}

// ---- CLI -------------------------------------------------------------------
const run = promisify(execFile);

const realGet = async (url, { etag } = {}) => {
  const headers = { accept: 'application/vnd.github+json', 'user-agent': 'cheese-work-upstream-pr-watch' };
  if (process.env.GITHUB_TOKEN) headers.authorization = `Bearer ${process.env.GITHUB_TOKEN}`;
  if (etag) headers['if-none-match'] = etag;
  const res = await fetch(url, { headers });
  return {
    status: res.status, etag: res.headers.get('etag'), remaining: res.headers.get('x-ratelimit-remaining'),
    reset: res.headers.get('x-ratelimit-reset'), retryAfter: res.headers.get('retry-after'),
    next: /<([^>]+)>;\s*rel="next"/.exec(res.headers.get('link') ?? '')?.[1],
    body: res.status === 200 ? await res.json() : null,
  };
};

const realDispatch = async (issue, text) => {
  const dir = mkdtempSync(join(tmpdir(), 'upstream-watch-'));
  try {
    const file = join(dir, 'instruction.md');
    writeFileSync(file, text);
    await run('multica', ['issue', 'wakeup', 'create', issue, '--after', '1m', '--mode', 'once', '--instruction-file', file]);
  } finally { rmSync(dir, { recursive: true, force: true }); }
};

async function main() {
  const arg = (name, fallback) => { const i = process.argv.indexOf(name); return i > 0 ? process.argv[i + 1] : fallback; };
  const ledger = arg('--ledger', 'docs/fork/UPSTREAM-PRS.md');
  const stateFile = arg('--state', join(process.env.XDG_STATE_HOME ?? join(homedir(), '.local/state'), 'upstream-pr-watch.json'));
  let state = {};
  try { state = JSON.parse(readFileSync(stateFile, 'utf8')); } catch (e) { if (e.code !== 'ENOENT') throw e; }
  const persist = (md, s) => {
    writeFileSync(ledger, md);
    mkdirSync(dirname(stateFile), { recursive: true });
    writeFileSync(stateFile, JSON.stringify(s, null, 1));
  };
  const result = await watch({
    md: readFileSync(ledger, 'utf8'), state, now: new Date(),
    self: (process.env.UPSTREAM_WATCH_SELF ?? 'congvc-dev').split(','), get: realGet, dispatch: realDispatch, save: persist,
  });
  if (!result.ok) {
    console.error(`upstream-pr-watch: monitoring stale: ${result.state.stale?.error ?? `backing off until ${result.state.backoffUntil}`}`);
    process.exitCode = 1;
  }
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) await main();

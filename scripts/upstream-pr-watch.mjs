// Upstream PR watcher (CHE-3958). One poll over the PRs in docs/fork/UPSTREAM-PRS.md:
// public GitHub API, conditional requests, no agent run unless something actionable
// happened. Publication scope A (CHE-3676): the continuation only prepares work for
// the C00 publisher; it never publishes. The ledger is read-only here: everything the
// poller observes (row overlay, ETags, seen event keys, pending deliveries, backoff,
// stale flag) lives in ONE state file, written atomically. The owning agent commits
// ledger transitions; the continuation text carries the exact row.
//
// Usage: node scripts/upstream-pr-watch.mjs [--ledger docs/fork/UPSTREAM-PRS.md] [--state FILE]
// Env: GITHUB_TOKEN (optional, raises the rate limit), UPSTREAM_WATCH_SELF (comma list, default congvc-dev)
import { execFile } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync, mkdirSync, renameSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { promisify } from 'node:util';

const API = 'https://api.github.com/repos';
const OVERLAP_MS = 10 * 60_000;
const FOLLOWUP_MS = 7 * 24 * 3_600_000;
const SEEN_MAX = 500;
const COLS = ['pr', 'issue', 'head', 'state', 'watermark', 'convergence', 'notes'];
const REF = /^([\w.-]+\/[\w.-]+)#(\d+)$/;
const PAGE = 100;
const RANK = { open: 0, 'merged upstream': 1, 'closed unmerged': 2, 'integrated into fork main': 2 };

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
const rowText = (r) => `| ${COLS.map((c) => cell(r[c])).join(' | ')} |`;

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

// One list, page by page. Every page keeps its own ETag, so an edit on any page shows up
// even when page 1 answers 304. A full page means "there may be more": request the next one.
// A 304 page contributes no items (already seen); a short or empty page ends the walk.
async function list(get, url, pages, now) {
  const items = [];
  const fresh = {};
  for (let n = 1; ; n++) {
    const u = `${url}&page=${n}`;
    const saved = pages[u];
    const res = await get(u, { etag: saved?.etag });
    check(res, u, now);
    const full = res.status === 304 ? Boolean(saved?.full) : res.body.length >= PAGE;
    if (res.status === 200) { items.push(...res.body); fresh[u] = { etag: res.etag, full }; }
    if (!full) return { items, pages: fresh };
  }
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
const COMMIT = (row) => `Commit this ledger row in docs/fork/UPSTREAM-PRS.md (the watcher never edits the ledger):\n${rowText(row)}`;
const instructions = {
  review: (row, evs) => `Upstream PR ${row.pr} has new review activity:\n${evs.map(quote).join('\n')}\n\n${SCOPE_A}`,
  merged: (row, evs, pr) => `Upstream PR ${row.pr} merged (merge SHA ${pr.merge_commit_sha}). Open the fork convergence PR: take upstream's version, validate it with the migration gate, then remove the fork equivalent. ${COMMIT(row)}\nThen set the Convergence PR column to the new fork PR (owner/repo#number); the watcher marks "integrated into fork main" after it lands.${evs.length ? `\nReview activity seen with the merge:\n${evs.map(quote).join('\n')}` : ''}`,
  closed: (row, reason) => `Upstream PR ${row.pr} was closed without a merge. Recorded reason: ${reason}. ${COMMIT(row)}\nReopening or a new PR needs a new approval from Cheese.`,
  integrated: (row) => `The convergence PR for upstream PR ${row.pr} landed in fork main. ${COMMIT(row)}`,
  followup: (row) => `Upstream PR ${row.pr} has had no substantive response for 7 days. Draft one concise follow-up comment proposal for the C00 publisher (no repeated pings). ${SCOPE_A}`,
};

// The ledger row with the poller's observations laid over it. An owner commit that is
// further along (higher rank) than the overlay wins, so the ledger stays the record.
function effective(row, overlay) {
  if (!overlay || (RANK[row.state] ?? 0) > (RANK[overlay.state] ?? 0)) return row;
  return { ...row, ...overlay };
}

// Poll one open row. Returns the new overlay row, the new per-PR poll state and the
// continuation text (if any). Nothing is committed here.
async function pollOpen(row, prev, { get, now, self }) {
  const [, repo, number] = REF.exec(row.pr);
  const base = `${API}/${repo}`;
  const prUrl = `${base}/pulls/${number}`;
  const prRes = await get(prUrl, { etag: prev.prEtag });
  check(prRes, prUrl, now);
  const pr = prRes.status === 304 ? prev.pr : prRes.body;
  const closed = pr.state === 'closed';

  const all = [];
  const pages = {};
  for (const [name, path] of [['comment', `issues/${number}/comments`], ['inline', `pulls/${number}/comments`], ['review', `pulls/${number}/reviews`]]) {
    const res = await list(get, `${base}/${path}?per_page=${PAGE}`, closed ? {} : prev.pages, now);
    Object.assign(pages, res.pages);
    all.push(...res.items.map(events[name]));
  }
  const watermark = row.watermark === '-' ? pr.created_at : row.watermark;
  const cutoff = new Date(Date.parse(watermark) - OVERLAP_MS).toISOString();
  const seen = new Set(prev.seen);
  const incoming = all.filter((e) => e.at >= cutoff && !seen.has(e.key));
  const human = (e) => !self.includes(e.who) && !e.who.endsWith('[bot]');
  const theirs = incoming.filter(human);
  const actionable = theirs.filter((e) => e.actionable);
  const lastResponseAt = [prev.lastResponseAt ?? pr.created_at, ...theirs.filter((e) => !e.pending).map((e) => e.at)].reduce((a, b) => (a > b ? a : b));
  // Pages that answered 304 keep their saved ETag; fresh ones replace it.
  const next = {
    prEtag: prRes.status === 200 ? prRes.etag : prev.prEtag,
    pages: closed ? {} : { ...prev.pages, ...pages },
    seen: [...prev.seen, ...incoming.map((e) => e.key)].slice(-SEEN_MAX),
    lastResponseAt, pr, followedUp: prev.followedUp,
  };
  const overlay = { head: pr.head.sha, state: row.state, watermark: incoming.reduce((a, e) => (e.at > a ? e.at : a), watermark), notes: row.notes };

  let text;
  if (pr.merged) {
    Object.assign(overlay, { state: 'merged upstream', notes: `merged upstream: ${pr.html_url} merge ${pr.merge_commit_sha}` });
    text = instructions.merged({ ...row, ...overlay }, actionable, pr);
  } else if (closed) {
    const last = all.filter((e) => human(e) && e.body).sort((a, b) => (a.at < b.at ? -1 : 1)).at(-1);
    const reason = last ? last.body.replace(/\s+/g, ' ').slice(0, 200) : 'no comment recorded';
    Object.assign(overlay, { state: 'closed unmerged', notes: `closed: ${reason}` });
    text = instructions.closed({ ...row, ...overlay }, reason);
  } else if (actionable.length) {
    text = instructions.review(row, actionable);
  } else if (!prev.followedUp && now.getTime() - Date.parse(lastResponseAt) >= FOLLOWUP_MS) {
    next.followedUp = now.toISOString();
    text = instructions.followup(row);
  }
  return { overlay, next, text };
}

async function pollIntegration(row, { get, now }) {
  const [, repo, number] = REF.exec(row.convergence) ?? [];
  if (!repo) return {};
  const url = `${API}/${repo}/pulls/${number}`;
  const res = await get(url);
  check(res, url, now);
  if (!res.body.merged) return {};
  const overlay = { head: row.head, state: 'integrated into fork main', watermark: row.watermark, notes: row.notes };
  return { overlay, text: instructions.integrated({ ...row, ...overlay }) };
}

const keyOf = (pr, text) => createHash('sha1').update(`${pr}\n${text}`).digest('hex').slice(0, 12);

// State shape: { rows: {pr: overlay}, prs: {pr: poll state}, pending: [outbox], backoffUntil?, stale? }
// `deliver(issue, text, key)` must be idempotent per key; `save(state)` must be atomic.
export async function watch({ md, state, now, self, get, deliver, save }) {
  state = { ...state, rows: { ...state.rows }, prs: { ...state.prs }, pending: [...(state.pending ?? [])] };
  const persist = () => save?.(state);
  if (state.backoffUntil && Date.parse(state.backoffUntil) > now.getTime()) return { ok: false, skipped: true, state };
  delete state.backoffUntil;
  const norm = self.map((s) => s.toLowerCase());
  const errors = [];
  const commit = ({ pr, overlay, next }) => {
    if (overlay) state.rows[pr] = overlay;
    if (next) state.prs[pr] = next;
  };

  // A delivery that failed (or was cut short) earlier goes first, exactly as saved.
  while (state.pending.length) {
    const p = state.pending[0];
    try {
      await deliver(p.issue, p.text, p.key);
    } catch (error) {
      state.stale = { since: state.stale?.since ?? now.toISOString(), error: `${p.pr}: delivery failed: ${error.message}` };
      persist();
      return { ok: false, state };
    }
    commit(p);
    state.pending.shift();
    persist();
  }

  for (const ledgerRow of parseLedger(md).rows) {
    const row = effective(ledgerRow, state.rows[ledgerRow.pr]);
    const live = row.state === 'open' || (row.state === 'merged upstream' && REF.test(row.convergence));
    if (!live) continue;
    try {
      const prev = { pages: {}, seen: [], ...state.prs[row.pr] };
      const result = row.state === 'open' ? await pollOpen(row, prev, { get, now, self: norm }) : await pollIntegration(row, { get, now });
      const change = { pr: row.pr, overlay: result.overlay, next: result.next };
      if (!result.text) { commit(change); persist(); continue; }
      // Save the delivery before attempting it; the commit happens only after it worked.
      state.pending.push({ ...change, issue: row.issue, text: result.text, key: keyOf(row.pr, result.text) });
      persist();
      await deliver(row.issue, result.text, state.pending.at(-1).key);
      commit(state.pending.pop());
      persist();
    } catch (error) {
      errors.push(`${row.pr}: ${error.message}`);
      if (error instanceof RateLimited) { state.backoffUntil = error.until.toISOString(); break; }
    }
  }
  if (errors.length) state.stale = { since: state.stale?.since ?? now.toISOString(), error: errors.join('; ') };
  else delete state.stale;
  persist();
  return { ok: errors.length === 0, state };
}

// ---- CLI -------------------------------------------------------------------
const execMultica = async (args, input) => {
  const argv = input === undefined ? args : [...args, '--instruction', input];
  return (await promisify(execFile)('multica', argv, { maxBuffer: 16 << 20 })).stdout;
};

// One wakeup per key: skip when the issue already carries the marker. The wakeup targets
// the issue's assigned agent explicitly (default would be the poller's own identity).
export const makeDeliver = (exec = execMultica) => async (issue, text, key) => {
  const marker = `[upstream-watch:${key}]`;
  const existing = JSON.parse(await exec(['issue', 'wakeup', 'list', issue, '--output', 'json']));
  if (existing.some((w) => (w.instruction ?? '').includes(marker))) return;
  const { assignee_type: type, assignee_id: agent } = JSON.parse(await exec(['issue', 'get', issue, '--output', 'json']));
  if (type !== 'agent' || !agent) throw new Error(`${issue} is not assigned to an agent`);
  await exec(['issue', 'wakeup', 'create', issue, '--kind', 'at', '--after', '1m', '--mode', 'once', '--agent-id', agent, '--output', 'json'], `${text}\n\n${marker}`);
};

const realGet = async (url, { etag } = {}) => {
  const headers = { accept: 'application/vnd.github+json', 'user-agent': 'cheese-work-upstream-pr-watch' };
  if (process.env.GITHUB_TOKEN) headers.authorization = `Bearer ${process.env.GITHUB_TOKEN}`;
  if (etag) headers['if-none-match'] = etag;
  const res = await fetch(url, { headers });
  return {
    status: res.status, etag: res.headers.get('etag'), remaining: res.headers.get('x-ratelimit-remaining'),
    reset: res.headers.get('x-ratelimit-reset'), retryAfter: res.headers.get('retry-after'),
    body: res.status === 200 ? await res.json() : null,
  };
};

async function main() {
  const arg = (name, fallback) => { const i = process.argv.indexOf(name); return i > 0 ? process.argv[i + 1] : fallback; };
  const ledger = arg('--ledger', 'docs/fork/UPSTREAM-PRS.md');
  const stateFile = arg('--state', join(process.env.XDG_STATE_HOME ?? join(homedir(), '.local/state'), 'upstream-pr-watch.json'));
  let state = {};
  try { state = JSON.parse(readFileSync(stateFile, 'utf8')); } catch (e) { if (e.code !== 'ENOENT') throw e; }
  const persist = (s) => {
    mkdirSync(dirname(stateFile), { recursive: true });
    writeFileSync(`${stateFile}.tmp`, JSON.stringify(s, null, 1));
    renameSync(`${stateFile}.tmp`, stateFile);
  };
  const result = await watch({
    md: readFileSync(ledger, 'utf8'), state, now: new Date(),
    self: (process.env.UPSTREAM_WATCH_SELF ?? 'congvc-dev').split(','), get: realGet, deliver: makeDeliver(), save: persist,
  });
  if (!result.ok) {
    console.error(`upstream-pr-watch: monitoring stale: ${result.state.stale?.error ?? `backing off until ${result.state.backoffUntil}`}`);
    process.exitCode = 1;
  }
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) await main();

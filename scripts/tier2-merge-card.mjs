// One batched merge card per window (CHE-3612, CHE-3598 plan C6). Lists every open Tier 2 PR into the default branch
// that is ready, with the classifier's green required checks and a cross-lab PASS, all on the PR's exact head. Cheese
// still merges: the card only lists the PRs. Tier 0/1 go through the agent merge lane, not this card.
//
// Run from a clean checkout of the default branch (the classifier must equal its base copy), as a wakeup run of the asking agent
// on ONE standing issue (CARD_ISSUE). The server lets an agent withdraw a card only from a running task on the card's own issue
// (server/internal/handler/decision_withdraw.go), so all cards hang on that issue and every run below is a wakeup run on it.
//   node scripts/tier2-merge-card.mjs         send the card. Two cron wakeups on the standing issue (Asia/Ho_Chi_Minh): `30 7,12 * * *` and `0 20 * * *`.
//                                             It first withdraws the earlier merge card if still open (a sent card cannot drop a moved head), posts a
//                                             fresh one, then arms the sweep. CARD_DRY_RUN=1 prints the ask instead.
//   node scripts/tier2-merge-card.mjs sweep   a 15-min wakeup armed by the send run (expires after 12 h): withdraw the open card if a listed PR
//                                             head moved; disable the wakeup once no merge card is open.
// No agent merges from this script: Cheese opens each PR link and merges it on GitHub (Tier 2 stays human, CHE-3612).
// Env: CARD_ISSUE (UUID of the standing issue), CARD_FOR (Cheese's member id), CARD_REPO (default cheese-work/multica).
// The cross-lab PASS is the `security-gate` status: it is `success` only for a signed verdict on the exact head whose
// reviewer lab differs from the author's (RGS-1, scripts/security-gate/security_gate.py); any push resets it.
import { execFile } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
import { pathToFileURL } from 'node:url';

const run = promisify(execFile);
const WINDOWS = ['07:30', '12:30', '20:00']; // ICT, UTC+7, no DST
const MAX_BUFFER = 64 * 1024 * 1024;

const minutesOf = (hhmm) => Number(hhmm.slice(0, 2)) * 60 + Number(hhmm.slice(3));

// Latest window at or before `now`; before the first window of the day, last evening's.
export function currentWindow(now = new Date()) {
  const ict = (now.getUTCHours() * 60 + now.getUTCMinutes() + 7 * 60) % (24 * 60);
  return [...WINDOWS].reverse().find((w) => minutesOf(w) <= ict) ?? WINDOWS[2];
}

// Every fact must name the PR's current head: a moved head fails one of these and drops the PR off the card.
export function eligible(prs) {
  return prs.filter((p) => (p.state ?? 'OPEN') === 'OPEN'
    && !p.isDraft
    && p.receipt?.tier === 2
    && p.receipt.error == null
    && p.receipt.classifier?.trusted === true
    && p.receipt.head === p.headSha
    && p.receipt.ci?.head_sha === p.headSha
    && p.receipt.ci.passed === true
    && p.securityGate?.sha === p.headSha
    && p.securityGate.state === 'success');
}

export function renderAsk(prs, window, sourceIssueId, sourceCommentId) {
  const n = prs.length;
  const noun = `${n} Tier 2 PR${n === 1 ? '' : 's'}`;
  // The link names the head: after a push the commit view no longer matches the head listed here.
  const lines = prs.map((p) => `- [#${p.number} @ ${p.headSha.slice(0, 7)}](${p.url}/commits/${p.headSha}) \`${p.headSha}\` ${p.title}`);
  return {
    context_eli10: `${noun} on cheese-work/multica ${n === 1 ? 'is' : 'are'} ready for your ${window} ICT merge window. Each has green required checks and a cross-lab PASS on the exact head below. Open a PR link and merge it on GitHub.\n\n${lines.join('\n')}`,
    agent_did: `Checked every open PR into main on ${window} ICT: ready, Tier 2 receipt, required checks and PASS on the current head.`,
    decision: `Merge the ${noun} listed above?`,
    stakes_if_wrong: 'You merge each PR yourself; every merge can be reverted. A PR whose head moves after this card is sent no longer has these checks and PASS; a sweep withdraws this card within about 15 min, and the next window lists only PRs whose head still holds.',
    options: [
      { label: 'I will merge them on GitHub', what_happens: 'You open each PR link above and merge it on GitHub. No agent merges. A PR whose head moves after this card no longer has these checks and PASS.', pros: ['Each PR gets your own look', 'No agent merges a Tier 2 PR'], cons: ['One click per PR'], cost_or_time: 'About 1 min per PR' },
      { label: 'Skip this window', what_happens: 'Nothing merges; the PRs appear again on the next card while their head holds.', pros: ['Time to read each diff'], cons: ['Longer wait to merge'], cost_or_time: 'Next window, up to about 12 h' },
    ],
    source_issue_id: sourceIssueId,
    source_comment_id: sourceCommentId,
    recommended_option: 'I will merge them on GitHub',
    recommendation: `Merge the ${noun} on GitHub; the checks and PASS are on the exact heads.`,
    default_if_no_reply: 'Nothing merges; the next window lists the PRs again.',
  };
}

const gh = async (...args) => JSON.parse((await run('gh', args, { maxBuffer: MAX_BUFFER })).stdout);

async function collect(repo) {
  const open = await gh('pr', 'list', '--repo', repo, '--state', 'open', '--limit', '200',
    '--json', 'number,url,title,headRefOid,baseRefName,isDraft');
  const defaultBranch = (await gh('repo', 'view', repo, '--json', 'defaultBranchRef')).defaultBranchRef.name;
  const dir = mkdtempSync(join(tmpdir(), 'tier2-card-'));
  try {
    // ponytail: PRs whose base is not the default branch (stacks) are skipped; merge order matters there.
    return await Promise.all(open.filter((x) => !x.isDraft && x.baseRefName === defaultBranch).map(async (p) => {
      const card = { number: p.number, url: p.url, title: p.title, headSha: p.headRefOid, isDraft: false, receipt: null, securityGate: null };
      // Cheap read first: classifying needs a fetch and a ~25 s classifier run.
      const status = await gh('api', `repos/${repo}/commits/${p.headRefOid}/status`);
      const gate = status.statuses.find((s) => s.context === 'security-gate');
      card.securityGate = gate ? { sha: p.headRefOid, state: gate.state } : null;
      if (card.securityGate?.state === 'success') card.receipt = await classify(repo, p, dir);
      return card;
    }));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

let fetching = Promise.resolve(); // git fetch writes FETCH_HEAD: one at a time, classifiers run in parallel

async function classify(repo, p, dir) {
  const base = (await gh('api', `repos/${repo}/pulls/${p.number}`)).base.sha;
  const file = join(dir, `${p.number}.json`);
  fetching = fetching.catch(() => {}).then(() => run('git', ['fetch', '--no-tags', 'origin', base, `+refs/pull/${p.number}/head`]));
  await fetching;
  // An error still writes a Tier 2 receipt that fails the checks above; a missing file is no receipt.
  await run('python3', ['scripts/mechanical-tier/mechanical_tier.py', 'classify', '--repo', '.', '--base', base,
    '--head', p.headRefOid, '--github', repo, '--pr', String(p.number), '--out', file]).catch(() => {});
  try {
    // `classify` records; only `verify` recomputes from git and the API. A receipt that does not verify is no receipt.
    await run('python3', ['scripts/mechanical-tier/mechanical_tier.py', 'verify', '--repo', '.', '--github', repo, '--pr', String(p.number), '--receipt', file]);
    return JSON.parse(readFileSync(file, 'utf8'));
  } catch { return null; }
}

async function main(args) {
  const repo = process.env.CARD_REPO ?? 'cheese-work/multica';
  if (args.length === 1 && args[0] === 'sweep') return sweepCards(repo);
  if (args.length) throw new Error('usage: tier2-merge-card.mjs [sweep]');
  return sendCard(repo);
}

const CARD_TEXT = /ICT merge window/;
export const isOpenCard = (d) => d.status === 'open' && CARD_TEXT.test(d.ask ?? '');

// The server lets an agent withdraw its card only from a running task on the card's own issue (decision_withdraw.go).
// So every card hangs on one standing issue (CARD_ISSUE) and each window and sweep run is a wakeup run on that issue.
export const openCardsArgs = (issueId) => ['decision', 'list', '--issue', issueId, '--output', 'json'];

const SWEEP_EVERY = '15m';
const SWEEP_FOR = '12h'; // longer than the longest gap between windows (20:00 to 07:30)
export const armSweepArgs = (issueId, memberId) => ['issue', 'wakeup', 'create', issueId, '--kind', 'every', '--every', SWEEP_EVERY, '--expires-in', SWEEP_FOR,
  '--instruction', `Run \`CARD_ISSUE=${issueId} CARD_FOR=${memberId} node scripts/tier2-merge-card.mjs sweep\` from a clean checkout of main, then end the run.`, '--output', 'json'];

const mca = async (...args) => JSON.parse((await run('multica', args, { maxBuffer: MAX_BUFFER })).stdout);

const wakeups = {
  list: () => mca('issue', 'wakeup', 'list', process.env.CARD_ISSUE, '--output', 'json'),
  create: () => mca(...armSweepArgs(process.env.CARD_ISSUE, process.env.CARD_FOR)),
  disable: (id) => mca('issue', 'wakeup', 'disable', process.env.CARD_ISSUE, id, '--output', 'json'),
};
const sweepRules = async (io) => (await io.list()).filter((x) => x.kind === 'every' && x.enabled);

async function disarmSweep(io = wakeups) {
  for (const r of await sweepRules(io)) await io.disable(r.id);
}

// Create the new rule first and disable the old ones after: a failed create leaves the old rule watching, so no gap.
export async function replaceSweep(io = wakeups) {
  const old = await sweepRules(io);
  await io.create();
  for (const r of old) await io.disable(r.id);
}

const liveIo = {
  collect,
  heads: async (repo) => new Map((await gh('pr', 'list', '--repo', repo, '--state', 'open', '--limit', '200', '--json', 'number,headRefOid')).map((x) => [x.number, x.headRefOid])),
  openCards: () => mca(...openCardsArgs(process.env.CARD_ISSUE)),
  withdraw: (id) => run('multica', ['decision', 'withdraw', id, '--reason', 'Superseded: a listed PR head moved or a newer Tier 2 merge card run replaced it', '--output', 'json']),
  comment: async () => {
    const dir = mkdtempSync(join(tmpdir(), 'tier2-card-'));
    try {
      const file = join(dir, 'comment.md');
      writeFileSync(file, `Tier 2 merge card run, ${currentWindow()} ICT window.`);
      return (await mca('issue', 'comment', 'add', process.env.CARD_ISSUE, '--content-file', file, '--output', 'json')).id;
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  },
  post: async (ask) => {
    const dir = mkdtempSync(join(tmpdir(), 'tier2-card-'));
    try {
      const askFile = join(dir, 'ask.json');
      writeFileSync(askFile, JSON.stringify(ask));
      await run('multica', ['decision', 'request', '--issue', process.env.CARD_ISSUE, '--for', process.env.CARD_FOR,
        '--class', 'reversible', '--ask-file', askFile, '--output', 'json']);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  },
  // Re-arming replaces the earlier sweep rule so rules never pile up.
  armSweep: () => replaceSweep(),
  disarmSweep: () => disarmSweep(),
};

// A sent card cannot change. Each send run withdraws every still-open earlier merge card, then posts a fresh one.
export async function sendCard(repo, io = liveIo) {
  const dry = Boolean(process.env.CARD_DRY_RUN);
  const ready = eligible(await io.collect(repo));
  // Re-read the heads after collecting: a push during collection leaves the old head in `ready`.
  const heads = await io.heads(repo);
  const fresh = ready.filter((p) => heads.get(p.number) === p.headSha);
  if (dry) return console.log(fresh.length ? JSON.stringify(renderAsk(fresh, currentWindow(), process.env.CARD_ISSUE), null, 2) : 'no Tier 2 PR is ready');
  for (const d of (await io.openCards()).filter(isOpenCard)) await io.withdraw(d.id);
  if (!fresh.length) {
    console.error('no Tier 2 PR is ready; no card sent');
    return io.disarmSweep();
  }
  // Arm before posting: a posted card always has a sweep watching it. If arming fails, no card is posted;
  // if the post fails, the armed sweep finds no open card and disables itself.
  await io.armSweep();
  await io.post(renderAsk(fresh, currentWindow(), process.env.CARD_ISSUE, await io.comment()));
}

// Runs every 15 min while a card is open (armed by sendCard): a push to a listed PR leaves the old head on the open card, so withdraw it.
// A listed PR that is no longer open (merged by Cheese from the card) is fine; only a moved head makes the card stale.
// With no merge card left open the wakeup rule is disabled, so idle days cost no runs.
export async function sweepCards(repo, io = liveIo) {
  let open = (await io.openCards()).filter(isOpenCard);
  if (open.length) {
    const heads = await io.heads(repo);
    const moved = (d) => [...d.ask.matchAll(/\/pull\/(\d+)\/commits\/([0-9a-f]{40})/g)].some(([, n, sha]) => heads.has(Number(n)) && heads.get(Number(n)) !== sha);
    for (const d of open.filter(moved)) await io.withdraw(d.id);
    open = open.filter((d) => !moved(d));
  }
  if (!open.length) await io.disarmSweep();
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) await main(process.argv.slice(2));

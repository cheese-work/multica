// One batched merge card per window (CHE-3612, CHE-3598 plan C6). Lists every open Tier 2 PR into the default branch
// that is ready, with the classifier's green required checks and a cross-lab PASS, all on the PR's exact head. Cheese
// still merges: the card only batches the ask. Tier 0/1 go through the agent merge lane, not this card.
//
// Run from a clean checkout of the default branch (the classifier must equal its base copy).
//   node scripts/tier2-merge-card.mjs            send the card; run at 07:30, 12:30 and 20:00 ICT (cron `30 0,5 * * *`
//                                                and `0 13 * * *` UTC). CARD_DRY_RUN=1 prints the ask instead.
//   node scripts/tier2-merge-card.mjs merge N:SHA...   run ONLY after Cheese answers "Merge all" (or picks PRs) on a
//                                                card. Re-checks every PR and merges it with `--match-head-commit`, so a
//                                                head that moved after the card is skipped, never merged.
// Env: CARD_ISSUE (issue the card hangs on), CARD_FOR (Cheese's member id), CARD_REPO (default cheese-work/multica).
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

export const mergeMethod = (title) => ({ '[squash]': '--squash', '[merge]': '--merge' })[title.trim().match(/\[(squash|merge)\]$/)?.[0]] ?? null;

export function parseListed(args) {
  if (!args.length) throw new Error('merge needs at least one N:SHA pair from the card');
  return args.map((a) => {
    const m = /^(\d+):([0-9a-f]{40})$/.exec(a);
    if (!m) throw new Error(`bad pair ${a}: expected <pr-number>:<40-char head sha>`);
    return { number: Number(m[1]), headSha: m[2] };
  });
}

// `current` = eligible() over a fresh read. A listed PR merges only if it is still eligible at the listed head.
export function planMerge(listed, current) {
  const merge = [], skipped = [];
  for (const l of listed) {
    const now = current.find((p) => p.number === l.number);
    if (now?.headSha === l.headSha) merge.push(now);
    else skipped.push({ number: l.number, reason: now ? `head moved to ${now.headSha}` : 'no longer open, ready and eligible' });
  }
  return { merge, skipped };
}

export function renderAsk(prs, window) {
  const n = prs.length;
  const noun = `${n} Tier 2 PR${n === 1 ? '' : 's'}`;
  const lines = prs.map((p) => `- [#${p.number}](${p.url}) \`${p.headSha}\` ${p.title}`);
  return {
    context_eli10: `${noun} on cheese-work/multica ${n === 1 ? 'is' : 'are'} ready for your ${window} ICT merge window. Each has green required checks and a cross-lab PASS on the exact head below. Open a PR link to merge it with one click.\n\n${lines.join('\n')}`,
    agent_did: `Checked every open PR into main on ${window} ICT: ready, Tier 2 receipt, required checks and PASS on the current head.`,
    decision: `Merge the ${noun} listed above?`,
    stakes_if_wrong: 'Every merge can be reverted. A PR whose head moves after this card is sent no longer has these checks and PASS, and is skipped.',
    options: [
      { label: `Merge all ${n}`, what_happens: `An agent runs \`node scripts/tier2-merge-card.mjs merge ${prs.map((p) => `${p.number}:${p.headSha}`).join(' ')}\`. Each PR is re-checked and merged at its listed head; a PR whose head moved is skipped.`, pros: ['One click'], cons: ['No per-PR look'], cost_or_time: 'About 1 min' },
      { label: 'Merge none now', what_happens: 'Nothing merges; the PRs appear again on the next card while their head holds.', pros: ['Time to read each diff'], cons: ['Longer wait to merge'], cost_or_time: 'Next window, up to about 12 h' },
    ],
    recommendation: `Merge all ${n}; the checks and PASS are on the exact heads.`,
    default_if_no_reply: 'Nothing merges; the next window lists the PRs again.',
  };
}

const gh = async (...args) => JSON.parse((await run('gh', args, { maxBuffer: MAX_BUFFER })).stdout);

async function collect(repo, only) {
  const open = await gh('pr', 'list', '--repo', repo, '--state', 'open', '--limit', '200',
    '--json', 'number,url,title,headRefOid,baseRefName,isDraft');
  const defaultBranch = (await gh('repo', 'view', repo, '--json', 'defaultBranchRef')).defaultBranchRef.name;
  const dir = mkdtempSync(join(tmpdir(), 'tier2-card-'));
  try {
    // ponytail: PRs whose base is not the default branch (stacks) are skipped; merge order matters there.
    return await Promise.all(open.filter((x) => !x.isDraft && x.baseRefName === defaultBranch && (!only || only.includes(x.number))).map(async (p) => {
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

async function main([cmd, ...rest]) {
  const repo = process.env.CARD_REPO ?? 'cheese-work/multica';
  return cmd === 'merge' ? mergeListed(repo, parseListed(rest)) : sendCard(repo);
}

async function sendCard(repo) {
  const ready = eligible(await collect(repo));
  // A push during collection leaves the old head in `ready`; re-read the heads and drop any that moved.
  const heads = new Map((await gh('pr', 'list', '--repo', repo, '--state', 'open', '--limit', '200', '--json', 'number,headRefOid')).map((x) => [x.number, x.headRefOid]));
  const fresh = ready.filter((p) => heads.get(p.number) === p.headSha);
  if (!fresh.length) return console.error('no Tier 2 PR is ready; no card sent');
  const ask = renderAsk(fresh, currentWindow());
  if (process.env.CARD_DRY_RUN) return console.log(JSON.stringify(ask, null, 2));
  const dir = mkdtempSync(join(tmpdir(), 'tier2-card-'));
  try {
    const askFile = join(dir, 'ask.json');
    writeFileSync(askFile, JSON.stringify(ask));
    await run('multica', ['decision', 'request', '--issue', process.env.CARD_ISSUE, '--for', process.env.CARD_FOR,
      '--class', 'reversible', '--ask-file', askFile, '--output', 'json']);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

async function mergeListed(repo, listed) {
  const plan = planMerge(listed, eligible(await collect(repo, listed.map((l) => l.number))));
  for (const s of plan.skipped) console.log(`skipped #${s.number}: ${s.reason}`);
  let failed = plan.skipped.length;
  for (const p of plan.merge) {
    const method = mergeMethod(p.title);
    if (!method) { console.log(`skipped #${p.number}: title has no [squash] or [merge] tag`); failed += 1; continue; }
    try {
      // GitHub refuses the merge if the head is no longer exactly this SHA: the last guard against a late push.
      await run('gh', ['pr', 'merge', String(p.number), '--repo', repo, method, '--match-head-commit', p.headSha]);
      console.log(`merged #${p.number} at ${p.headSha}`);
    } catch (e) {
      console.log(`failed #${p.number}: ${e.stderr?.trim() || e.message}`);
      failed += 1;
    }
  }
  process.exitCode = failed ? 1 : 0;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) await main(process.argv.slice(2));

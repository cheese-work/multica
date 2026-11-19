// Quick A/B measure (CHE-7658 step 9, CHE-7674). Read-only: scores the Quick-first trial
// (arm Q, Quick first) against today's mapping (arm D) per the plan's preregistration.
//
//   DATABASE_URL=... node scripts/quick-ab-measure.mjs --start 2026-11-21T00:00:00Z \
//       [--as-of <ISO>] [--gateway-usd gateway.json] [--labs labs.json] [--workspace <uuid>] [--json]
//   DATABASE_URL=... node scripts/quick-ab-measure.mjs --issues 7001,7002 [--gateway-usd ...]   # dry run on named issues
//
// Arms: the first `quick_entry` activity of a root issue. A draw row carries `details.arm`
// (Q/quick or D/default); an override row has no `details.arm` and is not a trial entry.
// Cohort: the first 10 eligible root issues per arm created at or after --start (card 4
// `starts_at`), in creation order; later ones are listed, not scored.
// Verdict: the readout is frozen at the first cutoff, no later than day 21 after --start, where both cohorts
// hold 10 and every scored issue is `done` or 7 days old: `scored`. Otherwise the cutoff is day 21 and the
// verdict `inconclusive` (recommends nothing); before that `pending`. Issues, tasks, runs and status after
// the cutoff are not read, so a late or historical --as-of reproduces the same numbers.
// Runs: every agent task row on the issue (any state) created from issue create to `done`
// or day 7. Hours: sum of run durations (started to completed) and create-to-end span.
// USD (CHE-7648): Sub2API gateway USD per ICT day (--gateway-usd, {"YYYY-MM-DD": usd}) split by the
// issue's share of that day's Multica tokens (input+output+cache read+cache write). A day with no
// gateway figure makes the issue's USD null, never a smaller number.
// Same-lab review: a review task (role `reviewer`, else workflow step `agent_review`) whose lab equals
// the lab of an author task. Lab = labs["provider/model"] (MULTICA_VOTE_GATE_LABS format). A review is
// classified only when its lab and every author lab are known; otherwise it counts as unknown and the
// issue's same-lab figure is null (shown `?`), never zero.
// The connection reaches psql through PG* variables (see background-agent-usage.mjs).
import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import { psqlJson } from './background-agent-usage.mjs';

const DAY = 86400e3;
const HOUR = 3600e3;
const ICT_OFFSET = 7 * HOUR;
const ARMS = ['Q', 'D'];

const ms = (t) => new Date(t).getTime();
const ictDay = (t) => new Date(ms(t) + ICT_OFFSET).toISOString().slice(0, 10);
const round = (n) => Math.round(n * 100) / 100;

function normArm(a) {
  const s = String(a ?? '').toLowerCase();
  if (s === 'q' || s === 'quick') return 'Q';
  if (s === 'd' || s === 'default') return 'D';
  return null;
}

function median(values) {
  const v = values.filter((x) => x !== null && x !== undefined).sort((a, b) => a - b);
  if (!v.length) return null;
  const mid = Math.floor(v.length / 2);
  return v.length % 2 ? v[mid] : (v[mid - 1] + v[mid]) / 2;
}

function score(issue, tasks, usageByTask, totals, ctx) {
  const created = ms(issue.created_at);
  const day7 = created + 7 * DAY;
  // Everything is read as of ctx.asOf (the frozen cutoff): later status, tasks and runs do not exist yet.
  const done = issue.done_at && ms(issue.done_at) <= ctx.asOf ? ms(issue.done_at) : null;
  const counted = tasks.filter((t) => {
    const c = ms(t.created_at);
    return c >= created && c < day7 && c <= ctx.asOf && (done === null || c <= done);
  });
  const end = Math.min(done ?? Infinity, day7, ctx.asOf);
  const runHours = counted.reduce((s, t) => (t.started_at && t.completed_at && ms(t.completed_at) <= ctx.asOf
    ? s + (ms(t.completed_at) - ms(t.started_at)) / HOUR : s), 0);

  const lab = (t) => ctx.labs[`${t.provider}/${t.model}`] ?? null;
  const isReview = (t) => t.role === 'reviewer' || (!t.role && t.workflow_step === 'agent_review');
  const reviews = counted.filter(isReview);
  const authors = counted.filter((t) => !isReview(t));
  const authorLabs = new Set(authors.map(lab).filter(Boolean));
  // A review is only classified when the evidence is complete: its own lab and every author lab known.
  // Anything else is unknown; unknown is never reported as zero same-lab.
  const authorsKnown = authors.length > 0 && authors.every(lab);
  let sameLab = 0;
  let unknownLab = 0;
  for (const t of reviews) {
    if (lab(t) && authorLabs.has(lab(t))) sameLab += 1;
    else if (!lab(t) || !authorsKnown) unknownLab += 1;
  }

  const tokensByDay = new Map();
  const lastDay = ictDay(ctx.asOf);
  for (const t of counted) for (const u of (usageByTask.get(t.id) ?? []).filter((x) => x.day <= lastDay)) tokensByDay.set(u.day, (tokensByDay.get(u.day) ?? 0) + Number(u.tokens));
  const missing = [];
  let usd = 0;
  for (const [d, tok] of [...tokensByDay].sort(([a], [b]) => a.localeCompare(b))) {
    const gw = ctx.gatewayUsd?.[d];
    const total = totals.get(d);
    if (gw === undefined || gw === null || !total) missing.push(d);
    else usd += (gw * tok) / total;
  }
  const hasUsd = ctx.gatewayUsd && tokensByDay.size > 0 && !missing.length;

  const intake = issue.intake ?? null;
  const est = Number.parseFloat(intake?.estimate);
  return {
    id: issue.id,
    number: issue.number,
    identifier: issue.identifier ?? `#${issue.number}`,
    arm: issue.arm,
    status: done !== null ? 'done' : issue.status === 'done' ? 'not_done' : issue.status,
    createdAt: issue.created_at,
    runs: counted.length,
    runHours: round(runHours),
    spanHours: round((end - created) / HOUR),
    unfinishedAtDay7: ctx.asOf >= day7 && (done === null || done > day7),
    usd: hasUsd ? round(usd) : null,
    usdMissingDays: ctx.gatewayUsd ? missing : [...tokensByDay.keys()].sort(),
    reviews: reviews.length,
    sameLabReviews: unknownLab ? null : sameLab,
    sameLabMatched: sameLab,
    unknownLabReviews: unknownLab,
    escalated: Boolean(issue.escalated),
    intakeCall: intake?.result ?? 'none',
    estimateHours: Number.isNaN(est) ? null : est,
    mature: done !== null || ctx.asOf >= day7,
  };
}

export function measure(data, opts) {
  const { start, asOf = new Date(), gatewayUsd = null, labs = {}, cohortSize = 10, issuesOnly = false } = opts;
  const ctx = { asOf: ms(asOf), gatewayUsd, labs };
  const tasksByIssue = new Map();
  for (const t of data.tasks) tasksByIssue.set(t.issue_id, [...(tasksByIssue.get(t.issue_id) ?? []), t]);
  const usageByTask = new Map();
  for (const u of data.usage) usageByTask.set(u.task_id, [...(usageByTask.get(u.task_id) ?? []), u]);
  const totals = new Map(data.totals.map((t) => [t.day, Number(t.tokens)]));
  const scoreOf = (i) => score(i, tasksByIssue.get(i.id) ?? [], usageByTask, totals, ctx);
  const byCreation = (a, b) => ms(a.created_at) - ms(b.created_at) || a.number - b.number;

  if (issuesOnly) {
    const issues = [...data.issues].sort(byCreation).map((i) => scoreOf({ ...i, arm: normArm(i.arm) ?? '-' }));
    ctx.asOf = ms(asOf);
    return { verdict: 'listed', reasons: [], counts: null, daily: [], issues, unscored: [], immature: [], arms: null };
  }

  const deadline = ms(start) + 21 * DAY;
  const seenAt = ms(asOf);
  const eligibleAll = data.issues
    .filter((i) => !i.parent_issue_id && i.entry_source === 'arm' && ms(i.created_at) >= ms(start) && ms(i.created_at) <= seenAt)
    .map((i) => ({ ...i, arm: normArm(i.arm) }))
    .filter((i) => i.arm)
    .sort(byCreation);
  const cohortAt = (t) => {
    const taken = { Q: [], D: [] };
    for (const i of eligibleAll) if (ms(i.created_at) <= t && taken[i.arm].length < cohortSize) taken[i.arm].push(i);
    return taken;
  };
  // Freeze the readout at the first qualifying cutoff, never later than day 21: the time both cohorts are
  // complete and every cohort issue is done or 7 days old. Later issues and later status cannot change it.
  const matureAt = (i) => Math.min(i.done_at && ms(i.done_at) <= seenAt ? ms(i.done_at) : Infinity, ms(i.created_at) + 7 * DAY);
  const early = cohortAt(deadline);
  const qualifying = ARMS.every((a) => early[a].length >= cohortSize)
    ? Math.max(...ARMS.flatMap((a) => early[a].map((i) => Math.max(ms(i.created_at), matureAt(i))))) : Infinity;
  let cutoff = null;
  if (qualifying <= deadline && qualifying <= seenAt) cutoff = qualifying;
  else if (seenAt >= deadline) cutoff = deadline;
  const obs = cutoff ?? seenAt;
  ctx.asOf = obs;

  const taken = cohortAt(obs);
  const counts = { Q: taken.Q.length, D: taken.D.length };
  const inCohort = new Set([...taken.Q, ...taken.D].map((i) => i.id));
  const issues = [...taken.Q, ...taken.D].sort(byCreation).map(scoreOf);
  const unscored = eligibleAll.filter((i) => !inCohort.has(i.id)).map(scoreOf);

  const dailyMap = new Map();
  for (const i of eligibleAll) {
    const d = ictDay(i.created_at);
    dailyMap.set(d, { day: d, Q: 0, D: 0, ...dailyMap.get(d) });
    dailyMap.get(d)[i.arm] += 1;
  }
  const daily = [...dailyMap.values()].sort((a, b) => a.day.localeCompare(b.day));

  const reasons = [];
  for (const arm of ARMS) if (counts[arm] < cohortSize) reasons.push(`cohort_below_${cohortSize}:${arm}`);
  const immature = issues.filter((i) => !i.mature).map(({ id, identifier, arm, status, createdAt }) => ({
    id, identifier, arm, status, ageDays: round((obs - ms(createdAt)) / DAY),
  }));
  if (immature.length) reasons.push('immature_scored_issue');

  let verdict = 'scored';
  if (reasons.length) verdict = seenAt >= deadline ? 'inconclusive' : 'pending';
  const cutoffInfo = { cutoff: new Date(obs).toISOString(), cutoffDay: round((obs - ms(start)) / DAY) };

  let arms = null;
  if (verdict === 'scored') {
    arms = {};
    for (const arm of ARMS) {
      const mine = issues.filter((i) => i.arm === arm);
      arms[arm] = {
        issues: mine.length,
        medianRuns: median(mine.map((i) => i.runs)),
        medianUsd: median(mine.map((i) => i.usd)),
        usdKnown: mine.filter((i) => i.usd !== null).length,
        sameLabReviews: mine.reduce((s, i) => s + i.sameLabMatched, 0),
        sameLabUnknownIssues: mine.filter((i) => i.unknownLabReviews > 0).length,
        sameLabZeroEstablished: mine.every((i) => i.unknownLabReviews === 0) && mine.every((i) => i.sameLabMatched === 0),
        escalations: mine.filter((i) => i.escalated).length,
      };
    }
  }
  return { verdict, reasons, ...cutoffInfo, counts, daily, issues, unscored, immature, arms };
}

const pad = (v, n) => String(v ?? '-').padEnd(n);

export function renderReport(r) {
  const out = [`Quick A/B measure: ${r.verdict}`];
  if (r.counts) out.push(`cohort counts at the cutoff: Q ${r.counts.Q}  D ${r.counts.D}`);
  if (r.cutoff) out.push(`cutoff: day ${r.cutoffDay} after start (${r.cutoff})`);
  if (r.reasons.length) out.push(`reasons: ${r.reasons.join(', ')}`);
  if (r.verdict === 'inconclusive') out.push('no recommendation');
  if (r.immature.length) {
    out.push('immature scored issues:');
    for (const i of r.immature) out.push(`  ${i.identifier} arm ${i.arm} ${i.status} age ${i.ageDays} d`);
  }
  if (r.daily.length) {
    out.push('', `${pad('day (ICT)', 12)} ${pad('Q', 4)} ${pad('D', 4)}`);
    for (const d of r.daily) out.push(`${pad(d.day, 12)} ${pad(d.Q, 4)} ${pad(d.D, 4)}`);
  }
  const row = (i) => [pad(i.identifier, 10), pad(i.arm, 4), pad(i.status, 12), pad(i.runs, 5), pad(i.runHours, 7), pad(i.spanHours, 7),
    pad(i.usd === null ? 'n/a' : i.usd, 8), pad(i.reviews, 4), pad(i.sameLabReviews ?? '?', 5), pad(i.unknownLabReviews, 4), pad(i.escalated ? 'yes' : 'no', 5),
    pad(i.intakeCall, 10), pad(i.estimateHours, 6), i.unfinishedAtDay7 ? 'unfinished@d7' : ''].join(' ').trimEnd();
  const head = [pad('issue', 10), pad('arm', 4), pad('status', 12), pad('runs', 5), pad('run h', 7), pad('span h', 7), pad('USD', 8),
    pad('rev', 4), pad('same', 5), pad('unk', 4), pad('esc', 5), pad('intake', 10), pad('est h', 6)].join(' ');
  if (r.issues.length) out.push('', head, ...r.issues.map(row));
  if (r.unscored.length) out.push('', 'listed, not scored (past the first 10 per arm):', ...r.unscored.map(row));
  if (r.arms) {
    out.push('');
    for (const a of ARMS) {
      const s = r.arms[a];
      out.push(`arm ${a}: median runs ${s.medianRuns}, median USD ${s.medianUsd ?? 'n/a'} (${s.usdKnown}/${s.issues} priced), ${s.sameLabUnknownIssues ? `same-lab reviews unknown on ${s.sameLabUnknownIssues} issue${s.sameLabUnknownIssues === 1 ? '' : 's'} (${s.sameLabReviews} matched)` : `same-lab reviews ${s.sameLabReviews}`}, escalations ${s.escalations}/${s.issues}`);
    }
  }
  return `${out.join('\n')}\n`;
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function buildQuery({ start, workspace, issues }) {
  if (workspace !== undefined && !UUID.test(workspace)) throw new Error('--workspace must be a UUID');
  if (issues !== undefined && !(Array.isArray(issues) && issues.length && issues.every((n) => /^\d+$/.test(String(n))))) {
    throw new Error('--issues must be a comma list of issue numbers');
  }
  let startIso = null;
  if (!issues) {
    if (!start || Number.isNaN(ms(start))) throw new Error('--start is required (card 4 starts_at)');
    startIso = new Date(ms(start)).toISOString();
  }
  const ws = workspace ? `AND i.workspace_id = '${workspace}'` : '';
  const filter = issues ? `i.number IN (${issues.map(Number).join(',')}) ${ws}` : `i.created_at >= '${startIso}' ${ws}`;
  return `
WITH picked AS (
  SELECT i.id, i.number, i.status, i.created_at, i.updated_at, i.parent_issue_id, w.issue_prefix,
         e.details AS entry
  FROM issue i
  JOIN workspace w ON w.id = i.workspace_id
  LEFT JOIN LATERAL (
    SELECT a.details FROM activity_log a
    WHERE a.issue_id = i.id AND a.action = 'quick_entry'
    ORDER BY a.created_at, a.id LIMIT 1
  ) e ON true
  WHERE ${filter}${issues ? '' : ' AND e.details IS NOT NULL'}
), issue_rows AS (
  SELECT p.id, p.number, p.status, p.created_at, p.parent_issue_id,
         p.issue_prefix || '-' || p.number AS identifier,
         CASE WHEN p.entry ? 'arm' THEN 'arm' ELSE 'override' END AS entry_source,
         p.entry->>'arm' AS arm,
         COALESCE((SELECT min(a.created_at) FROM activity_log a
                    WHERE a.issue_id = p.id AND a.action = 'status_changed' AND a.details->>'to' = 'done'),
                  CASE WHEN p.status = 'done' THEN p.updated_at END) AS done_at,
         (SELECT jsonb_build_object('result', a.details->>'result', 'estimate', a.details->>'estimate')
            FROM activity_log a WHERE a.issue_id = p.id AND a.action = 'intake_call'
            ORDER BY a.created_at DESC, a.id DESC LIMIT 1) AS intake,
         (EXISTS (SELECT 1 FROM activity_log a WHERE a.issue_id = p.id AND a.action = 'workflow_escalated')
          OR COALESCE((SELECT a.details->>'result' FROM activity_log a WHERE a.issue_id = p.id AND a.action = 'intake_call'
                       ORDER BY a.created_at DESC, a.id DESC LIMIT 1), '') = 'escalated') AS escalated
  FROM picked p
), task_rows AS (
  SELECT q.id, q.issue_id, q.status, q.created_at, q.started_at, q.completed_at, q.workflow_step,
         (SELECT a.details->>'role' FROM activity_log a
           WHERE a.issue_id = q.issue_id AND a.action = 'workflow_role' AND a.details->>'task_id' = q.id::text
           ORDER BY a.created_at DESC, a.id DESC LIMIT 1) AS role,
         rt.provider, COALESCE(q.applied_model, q.claimed_model, ag.model) AS model
  FROM agent_task_queue q
  JOIN picked p ON p.id = q.issue_id
  LEFT JOIN agent ag ON ag.id = q.agent_id
  LEFT JOIN agent_runtime rt ON rt.id = ag.runtime_id
), usage_rows AS (
  SELECT u.task_id, (u.created_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date::text AS day,
         SUM(u.input_tokens + u.output_tokens + u.cache_read_tokens + u.cache_write_tokens)::bigint AS tokens
  FROM task_usage u WHERE u.task_id IN (SELECT id FROM task_rows)
  GROUP BY 1, 2
), total_rows AS (
  SELECT (u.created_at AT TIME ZONE 'Asia/Ho_Chi_Minh')::date::text AS day,
         SUM(u.input_tokens + u.output_tokens + u.cache_read_tokens + u.cache_write_tokens)::bigint AS tokens
  FROM task_usage u WHERE u.created_at >= (SELECT min(created_at) FROM picked) - interval '1 day'
  GROUP BY 1
)
SELECT json_build_object(
  'issues', COALESCE((SELECT json_agg(r) FROM issue_rows r), '[]'::json),
  'tasks', COALESCE((SELECT json_agg(r) FROM task_rows r), '[]'::json),
  'usage', COALESCE((SELECT json_agg(r) FROM usage_rows r), '[]'::json),
  'totals', COALESCE((SELECT json_agg(r) FROM total_rows r), '[]'::json))`;
}

export async function queryData(databaseUrl, opts, exec) {
  return psqlJson(databaseUrl, buildQuery(opts), exec);
}

const readJson = (path) => JSON.parse(readFileSync(path, 'utf8'));

async function main(argv) {
  const arg = (name) => (argv.includes(name) ? argv[argv.indexOf(name) + 1] : undefined);
  if (!process.env.DATABASE_URL) throw new Error('DATABASE_URL is required');
  const issues = arg('--issues')?.split(',');
  const opts = { start: arg('--start'), workspace: arg('--workspace'), issues };
  const asOf = arg('--as-of') ? new Date(arg('--as-of')) : new Date();
  if (Number.isNaN(asOf.getTime())) throw new Error('--as-of must be a date');
  const labsFile = arg('--labs');
  const labs = labsFile ? readJson(labsFile) : JSON.parse(process.env.MULTICA_VOTE_GATE_LABS || '{}');
  const gatewayUsd = arg('--gateway-usd') ? readJson(arg('--gateway-usd')) : null;
  const data = await queryData(process.env.DATABASE_URL, opts);
  const result = measure(data, { start: opts.start, asOf, gatewayUsd, labs, issuesOnly: Boolean(issues) });
  process.stdout.write(argv.includes('--json') ? `${JSON.stringify(result, null, 2)}\n` : renderReport(result));
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((e) => { console.error(e.message); process.exit(1); });
}

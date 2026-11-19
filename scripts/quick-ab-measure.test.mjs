import test from 'node:test';
import assert from 'node:assert/strict';
import { measure, renderReport, queryData } from './quick-ab-measure.mjs';

const DAY = 86400e3;
const START = new Date('2026-11-21T00:00:00Z'); // card 4 starts_at
const at = (days, hours = 0) => new Date(START.getTime() + days * DAY + hours * 3600e3).toISOString();
const asOf = (days) => new Date(START.getTime() + days * DAY);
let seq = 0;

// One arm issue: created `day` days after START, with `runs` finished one-hour tasks.
function issue(arm, n, { day = n, status = 'done', doneAfterH = 5, runs = 2, parent = null, source = 'arm', extra = {} } = {}) {
  const id = `${arm}-${n}`;
  const created = at(day);
  return {
    issue: {
      id, number: 1000 + n + (arm === 'Q' ? 0 : 500), status, created_at: created,
      done_at: status === 'done' ? at(day, doneAfterH) : null,
      parent_issue_id: parent, arm, entry_source: source, intake: null, escalated: false, ...extra,
    },
    tasks: Array.from({ length: runs }, (_, i) => ({
      id: `${id}-t${i}`, issue_id: id, status: 'completed', created_at: at(day, i),
      started_at: at(day, i), completed_at: at(day, i + 1), workflow_step: 'in_progress', role: null,
      provider: 'claude', model: 'sonnet',
    })),
  };
}
const pack = (items) => ({ issues: items.map((x) => x.issue), tasks: items.flatMap((x) => x.tasks), usage: [], totals: [] });
const cohort = (arm, count, opts = {}) => Array.from({ length: count }, (_, i) => issue(arm, i + 1, { day: i, ...opts }));
const run = (items, days, extra = {}) => measure(pack(items), { start: START, asOf: asOf(days), ...extra });

test('9 issues in one arm at day 21 -> inconclusive, counts stated, nothing scored as a recommendation', () => {
  const r = run([...cohort('Q', 9), ...cohort('D', 10)], 21);
  assert.equal(r.verdict, 'inconclusive');
  assert.deepEqual(r.counts, { Q: 9, D: 10 });
  assert.ok(r.reasons.includes('cohort_below_10:Q'));
  assert.equal(r.arms, null);
});

test('10/10 with one issue 5 days old and not done on day 21 -> inconclusive, immature issue listed', () => {
  const items = [...cohort('Q', 10), ...cohort('D', 10)];
  items[9] = issue('Q', 10, { day: 16, status: 'in_progress' }); // 5 days old at day 21
  const r = run(items, 21);
  assert.equal(r.verdict, 'inconclusive');
  assert.deepEqual(r.immature.map((x) => x.id), ['Q-10']);
  assert.ok(r.reasons.includes('immature_scored_issue'));
});

test('10/10 all mature (done, or 7 days old) -> scored', () => {
  const items = [...cohort('Q', 10), ...cohort('D', 10)];
  items[3] = issue('Q', 4, { day: 3, status: 'in_progress' }); // not done but 18 days old at day 21
  const r = run(items, 21);
  assert.equal(r.verdict, 'scored');
  assert.equal(r.issues.length, 20);
  assert.equal(r.arms.Q.issues, 10);
});

test('before day 21 an incomplete or immature cohort is pending, not inconclusive', () => {
  const r = run([...cohort('Q', 9), ...cohort('D', 10)], 10);
  assert.equal(r.verdict, 'pending');
});

test('scored once both cohorts are complete and mature before day 21 (first day that holds)', () => {
  const r = run([...cohort('Q', 10), ...cohort('D', 10)], 12);
  assert.equal(r.verdict, 'scored');
});

test('first 10 per arm in creation order are scored; the rest are listed, not scored', () => {
  const items = [...cohort('Q', 12), ...cohort('D', 10)];
  const r = run(items, 21);
  assert.equal(r.verdict, 'scored');
  assert.equal(r.issues.filter((x) => x.arm === 'Q').length, 10);
  assert.deepEqual(r.unscored.map((x) => x.id), ['Q-11', 'Q-12']);
  assert.deepEqual(r.counts, { Q: 12, D: 10 });
});

test('only root issues with a quick_entry arm row after the start are eligible', () => {
  const items = [
    ...cohort('Q', 10), ...cohort('D', 10),
    issue('Q', 90, { parent: 'Q-1' }),                    // child
    issue('Q', 91, { source: 'override' }),               // member override, not a trial draw
    issue('D', 92, { day: -3 }),                          // before card 4
  ];
  const r = run(items, 21);
  assert.deepEqual(r.counts, { Q: 10, D: 10 });
  assert.equal(r.unscored.length, 0);
});

test('daily counts per arm are grouped by ICT day of creation', () => {
  const items = [issue('Q', 1, { day: 0 }), issue('Q', 2, { day: 0, ...{} }), issue('D', 3, { day: 1 })];
  items[1].issue.created_at = '2026-11-21T20:00:00Z'; // 21 Nov 20:00 UTC = 22 Nov 03:00 ICT
  const r = run(items, 3);
  assert.deepEqual(r.daily, [
    { day: '2026-11-21', Q: 1, D: 0 },
    { day: '2026-11-22', Q: 1, D: 1 },
  ]);
});

test('runs count every task state from create to done; later tasks and a day-7 cut are excluded', () => {
  const x = issue('D', 1, { day: 0, status: 'done', doneAfterH: 5, runs: 2 });
  x.tasks.push({ ...x.tasks[0], id: 'f', status: 'failed', created_at: at(0, 2) });
  x.tasks.push({ ...x.tasks[0], id: 'c', status: 'cancelled', created_at: at(0, 3) });
  x.tasks.push({ ...x.tasks[0], id: 'after-done', created_at: at(0, 6) });
  const y = issue('Q', 2, { day: 0, status: 'in_progress', runs: 1 });
  y.tasks.push({ ...y.tasks[0], id: 'd7', created_at: at(7, 0) }); // exactly day 7 is outside
  y.tasks.push({ ...y.tasks[0], id: 'd6', created_at: at(6, 23) });
  const r = run([x, y], 30);
  const byId = Object.fromEntries(r.issues.map((i) => [i.id, i]));
  assert.equal(byId['D-1'].runs, 4);
  assert.equal(byId['Q-2'].runs, 2);
  assert.equal(byId['Q-2'].unfinishedAtDay7, true);
});

test('actual hours = sum of run durations; span = create to window end', () => {
  const x = issue('D', 1, { day: 0, runs: 3, doneAfterH: 8 });
  const [i] = run([x], 30).issues;
  assert.equal(i.runHours, 3);
  assert.equal(i.spanHours, 8);
});

test('USD is the gateway day total split by the issue share of Multica tokens that day (CHE-7648)', () => {
  const x = issue('D', 1, { day: 0, runs: 2 });
  const data = pack([x]);
  // day 21 Nov (ICT): issue used 300 of 1000 Multica tokens; gateway paid 200 USD. Day 22: 100 of 400 tokens, 80 USD.
  data.usage = [
    { task_id: 'D-1-t0', day: '2026-11-21', tokens: 200 }, { task_id: 'D-1-t1', day: '2026-11-21', tokens: 100 },
    { task_id: 'D-1-t1', day: '2026-11-22', tokens: 100 },
    { task_id: 'other', day: '2026-11-21', tokens: 5000 }, // not on this issue: ignored
  ];
  data.totals = [{ day: '2026-11-21', tokens: 1000 }, { day: '2026-11-22', tokens: 400 }];
  const r = measure(data, { start: START, asOf: asOf(30), gatewayUsd: { '2026-11-21': 200, '2026-11-22': 80 } });
  assert.equal(r.issues[0].usd, 200 * 0.3 + 80 * 0.25); // 80
  assert.deepEqual(r.issues[0].usdMissingDays, []);
});

test('a day with no gateway figure gives null USD with the day named, never a smaller number', () => {
  const x = issue('D', 1, { day: 0, runs: 1 });
  const data = pack([x]);
  data.usage = [{ task_id: 'D-1-t0', day: '2026-11-21', tokens: 10 }, { task_id: 'D-1-t0', day: '2026-11-22', tokens: 10 }];
  data.totals = [{ day: '2026-11-21', tokens: 100 }, { day: '2026-11-22', tokens: 100 }];
  const r = measure(data, { start: START, asOf: asOf(30), gatewayUsd: { '2026-11-21': 50 } });
  assert.equal(r.issues[0].usd, null);
  assert.deepEqual(r.issues[0].usdMissingDays, ['2026-11-22']);
});

test('no gateway file at all -> null USD', () => {
  assert.equal(run([issue('D', 1, { day: 0 })], 30).issues[0].usd, null);
});

test('same-lab review count: a review task whose lab equals an author lab; unknown labs are counted apart', () => {
  const x = issue('Q', 1, { day: 0, runs: 1 }); // author: claude/sonnet
  x.tasks.push({ ...x.tasks[0], id: 'r1', workflow_step: 'agent_review', provider: 'codex', model: 'gpt-6' }); // cross-lab
  x.tasks.push({ ...x.tasks[0], id: 'r2', workflow_step: 'agent_review', provider: 'claude', model: 'opus' }); // same lab
  x.tasks.push({ ...x.tasks[0], id: 'r3', workflow_step: 'agent_review', provider: 'x', model: 'y' });         // unknown
  x.tasks.push({ ...x.tasks[0], id: 'r4', workflow_step: 'in_progress', role: 'reviewer', provider: 'claude', model: 'sonnet' }); // role row wins
  const labs = { 'claude/sonnet': 'anthropic', 'claude/opus': 'anthropic', 'codex/gpt-6': 'openai' };
  const [i] = run([x], 30, { labs }).issues;
  assert.equal(i.reviews, 4);
  assert.equal(i.sameLabReviews, 2);
  assert.equal(i.unknownLabReviews, 1);
});

test('escalation flag and intake_call vs actual runs and hours', () => {
  const x = issue('Q', 1, { day: 0, runs: 3, extra: { escalated: true, intake: { result: 'escalated', estimate: 2 } } });
  const y = issue('Q', 2, { day: 1, runs: 1, extra: { intake: { result: 'simple', estimate: 1.5 } } });
  const z = issue('D', 3, { day: 1, runs: 1 });
  const r = run([x, y, z], 30);
  const byId = Object.fromEntries(r.issues.map((i) => [i.id, i]));
  assert.equal(byId['Q-1'].escalated, true);
  assert.deepEqual([byId['Q-1'].intakeCall, byId['Q-1'].estimateHours, byId['Q-1'].runs, byId['Q-1'].runHours], ['escalated', 2, 3, 3]);
  assert.deepEqual([byId['Q-2'].intakeCall, byId['Q-2'].estimateHours], ['simple', 1.5]);
  assert.equal(byId['D-3'].intakeCall, 'none');
});

test('dry run: 5 finished Default issues match a hand count', () => {
  // Hand count: runs 3,2,4,1,5 -> total 15, median 3; run hours equal runs (1 h each); one gateway day, 100 USD,
  // Multica tokens that day 2000, issue tokens 100,100,200,100,500 -> USD 5,5,10,5,25.
  const runsEach = [3, 2, 4, 1, 5];
  const items = runsEach.map((n, i) => issue('D', i + 1, { day: 0, runs: n }));
  const data = pack(items);
  const tokens = [100, 100, 200, 100, 500];
  data.usage = items.map((x, i) => ({ task_id: `${x.issue.id}-t0`, day: '2026-11-21', tokens: tokens[i] }));
  data.totals = [{ day: '2026-11-21', tokens: 2000 }];
  const r = measure(data, { start: START, asOf: asOf(30), gatewayUsd: { '2026-11-21': 100 }, issuesOnly: true });
  assert.deepEqual(r.issues.map((i) => i.runs), runsEach);
  assert.deepEqual(r.issues.map((i) => i.usd), [5, 5, 10, 5, 25]);
  assert.equal(r.issues.reduce((s, i) => s + i.runHours, 0), 15);
  assert.equal(r.verdict, 'listed'); // a dry run lists issues; no cohort verdict
});

test('report prints the verdict, daily counts and one line per scored issue', () => {
  const text = renderReport(run([...cohort('Q', 9), ...cohort('D', 10)], 21));
  assert.match(text, /inconclusive/);
  assert.match(text, /2026-11-21\s+1\s+1/);
  assert.match(text, /Q 9\b.*D 10\b/s);
});

const URL_WITH_SECRET = 'postgres://app:s3cr3tpw@db.example:5433/multica?sslmode=require';
const EMPTY = JSON.stringify({ issues: [], tasks: [], usage: [], totals: [] });

test('the query is read-only, uses PG* env for credentials and validates its inputs', async () => {
  let call;
  await queryData(URL_WITH_SECRET, { start: START }, async (cmd, args, opts) => { call = { cmd, args, env: opts.env }; return { stdout: EMPTY }; });
  assert.equal(call.cmd, 'psql');
  const sql = call.args.join(' ');
  assert.ok(!sql.includes('s3cr3tpw'));
  assert.match(sql, /quick_entry/);
  assert.match(sql, /intake_call/);
  assert.doesNotMatch(sql, /\b(INSERT|UPDATE|DELETE|DROP|ALTER|TRUNCATE)\b/i);
  assert.equal(call.env.PGPASSWORD, 's3cr3tpw');
  await assert.rejects(queryData(URL_WITH_SECRET, { start: START, workspace: "x'; DROP TABLE issue;--" }, async () => ({ stdout: EMPTY })), /workspace/);
  await assert.rejects(queryData(URL_WITH_SECRET, { start: START, issues: ['1; DROP'] }, async () => ({ stdout: EMPTY })), /issues/);
});

test('a failing psql never echoes the command or the password', async () => {
  const failing = async (cmd, args) => {
    const err = new Error(`Command failed: ${cmd} ${args.join(' ')} ${URL_WITH_SECRET}`);
    err.stderr = 'psql: error: password authentication failed\n';
    throw err;
  };
  await assert.rejects(queryData(URL_WITH_SECRET, { start: START }, failing), (e) => {
    assert.ok(!e.message.includes('s3cr3tpw'));
    assert.ok(!e.message.includes('Command failed'));
    return true;
  });
});

// Background agent usage by purpose (CHE-4013 step 14): claim time, run time and
// USD per 100 replies for each `bg:*` job, from existing task and task_usage rows.
//
//   DATABASE_URL=... node scripts/background-agent-usage.mjs [--days 3] [--json]
//
// Reply = a completed task. Failed attempts add cost and failover but no reply.
// USD is what the provider charged (task_usage.cost_usd_ticks, 1e-10 USD), divided by
// the replies that carry a price; unpriced replies are counted and shown, and a
// purpose with no priced reply reports null, never 0.
// The connection reaches psql through PG* environment variables, never argv, and
// failures print only psql's own message, never the command or the URL.
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { pathToFileURL } from 'node:url';

const TICKS_PER_USD = 1e10;

const QUERY = (days) => `
SELECT COALESCE(json_agg(t), '[]'::json) FROM (
  SELECT q.purpose, q.status, q.created_at, q.dispatched_at, q.started_at, q.completed_at,
         COALESCE(SUM(u.cost_usd_ticks), 0)::bigint AS cost_usd_ticks,
         COALESCE(SUM(u.input_tokens), 0)::bigint AS input_tokens,
         COALESCE(SUM(u.output_tokens), 0)::bigint AS output_tokens
  FROM agent_task_queue q
  LEFT JOIN task_usage u ON u.task_id = q.id
  WHERE q.purpose LIKE 'bg:%' AND q.created_at > now() - interval '${Number(days)} days'
  GROUP BY q.id
) t`;

const seconds = (from, to) => (from && to ? (new Date(to) - new Date(from)) / 1000 : null);

function stats(values) {
  const v = values.filter((x) => x !== null).sort((a, b) => a - b);
  if (!v.length) return null;
  const at = (p) => v[Math.min(v.length - 1, Math.ceil(p * v.length) - 1)];
  return { p50: at(0.5), p95: at(0.95), max: v[v.length - 1] };
}

export function summarize(rows) {
  const groups = new Map();
  for (const row of rows) {
    if (!row.purpose?.startsWith('bg:')) continue;
    if (!groups.has(row.purpose)) groups.set(row.purpose, []);
    groups.get(row.purpose).push(row);
  }
  return [...groups].sort(([a], [b]) => a.localeCompare(b)).map(([purpose, tasks]) => {
    const done = tasks.filter((t) => t.status === 'completed');
    const replies = done.length;
    const pricedReplies = done.filter((t) => Number(t.cost_usd_ticks) > 0).length;
    const ticks = tasks.reduce((sum, t) => sum + Number(t.cost_usd_ticks), 0);
    return {
      purpose,
      tasks: tasks.length,
      replies,
      unpricedReplies: replies - pricedReplies,
      failed: tasks.filter((t) => t.status === 'failed').length,
      claimSeconds: stats(tasks.map((t) => seconds(t.created_at, t.dispatched_at ?? t.started_at))),
      runSeconds: stats(tasks.map((t) => seconds(t.started_at, t.completed_at))),
      usdPer100Replies: ticks > 0 && pricedReplies > 0 ? (ticks / TICKS_PER_USD / pricedReplies) * 100 : null,
    };
  });
}

const cell = (s) => (s ? `${s.p50}/${s.p95}` : '-');

export function renderReport(groups) {
  const lines = groups.map((g) => [
    g.purpose.padEnd(18), String(g.tasks).padEnd(6), String(g.replies).padEnd(8), String(g.unpricedReplies).padEnd(9), String(g.failed).padEnd(7),
    cell(g.claimSeconds).padEnd(11), cell(g.runSeconds).padEnd(11),
    g.usdPer100Replies === null ? 'n/a' : g.usdPer100Replies.toFixed(2),
  ].join(' '));
  const head = ['purpose'.padEnd(18), 'tasks'.padEnd(6), 'replies'.padEnd(8), 'unpriced'.padEnd(9), 'failed'.padEnd(7),
    'claim p50/95'.padEnd(11), 'run p50/95'.padEnd(11), 'USD/100'].join(' ');
  return [head, ...lines].join('\n') + '\n';
}

const run = promisify(execFile);

// psql reads the connection from PG* variables, so the URL never appears in argv
// (visible in `ps`) or in an execFile error message.
export async function queryRows(databaseUrl, days, exec = run) {
  let url;
  try {
    url = new URL(databaseUrl);
  } catch {
    throw new Error('DATABASE_URL is not a valid connection URL');
  }
  const env = { ...process.env };
  for (const k of Object.keys(env)) if (k.startsWith('PG')) delete env[k];
  Object.assign(env, {
    PGHOST: url.hostname,
    PGPORT: url.port,
    PGUSER: decodeURIComponent(url.username),
    PGPASSWORD: decodeURIComponent(url.password),
    PGDATABASE: decodeURIComponent(url.pathname.slice(1)),
    PGSSLMODE: url.searchParams.get('sslmode') ?? '',
  });
  for (const k of Object.keys(env)) if (env[k] === '' && k.startsWith('PG')) delete env[k];
  try {
    const { stdout } = await exec('psql', ['-At', '-c', QUERY(days)], { env });
    return JSON.parse(stdout);
  } catch (e) {
    const detail = String(e.stderr ?? '').trim().split('\n').pop();
    throw new Error(`psql failed${detail ? `: ${detail}` : ''}`);
  }
}

async function main(argv) {
  const days = argv.includes('--days') ? argv[argv.indexOf('--days') + 1] : '3';
  if (!/^\d+$/.test(days)) throw new Error('--days must be a whole number');
  if (!process.env.DATABASE_URL) throw new Error('DATABASE_URL is required');
  const groups = summarize(await queryRows(process.env.DATABASE_URL, days));
  process.stdout.write(argv.includes('--json') ? `${JSON.stringify(groups, null, 2)}\n` : renderReport(groups));
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((e) => { console.error(e.message); process.exit(1); });
}

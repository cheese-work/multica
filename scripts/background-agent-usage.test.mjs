import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { summarize, renderReport, queryRows } from './background-agent-usage.mjs';

const rows = JSON.parse(readFileSync(new URL('./fixtures/background-agent-usage.json', import.meta.url)));

test('groups background tasks by purpose and ignores ordinary tasks', () => {
  const out = summarize(rows);
  assert.deepEqual(out.map((g) => g.purpose), ['bg:dm_title', 'bg:quick_actions']);
});

test('claim time, run time and USD per 100 replies for quick_actions', () => {
  const g = summarize(rows).find((x) => x.purpose === 'bg:quick_actions');
  assert.equal(g.tasks, 3);
  assert.equal(g.replies, 2); // completed tasks
  assert.equal(g.failed, 1);
  assert.deepEqual(g.claimSeconds, { p50: 2, p95: 3, max: 3 });
  assert.deepEqual(g.runSeconds, { p50: 10, p95: 20, max: 20 });
  // 60,000,000 ticks of 1e-10 USD = 0.006 USD over 2 replies = 0.30 USD per 100
  assert.equal(g.usdPer100Replies, 0.3);
});

test('a purpose with no provider-priced task reports no USD, never zero', () => {
  const out = summarize([{ ...rows[2], status: 'completed' }]);
  assert.equal(out[0].usdPer100Replies, null);
});

test('report is a plain table with one line per purpose', () => {
  const text = renderReport(summarize(rows));
  assert.match(text, /bg:quick_actions\s+3\s+2\s+/);
  assert.match(text, /0\.30/);
  assert.equal(text.trim().split('\n').length, 3);
});

test('mixed priced and unpriced replies: USD is over priced replies and the gap is shown', () => {
  const done = (cost) => ({ ...rows[0], status: 'completed', cost_usd_ticks: cost });
  const [g] = summarize([done(20000000), done(0)]);
  assert.equal(g.replies, 2);
  assert.equal(g.unpricedReplies, 1);
  assert.equal(g.usdPer100Replies, 0.2); // 0.002 USD over 1 priced reply
  assert.match(renderReport([g]), /unpriced/);
});

const URL_WITH_SECRET = 'postgres://app:s3cr3tpw@db.example:5433/multica?sslmode=require';

test('database credentials go through the environment, never psql arguments', async () => {
  let call;
  await queryRows(URL_WITH_SECRET, 3, async (cmd, args, opts) => {
    call = { cmd, args, env: opts.env };
    return { stdout: '[]' };
  });
  assert.equal(call.cmd, 'psql');
  assert.ok(!call.args.join(' ').includes('s3cr3tpw'));
  assert.ok(!call.args.join(' ').includes('db.example'));
  assert.equal(call.env.PGPASSWORD, 's3cr3tpw');
  assert.equal(call.env.PGHOST, 'db.example');
  assert.equal(call.env.PGPORT, '5433');
  assert.equal(call.env.PGUSER, 'app');
  assert.equal(call.env.PGDATABASE, 'multica');
  assert.equal(call.env.PGSSLMODE, 'require');
});

test('a failing psql never echoes the command, arguments or password', async () => {
  const failing = async (cmd, args) => {
    const err = new Error(`Command failed: ${cmd} ${args.join(' ')} ${URL_WITH_SECRET}`);
    err.stderr = 'psql: error: connection to server at "db.example" failed: FATAL: password authentication failed\n';
    throw err;
  };
  await assert.rejects(queryRows(URL_WITH_SECRET, 3, failing), (e) => {
    assert.ok(!e.message.includes('s3cr3tpw'));
    assert.ok(!e.message.includes('Command failed'));
    assert.match(e.message, /password authentication failed/);
    return true;
  });
});

test('a malformed DATABASE_URL is rejected without echoing it', async () => {
  await assert.rejects(queryRows('not a url s3cr3tpw', 3, async () => ({ stdout: '[]' })), (e) => {
    assert.ok(!e.message.includes('s3cr3tpw'));
    return true;
  });
});

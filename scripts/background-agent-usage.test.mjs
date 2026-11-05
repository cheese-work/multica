import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { summarize, renderReport } from './background-agent-usage.mjs';

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

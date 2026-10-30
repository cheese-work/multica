import test from 'node:test';
import assert from 'node:assert/strict';
import { renderDocument, sweepOnce } from './decision-answer-sweep.mjs';

const item = (over = {}) => ({
  answer_id: 'a1', state_version: 3, issue: 'CHE-12', ask: 'Ship it?', class: 'reversible',
  quote: '> Yes, ship it.', answered_at: '2026-10-30T16:30:00Z', state: 'effective',
  disclaim_reason: null, project_title: 'Cheese Work Setup', ...over,
});

test('renders the exact quote, ICT time, state and the controlled tags', () => {
  const doc = renderDocument(item());
  assert.equal(doc.document_id, 'decision-answer:a1');
  assert.ok(doc.content.includes('> Yes, ship it.'));
  assert.ok(doc.content.includes('30 Oct 2026, 23:30 ICT'));
  assert.ok(doc.content.includes('State: effective'));
  assert.ok(doc.content.includes('state_version: 3'));
  assert.deepEqual(doc.tags, ['kind:decision', 'issue:CHE-12', 'source:agent', 'project:cheese-work-setup']);
});

test('a withdrawn answer keeps the quote and carries the reason', () => {
  const doc = renderDocument(item({ state: 'withdrawn', disclaim_reason: 'agent_veto' }));
  assert.ok(doc.content.includes('State: withdrawn (reason: agent_veto)'));
  assert.ok(doc.content.includes('> Yes, ship it.'));
});

test('no project means no project tag', () => {
  assert.ok(!renderDocument(item({ project_title: null })).tags.some((t) => t.startsWith('project:')));
});

const run = async (items, retainFails = new Set()) => {
  const retained = [], acked = [];
  const result = await sweepOnce({
    readFeed: async () => items,
    retain: async (doc) => { if (retainFails.has(doc.document_id)) throw new Error('down'); retained.push(doc); },
    ack: async (id, version) => { acked.push([id, version]); },
    log: () => {},
  });
  return { retained, acked, result };
};

test('effective and withdrawn are retained then acked; pending and not_delivered are only acked', async () => {
  const items = [item(), item({ answer_id: 'a2', state: 'withdrawn', disclaim_reason: 'human' }),
    item({ answer_id: 'a3', state: 'pending' }), item({ answer_id: 'a4', state: 'not_delivered' })];
  const { retained, acked, result } = await run(items);
  assert.deepEqual(retained.map((d) => d.document_id), ['decision-answer:a1', 'decision-answer:a2']);
  assert.deepEqual(acked.map((a) => a[0]), ['a1', 'a2', 'a3', 'a4']);
  assert.equal(result.failed, 0);
});

test('a retain failure is never acked, so the next run repeats the upsert', async () => {
  const { acked, result } = await run([item(), item({ answer_id: 'a2' })], new Set(['decision-answer:a1']));
  assert.deepEqual(acked, [['a2', 3]]);
  assert.equal(result.failed, 1);
});

// X99 sweep (CHE-2687): writes one Hindsight document per delivered decision
// answer and marks it withdrawn after a disclaim. Reads only the R2 feed
// (`multica decision feed`) with the dedicated decisions:feed PAT; never DMs.
//
// Env: MULTICA_TOKEN (sweep PAT), MULTICA_SERVER_URL, MULTICA_WORKSPACE_ID,
//      HINDSIGHT_URL, HINDSIGHT_TOKEN, HINDSIGHT_BANK (default multica-org).
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { pathToFileURL } from 'node:url';

const run = promisify(execFile);
const RETAINED_STATES = new Set(['effective', 'withdrawn']);

const slug = (title) => title.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');

const ictTime = (iso) => {
  const parts = Object.fromEntries(new Intl.DateTimeFormat('en-GB', {
    timeZone: 'Asia/Ho_Chi_Minh', day: '2-digit', month: 'short', year: 'numeric',
    hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
  }).formatToParts(new Date(iso)).map((p) => [p.type, p.value]));
  return `${parts.day} ${parts.month} ${parts.year}, ${parts.hour}:${parts.minute} ICT`;
};

export function renderDocument(item) {
  const state = item.state === 'withdrawn' ? `withdrawn (reason: ${item.disclaim_reason})` : item.state;
  const tags = ['kind:decision', `issue:${item.issue}`, 'source:agent'];
  const project = item.project_title ? slug(item.project_title) : '';
  if (project) tags.push(`project:${project}`);
  return {
    document_id: `decision-answer:${item.answer_id}`,
    tags,
    content: [
      `Decision ${item.issue} (class: ${item.class})`,
      `Ask: ${item.ask}`,
      `Answer time: ${ictTime(item.answered_at)}`,
      `State: ${state}`,
      `state_version: ${item.state_version}`,
      'Quote:',
      item.quote,
    ].join('\n'),
  };
}

// Retain, then ack. A retain failure is not acked, so the next run repeats the
// same idempotent upsert; pending and undelivered answers are only acked, and
// resurface when their state version rises.
export async function sweepOnce({ readFeed, retain, ack, log = console.error }) {
  let failed = 0;
  for (const item of await readFeed()) {
    try {
      if (RETAINED_STATES.has(item.state)) await retain(renderDocument(item));
      await ack(item.answer_id, item.state_version);
    } catch (error) {
      failed += 1;
      log(`answer ${item.answer_id}: ${error.message}`);
    }
  }
  return { failed };
}

async function main() {
  const bank = process.env.HINDSIGHT_BANK ?? 'multica-org';
  const multica = async (...args) => JSON.parse((await run('multica', [...args, '--output', 'json'])).stdout);
  const result = await sweepOnce({
    readFeed: async () => (await multica('decision', 'feed')).answers ?? [],
    retain: async (doc) => {
      const response = await fetch(`${process.env.HINDSIGHT_URL}/v1/default/banks/${bank}/memories`, {
        method: 'POST',
        headers: { authorization: `Bearer ${process.env.HINDSIGHT_TOKEN}`, 'content-type': 'application/json' },
        body: JSON.stringify({ async: false, items: [{ content: doc.content, document_id: doc.document_id, tags: doc.tags, context: 'decision answer' }] }),
      });
      if (!response.ok) throw new Error(`retain ${doc.document_id}: HTTP ${response.status}`);
    },
    ack: async (id, version) => { await multica('decision', 'feed', 'ack', id, '--version', String(version)); },
  });
  process.exitCode = result.failed ? 1 : 0;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) await main();

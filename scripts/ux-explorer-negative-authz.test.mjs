import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { runChecks } from './ux-explorer-negative-authz.mjs';

const SANDBOX = { id: 'sandbox-id', slug: 'ux-explorer-sandbox' };
const REAL = { workspaceId: 'real-ws', issueId: 'real-issue', attachmentId: 'real-att' };
const S = 'mul_seeder';
const E = 'mul_explorer';

// Fake API. `leak` names a route that wrongly lets the explorer through.
function fakeServer({ leak = '' } = {}) {
  const hits = [];
  const wneg = { id: 'wneg-id', issue: null, comments: 0, atts: 0, deleted: false };
  const srv = http.createServer((req, res) => {
    const who = req.headers.authorization === `Bearer ${S}` ? 'S' : req.headers.authorization === `Bearer ${E}` ? 'E' : '';
    const ws = req.headers['x-workspace-id'] || '';
    const url = new URL(req.url, 'http://x');
    const path = url.pathname;
    let body = '';
    req.on('data', (c) => { body += c; });
    req.on('end', () => {
      hits.push({ who, method: req.method, path, ws, body: body.length });
      const send = (code, obj) => { res.statusCode = code; res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(obj ?? {})); };
      const eMember = (w) => w === SANDBOX.id;
      if (!who) return send(401);
      if (who === 'E' && ws === REAL.workspaceId && req.method !== 'GET') return send(500, { error: 'write against real workspace' });
      if (req.method === 'POST' && path === '/api/workspaces') return send(201, { id: wneg.id, slug: JSON.parse(body).slug });
      if (req.method === 'DELETE' && path === `/api/workspaces/${wneg.id}`) { wneg.deleted = true; return send(204); }
      if (req.method === 'GET' && path === '/api/workspaces') {
        return send(200, who === 'E' ? [{ slug: SANDBOX.slug }] : [{ slug: 'x' }, ...(wneg.deleted ? [] : [{ slug: 'ux-negative' }])]);
      }
      if (req.method === 'GET' && path === `/api/workspaces/${wneg.id}`) {
        if (wneg.deleted) return send(404);
        return who === 'S' || leak === 'workspace' ? send(200, { id: wneg.id }) : send(404);
      }
      if (req.method === 'GET' && path === `/api/workspaces/${SANDBOX.id}`) return send(eMember(SANDBOX.id) ? 200 : 403, {});
      if (req.method === 'GET' && path === `/api/workspaces/${REAL.workspaceId}`) return send(403);
      if (ws === wneg.id) {
        if (who === 'E' && !(leak === 'write-issue' && req.method === 'POST' && path === '/api/issues')) return send(403);
        if (req.method === 'POST' && path === '/api/issues') { wneg.issue = { id: 'wneg-issue', identifier: 'UXN-1', status: 'todo' }; return send(201, wneg.issue); }
        if (req.method === 'POST' && path === '/api/issues/wneg-issue/comments') { wneg.comments++; return send(201, { id: 'c1' }); }
        if (req.method === 'POST' && path === '/api/upload-file') { wneg.atts++; return send(200, { id: 'wneg-att' }); }
        if (req.method === 'GET' && path === '/api/issues/wneg-issue') return send(200, wneg.issue);
        if (req.method === 'GET' && path === '/api/issues/wneg-issue/comments') return send(200, Array(wneg.comments).fill({}));
        if (req.method === 'GET' && path === '/api/issues/wneg-issue/attachments') return send(200, Array(wneg.atts).fill({}));
        if (req.method === 'GET' && path === '/api/issues') return send(200, { issues: wneg.issue ? [wneg.issue] : [] });
      }
      if (req.method === 'GET' && path === '/api/attachments/wneg-att/download') return send(who === 'S' ? 200 : 404);
      if (req.method === 'GET' && path === `/api/attachments/${REAL.attachmentId}/download`) return send(404);
      if (req.method === 'GET' && path === '/api/issues/boom') return send(500);
      if (req.method === 'GET' && path === `/api/issues/${REAL.issueId}`) return send(403);
      return send(404);
    });
  });
  return new Promise((resolve) => srv.listen(0, '127.0.0.1', () => resolve({
    server: `http://127.0.0.1:${srv.address().port}`, hits, wneg, close: () => new Promise((r) => srv.close(r)),
  })));
}

const cfg = (server, extra = {}) => ({
  server, sandboxId: SANDBOX.id, sandboxSlug: SANDBOX.slug, real: REAL,
  tokens: { seeder: S, explorer: E },
  probeWs: async () => ({ outcome: 'denied', detail: 'auth_error' }),
  ...extra,
});

test('all denials hold: pass, W-neg unchanged and deleted, no write against the real workspace', async () => {
  const f = await fakeServer();
  try {
    const out = await runChecks(cfg(f.server));
    assert.equal(out.stop, false, JSON.stringify(out.rows.filter((r) => !r.ok)));
    assert.ok(out.rows.every((r) => r.ok), JSON.stringify(out.rows.filter((r) => !r.ok)));
    const names = out.rows.map((r) => r.name).join('\n');
    for (const want of ['workspace read', 'issue by id', 'issue by identifier', 'comment list', 'attachment download', 'websocket subscribe',
      'create issue', 'add comment', 'change status', 'upload attachment', 'readback', 'workspace list', 'cheese-work issue', 'cheese-work attachment', 'cleanup']) {
      assert.ok(names.includes(want), `missing row: ${want}`);
    }
    assert.equal(f.wneg.deleted, true);
    assert.equal(f.hits.filter((h) => h.who === 'E' && h.ws === REAL.workspaceId && h.method !== 'GET').length, 0);
    assert.equal(f.hits.filter((h) => h.who === 'E' && h.ws === 'wneg-id' && h.method !== 'GET' && h.method !== 'POST' && h.method !== 'PUT').length, 0);
  } finally { await f.close(); }
});

test('rows hold status codes only, never a response body', async () => {
  const f = await fakeServer();
  try {
    const out = await runChecks(cfg(f.server));
    assert.ok(out.rows.every((r) => typeof r.got === 'string' && !r.got.includes('{')));
  } finally { await f.close(); }
});

test('an unexpected success stops the run, still deletes W-neg and flags the stop', async () => {
  const f = await fakeServer({ leak: 'write-issue' });
  try {
    const out = await runChecks(cfg(f.server));
    assert.equal(out.stop, true);
    const bad = out.rows.find((r) => r.name.includes('create issue'));
    assert.equal(bad.ok, false);
    assert.equal(f.wneg.deleted, true);
    assert.ok(!out.rows.some((r) => r.name.includes('cheese-work issue')), 'real workspace is not probed after a stop');
  } finally { await f.close(); }
});

test('a 500 is inconclusive: the row fails but the run does not stop', async () => {
  const f = await fakeServer();
  try {
    const out = await runChecks(cfg(f.server, { real: { ...REAL, issueId: 'boom' } }));
    const row = out.rows.find((r) => r.name.includes('cheese-work issue'));
    assert.equal(row.got, '500');
    assert.equal(row.ok, false);
    assert.equal(out.stop, false);
  } finally { await f.close(); }
});

test('a websocket auth_ack is an unexpected success', async () => {
  const f = await fakeServer();
  try {
    const out = await runChecks(cfg(f.server, { probeWs: async () => ({ outcome: 'ack', detail: 'auth_ack' }) }));
    assert.equal(out.stop, true);
    assert.equal(out.rows.find((r) => r.name.includes('websocket')).ok, false);
    assert.equal(f.wneg.deleted, true);
  } finally { await f.close(); }
});

test('the real workspace is never written to even when the explorer reads are denied', async () => {
  const f = await fakeServer();
  try {
    await runChecks(cfg(f.server));
    const realHits = f.hits.filter((h) => h.ws === REAL.workspaceId || h.path.includes(REAL.workspaceId));
    assert.ok(realHits.length > 0);
    assert.ok(realHits.every((h) => h.method === 'GET' && h.who === 'E'));
  } finally { await f.close(); }
});

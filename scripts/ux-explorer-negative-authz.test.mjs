import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runChecks, probeWs, inAgentContext } from './ux-explorer-negative-authz.mjs';

const SANDBOX = { id: 'sandbox-id', slug: 'ux-explorer-sandbox' };
const REAL = { workspaceId: 'real-ws', issueId: 'real-issue', attachmentId: 'real-att' };
const S = 'mul_seeder';
const E = 'mul_explorer';

// Fake API. `leak` names a route that wrongly lets the explorer through.
// `missing` names a real fixture (workspace|issue|attachment) the authorized reader cannot find.
function fakeServer({ leak = '', missing = '', extraWs = '', stall = '', missingDownload = false } = {}) {
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
        return send(200, who === 'E' ? [{ slug: SANDBOX.slug }, ...(extraWs ? [{ slug: extraWs }] : [])] : [{ slug: 'x' }, ...(wneg.deleted ? [] : [{ slug: 'ux-negative' }])]);
      }
      if (req.method === 'GET' && path === `/api/workspaces/${wneg.id}`) {
        if (wneg.deleted) return send(404);
        if (who === 'E' && stall === 'headers') return; // never responds
        if (who === 'E' && stall === 'body') { res.statusCode = 200; res.write('{"id":"'); return; } // 2xx whose body never ends
        return who === 'S' || leak === 'workspace' ? send(200, { id: wneg.id }) : send(404);
      }
      if (req.method === 'GET' && path === `/api/workspaces/${SANDBOX.id}`) return send(eMember(SANDBOX.id) ? 200 : 403, {});
      if (req.method === 'GET' && path === `/api/workspaces/${REAL.workspaceId}`) return send(who === 'S' ? (missing === 'workspace' ? 404 : 200) : 403);
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
      if (req.method === 'GET' && path === '/api/attachments/wneg-att/download') return send(who === 'S' ? (missingDownload ? 404 : 200) : 404);
      // Nonexistent and forbidden both read 404 to E, which is why S must prove the actual download works.
      // 302 is the presigned-redirect mode; 200 is the proxy mode.
      if (req.method === 'GET' && path === `/api/attachments/${REAL.attachmentId}/download`) {
        if (who !== 'S' || missing === 'attachment' || missing === 'download') return send(404);
        res.statusCode = 302; res.setHeader('location', 'https://signed.example/x'); return res.end();
      }
      if (req.method === 'GET' && path === '/api/issues/boom') return send(who === 'S' ? 200 : 500);
      if (req.method === 'GET' && path === `/api/issues/${REAL.issueId}`) return send(who === 'S' ? (missing === 'issue' ? 404 : 200) : 403);
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
  probeWs: async (url) => (url.includes(SANDBOX.id) ? { outcome: 'ack', detail: 'auth_ack' } : { outcome: 'denied', detail: 'membership-rejected' }),
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
    assert.ok(!out.rows.some((r) => r.name.includes('cheese-work issue by id')), 'real workspace is not probed by E after a stop');
  } finally { await f.close(); }
});

test('a 500 is inconclusive: the row fails but the run does not stop', async () => {
  const f = await fakeServer();
  try {
    const out = await runChecks(cfg(f.server, { real: { ...REAL, issueId: 'boom' } }));
    const row = out.rows.find((r) => r.name === 'cheese-work issue by id');
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
    assert.equal(out.rows.find((r) => r.name === 'W-neg websocket subscribe').ok, false);
    assert.equal(f.wneg.deleted, true);
  } finally { await f.close(); }
});

test('the real workspace is never written to even when the explorer reads are denied', async () => {
  const f = await fakeServer();
  try {
    await runChecks(cfg(f.server));
    const realHits = f.hits.filter((h) => h.ws === REAL.workspaceId || h.path.includes(REAL.workspaceId));
    assert.ok(realHits.length > 0);
    assert.ok(realHits.every((h) => h.method === 'GET'), 'reads only');
    assert.ok(realHits.some((h) => h.who === 'E'));
  } finally { await f.close(); }
});

test('a websocket that cannot reach the authorized sandbox fails the run: no denial is trusted', async () => {
  const f = await fakeServer();
  try {
    const out = await runChecks(cfg(f.server, { probeWs: async () => ({ outcome: 'inconclusive', detail: 'transport-error' }) }));
    assert.equal(out.rows.find((r) => r.name.includes('control: explorer websocket')).ok, false);
    assert.equal(out.rows.find((r) => r.name === 'W-neg websocket subscribe').ok, false);
    assert.ok(out.rows.some((r) => !r.ok));
    assert.equal(f.wneg.deleted, true);
  } finally { await f.close(); }
});

test('a transport or server error on W-neg is inconclusive and fails the row', async () => {
  const f = await fakeServer();
  try {
    const out = await runChecks(cfg(f.server, {
      probeWs: async (url) => (url.includes(SANDBOX.id) ? { outcome: 'ack', detail: 'auth_ack' } : { outcome: 'inconclusive', detail: 'closed' }),
    }));
    const row = out.rows.find((r) => r.name === 'W-neg websocket subscribe');
    assert.equal(row.ok, false);
    assert.equal(out.stop, false);
  } finally { await f.close(); }
});

for (const missing of ['workspace', 'issue', 'attachment', 'download']) {
  test(`a nonexistent real ${missing} fixture fails the run instead of passing as a 404 denial`, async () => {
    const f = await fakeServer({ missing });
    try {
      const out = await runChecks(cfg(f.server));
      const control = out.rows.find((r) => r.name.startsWith('control: cheese-work') && r.name.includes(missing === 'download' ? 'attachment' : missing));
      assert.equal(control.ok, false, JSON.stringify(out.rows));
      assert.ok(out.rows.some((r) => !r.ok));
      assert.equal(out.stop, false);
      assert.equal(f.wneg.deleted, true);
    } finally { await f.close(); }
  });
}

// Minimal WebSocket server: answers the handshake, then sends `frame` (a string) once the client frame arrives, or nothing.
function wsServer(mode) {
  const srv = http.createServer((req, res) => res.end());
  const socks = new Set();
  srv.on('upgrade', (req, socket) => {
    socks.add(socket);
    if (mode === 'refuse') { socket.end('HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n'); return; }
    const accept = createHash('sha1').update(`${req.headers['sec-websocket-key']}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest('base64');
    socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
    socket.on('error', () => {});
    socket.once('data', () => {
      if (mode === 'drop') { socket.destroy(); return; }
      const text = { ack: '{"type":"auth_ack"}', member: '{"error":"not a member of this workspace"}', other: '{"error":"membership lookup failed"}' }[mode];
      socket.write(Buffer.concat([Buffer.from([0x81, text.length]), Buffer.from(text)]));
    });
  });
  return new Promise((resolve) => srv.listen(0, '127.0.0.1', () => resolve({ url: `ws://127.0.0.1:${srv.address().port}/ws`, close: () => { socks.forEach((x) => x.destroy()); return new Promise((r) => srv.close(r)); } })));
}

for (const [mode, want] of [['ack', 'ack'], ['member', 'denied'], ['other', 'inconclusive'], ['drop', 'inconclusive'], ['refuse', 'inconclusive']]) {
  test(`probeWs: ${mode} -> ${want}`, async () => {
    const w = await wsServer(mode);
    try { assert.equal((await probeWs(w.url, 'tok', 3000)).outcome, want); } finally { await w.close(); }
  });
}

test('an unexpected extra workspace in E\'s list stops the run before any real-workspace probe', async () => {
  const f = await fakeServer({ extraWs: 'cheese-work' });
  try {
    const out = await runChecks(cfg(f.server));
    assert.equal(out.stop, true);
    assert.equal(out.rows.find((r) => r.name === 'E workspace list is the sandbox only').ok, false);
    assert.equal(f.hits.filter((h) => h.who === 'E' && (h.ws === REAL.workspaceId || h.path.includes(REAL.workspaceId))).length, 0);
    assert.ok(!out.rows.some((r) => r.name.startsWith('cheese-work ')), 'no real-workspace denial row after a stop');
    assert.equal(f.wneg.deleted, true);
  } finally { await f.close(); }
});

test('a successful explorer response is never body-read: a never-ending body does not delay STOP or cleanup', async () => {
  const f = await fakeServer({ stall: 'body' });
  try {
    const t0 = Date.now();
    const out = await runChecks(cfg(f.server, { requestTimeoutMs: 20000 }));
    assert.ok(Date.now() - t0 < 5000, 'run waited on a response body');
    assert.equal(out.stop, true);
    assert.equal(f.wneg.deleted, true);
  } finally { await f.close(); }
});

test('a request that never answers times out as an inconclusive row and the run still cleans up', async () => {
  const f = await fakeServer({ stall: 'headers' });
  try {
    const out = await runChecks(cfg(f.server, { requestTimeoutMs: 300 }));
    const row = out.rows.find((r) => r.name === 'W-neg workspace read');
    assert.equal(row.ok, false);
    assert.equal(row.got, 'error');
    assert.equal(out.stop, false);
    assert.equal(f.wneg.deleted, true);
  } finally { await f.close(); }
});

test('a W-neg attachment whose authorized download is broken cannot pass as a denial', async () => {
  const f = await fakeServer({ missingDownload: true });
  try {
    const out = await runChecks(cfg(f.server));
    assert.equal(out.rows.find((r) => r.name === 'control: seeder downloads W-neg attachment').ok, false);
    assert.equal(out.rows.find((r) => r.name === 'W-neg attachment download').ok, false);
    assert.equal(f.wneg.deleted, true);
  } finally { await f.close(); }
});

test('inAgentContext: task id, agent id, task config root and a daemon marker all refuse', () => {
  const dir = mkdtempSync(join(tmpdir(), 'uxn-'));
  try {
    assert.equal(inAgentContext({}, dir), false);
    assert.equal(inAgentContext({ MULTICA_DAEMON_PORT: '19514' }, dir), false, 'port alone is not task identity');
    for (const env of [{ MULTICA_TASK_ID: 't' }, { MULTICA_AGENT_ID: 'a' }, { MULTICA_TASK_CONFIG_ROOT: '/x' }]) {
      assert.equal(inAgentContext(env, dir), true, JSON.stringify(env));
    }
    mkdirSync(join(dir, '.multica'));
    writeFileSync(join(dir, '.multica', 'daemon_task_context.json'), '{"managed_by":"someone-else"}');
    assert.equal(inAgentContext({}, dir), false, 'foreign marker');
    writeFileSync(join(dir, '.multica', 'daemon_task_context.json'), '{"managed_by":"multica-daemon-task"}');
    mkdirSync(join(dir, 'a', 'b'), { recursive: true });
    assert.equal(inAgentContext({}, join(dir, 'a', 'b')), true, 'marker found in an ancestor');
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

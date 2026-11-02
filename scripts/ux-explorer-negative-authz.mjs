// Negative authorization checks for the UX explorer identity E (CHE-2978, B1b of CHE-2970).
// Run by a person in their own X99 shell, outside any agent task: the CLI refuses PATs in
// agent runs, and raw status codes need direct HTTP. Seeder S creates a disposable
// workspace W-neg that E is not a member of; E then tries to read and write it by known ID.
// Only status codes are recorded, never response bodies. No write is ever sent to a real workspace.
//
//   node scripts/ux-explorer-negative-authz.mjs
// Env: KEYDIR (default ~/.multica-explorer, needs seeder.pat and explorer.pat), MULTICA_SERVER_URL,
//      REAL_WORKSPACE_ID, REAL_ISSUE_ID, REAL_ATTACHMENT_ID (a cheese-work issue and attachment).
import { readFileSync, statSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const DENIED = new Set([401, 403, 404]);

async function call(server, token, method, path, { ws, json, form } = {}) {
  const headers = { Authorization: `Bearer ${token}` };
  if (ws) headers['X-Workspace-ID'] = ws;
  let body;
  if (json !== undefined) { headers['Content-Type'] = 'application/json'; body = JSON.stringify(json); }
  if (form) body = form;
  const res = await fetch(server + path, { method, headers, body, redirect: 'manual' });
  let data = null;
  if (res.status >= 200 && res.status < 300 && res.status !== 204) data = await res.json().catch(() => null);
  else await res.body?.cancel();
  return { status: res.status, data };
}

const listOf = (data, key) => (Array.isArray(data) ? data : data?.[key] ?? []);

// Real WebSocket probe: first-frame auth, then watch for auth_ack vs an error or close.
export function probeWs(url, token, timeoutMs = 10000) {
  return new Promise((resolve) => {
    const sock = new WebSocket(url);
    const done = (outcome, detail) => { clearTimeout(timer); try { sock.close(); } catch { /* already closed */ } resolve({ outcome, detail }); };
    const timer = setTimeout(() => done('inconclusive', 'timeout'), timeoutMs);
    sock.onopen = () => sock.send(JSON.stringify({ type: 'auth', payload: { token } }));
    sock.onmessage = (ev) => {
      const text = String(ev.data);
      if (text.includes('"auth_ack"')) done('ack', 'auth_ack');
      else if (text.includes('"error"')) done('denied', 'auth_error');
      else done('inconclusive', 'other-frame');
    };
    sock.onerror = () => done('denied', 'upgrade-refused');
    sock.onclose = () => done('denied', 'closed');
  });
}

export async function runChecks({ server, sandboxId, sandboxSlug, real, tokens, probeWs: probe = probeWs }) {
  const { seeder: S, explorer: E } = tokens;
  const rows = [];
  let stop = false;
  const add = (name, expect, got, ok) => { rows.push({ name, expect, got: String(got), ok }); return ok; };

  // An explorer attempt must be denied. A 2xx/3xx is an unexpected success and stops the run;
  // any other non-denial code (400, 5xx) proves nothing and fails the row without stopping.
  const expectDenied = async (name, method, path, opts) => {
    if (stop) return;
    const { status } = await call(server, E, method, path, opts);
    const ok = DENIED.has(status);
    if (status >= 200 && status < 400) stop = true;
    add(name, '401/403/404', status, ok);
  };

  const slug = `ux-negative-${Date.now().toString(36)}`;
  const created = await call(server, S, 'POST', '/api/workspaces', {
    json: { name: 'UX negative check (disposable)', slug, issue_prefix: 'UXN' },
  });
  if (created.status !== 201 && created.status !== 200) throw new Error(`W-neg create failed: ${created.status}`);
  const wneg = created.data.id;

  try {
    const issue = await call(server, S, 'POST', '/api/issues', { ws: wneg, json: { title: 'W-neg issue', status: 'todo' } });
    const issueId = issue.data?.id;
    const identifier = issue.data?.identifier;
    const seeded = add('seed W-neg issue', '2xx', issue.status, !!issueId);
    let attachmentId = null;
    if (seeded) {
      const comment = await call(server, S, 'POST', `/api/issues/${issueId}/comments`, { ws: wneg, json: { content: 'W-neg comment' } });
      add('seed W-neg comment', '2xx', comment.status, comment.status < 300);
      const form = new FormData();
      form.set('file', new Blob(['W-neg file']), 'w-neg.txt');
      form.set('issue_id', issueId);
      const upload = await call(server, S, 'POST', '/api/upload-file', { ws: wneg, form });
      attachmentId = upload.data?.id ?? null;
      add('seed W-neg attachment', '2xx', upload.status, !!attachmentId);
    }

    const controlS = await call(server, S, 'GET', `/api/workspaces/${wneg}`);
    const controlE = await call(server, E, 'GET', `/api/workspaces/${sandboxId}`);
    const controls = add('control: seeder reads W-neg', '200', controlS.status, controlS.status === 200)
      && add('control: explorer reads sandbox', '200', controlE.status, controlE.status === 200);
    if (!seeded || !controls) return { rows, stop };

    // 1. Reads against W-neg by known ID.
    await expectDenied('W-neg workspace read', 'GET', `/api/workspaces/${wneg}`);
    await expectDenied('W-neg issue by id', 'GET', `/api/issues/${issueId}`, { ws: wneg });
    await expectDenied('W-neg issue by identifier', 'GET', `/api/issues/${identifier}`, { ws: wneg });
    await expectDenied('W-neg comment list', 'GET', `/api/issues/${issueId}/comments`, { ws: wneg });
    if (attachmentId) await expectDenied('W-neg attachment download', 'GET', `/api/attachments/${attachmentId}/download`);
    else add('W-neg attachment download', '401/403/404', 'NOT-RUN (no attachment seeded)', false);
    if (!stop) {
      const wsUrl = `${server.replace(/^http/, 'ws')}/ws?workspace_id=${wneg}`;
      const { outcome, detail } = await probe(wsUrl, E);
      if (outcome === 'ack') stop = true;
      add('W-neg websocket subscribe', 'denied, no auth_ack', `${outcome} (${detail})`, outcome === 'denied');
    }

    // 2. Writes against W-neg only.
    await expectDenied('W-neg create issue', 'POST', '/api/issues', { ws: wneg, json: { title: 'E must not create this' } });
    await expectDenied('W-neg add comment', 'POST', `/api/issues/${issueId}/comments`, { ws: wneg, json: { content: 'E must not write this' } });
    await expectDenied('W-neg change status', 'PUT', `/api/issues/${issueId}`, { ws: wneg, json: { status: 'done' } });
    const form = new FormData();
    form.set('file', new Blob(['E must not upload this']), 'e.txt');
    form.set('issue_id', issueId);
    await expectDenied('W-neg upload attachment', 'POST', '/api/upload-file', { ws: wneg, form });

    if (!stop) {
      const [one, comments, atts, list] = await Promise.all([
        call(server, S, 'GET', `/api/issues/${issueId}`, { ws: wneg }),
        call(server, S, 'GET', `/api/issues/${issueId}/comments`, { ws: wneg }),
        call(server, S, 'GET', `/api/issues/${issueId}/attachments`, { ws: wneg }),
        call(server, S, 'GET', '/api/issues', { ws: wneg }),
      ]);
      const counts = `status=${one.data?.status} comments=${listOf(comments.data, 'comments').length} attachments=${listOf(atts.data, 'attachments').length} issues=${listOf(list.data, 'issues').length}`;
      add('W-neg readback unchanged', 'status=todo comments=1 attachments=1 issues=1', counts,
        counts === 'status=todo comments=1 attachments=1 issues=1');
    }

    // 3. E's own view and the real workspace (reads only).
    if (!stop) {
      const mine = await call(server, E, 'GET', '/api/workspaces');
      const slugs = listOf(mine.data, 'workspaces').map((w) => w.slug);
      add('E workspace list is the sandbox only', `[${sandboxSlug}]`, `count=${slugs.length} match=${slugs.length === 1 && slugs[0] === sandboxSlug}`,
        slugs.length === 1 && slugs[0] === sandboxSlug);
    }
    await expectDenied('cheese-work workspace read', 'GET', `/api/workspaces/${real.workspaceId}`);
    await expectDenied('cheese-work issue by id', 'GET', `/api/issues/${real.issueId}`, { ws: real.workspaceId });
    await expectDenied('cheese-work attachment download', 'GET', `/api/attachments/${real.attachmentId}/download`);
    return { rows, stop };
  } finally {
    const del = await call(server, S, 'DELETE', `/api/workspaces/${wneg}`).catch(() => ({ status: 'error' }));
    const after = await call(server, S, 'GET', `/api/workspaces/${wneg}`).catch(() => ({ status: 'error' }));
    add('cleanup: W-neg deleted', 'delete 2xx, then 403/404', `delete=${del.status} then=${after.status}`,
      del.status >= 200 && del.status < 300 && DENIED.has(after.status));
  }
}

function readPat(keydir, name) {
  const file = join(keydir, `${name}.pat`);
  const mode = (statSync(file).mode & 0o777).toString(8);
  if (mode !== '600') throw new Error(`${file} must be mode 600, found ${mode}`);
  return readFileSync(file, 'utf8').trim();
}

async function main() {
  if (process.env.MULTICA_TASK_ID) { console.error('refusing: running inside an agent task'); process.exit(2); }
  const keydir = process.env.KEYDIR ?? join(homedir(), '.multica-explorer');
  const out = await runChecks({
    server: process.env.MULTICA_SERVER_URL ?? 'https://congvc-c00.taila6fa8a.ts.net:19444',
    sandboxId: '0c251edf-434a-47e8-acdb-f9f060278d55',
    sandboxSlug: 'ux-explorer-sandbox',
    real: {
      workspaceId: process.env.REAL_WORKSPACE_ID ?? 'f2b734e0-b6de-4414-8a27-a2f69b0ef843',
      issueId: process.env.REAL_ISSUE_ID ?? '01a1981d-9257-7ad3-8833-273f0fc824f6',
      attachmentId: process.env.REAL_ATTACHMENT_ID ?? '01a19a28-1f5d-74e2-996b-f857f5287dc2',
    },
    tokens: { seeder: readPat(keydir, 'seeder'), explorer: readPat(keydir, 'explorer') },
  });
  console.log('| Check | Expected | Got | Result |\n|---|---|---|---|');
  for (const r of out.rows) console.log(`| ${r.name} | ${r.expect} | ${r.got} | ${r.ok ? 'PASS' : 'FAIL'} |`);
  const failed = out.rows.filter((r) => !r.ok).length;
  if (out.stop) console.log('STOP: unexpected success. Keep B2/B3 blocked, rotate E, file a security bug.');
  console.log(failed === 0 ? `PASS: ${out.rows.length}/${out.rows.length} checks` : `FAIL: ${failed} of ${out.rows.length} checks`);
  process.exit(out.stop ? 3 : failed ? 1 : 0);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main().catch((err) => { console.error(`error: ${err.message}`); process.exit(1); });
}

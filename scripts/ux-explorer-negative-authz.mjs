// Negative authorization checks for the UX explorer identity E (CHE-2978, B1b of CHE-2970).
// Run by a person in their own X99 shell, outside any agent task: the CLI refuses PATs in
// agent runs, and raw status codes need direct HTTP. Seeder S creates a disposable
// workspace W-neg that E is not a member of; E then tries to read and write it by known ID.
// Only status codes are recorded, never response bodies. No write is ever sent to a real workspace.
//
//   node scripts/ux-explorer-negative-authz.mjs
// Env: KEYDIR (default ~/.multica-explorer, needs seeder.pat and explorer.pat; optional reader.pat,
//      a member of the real workspace used only to confirm the real fixtures exist and download, default seeder), MULTICA_SERVER_URL,
//      REAL_WORKSPACE_ID, REAL_ISSUE_ID, REAL_ATTACHMENT_ID (a cheese-work issue and attachment).
import { existsSync, readFileSync, statSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { pathToFileURL } from 'node:url';

const DENIED = new Set([401, 403, 404]);

// Daemon-managed agent task markers, mirroring the CLI's strong task-identity signals
// (server/cmd/multica/cmd_agent.go inDaemonTaskIdentityContext). MULTICA_DAEMON_PORT alone is not one.
export function inAgentContext(env = process.env, cwd = process.cwd()) {
  if (env.MULTICA_TASK_ID || env.MULTICA_AGENT_ID || String(env.MULTICA_TASK_CONFIG_ROOT ?? '').trim()) return true;
  for (let dir = cwd; ; dir = dirname(dir)) {
    try {
      if (JSON.parse(readFileSync(join(dir, '.multica', 'daemon_task_context.json'), 'utf8')).managed_by === 'multica-daemon-task') return true;
    } catch { /* missing, unreadable or foreign marker: no signal here */ }
    if (dirname(dir) === dir) return false;
  }
}

// Each call is bounded by timeoutMs. A response body is read only when `data` is set (setup and readback
// by the seeder); every other call, including all explorer attempts, records the status and cancels the body.
// A transport error or timeout is status 'error': it fails the row and is never a denial.
async function call(server, token, method, path, { ws, json, form, data: wantData = false, timeoutMs = 15000 } = {}) {
  const headers = { Authorization: `Bearer ${token}` };
  if (ws) headers['X-Workspace-ID'] = ws;
  let body;
  if (json !== undefined) { headers['Content-Type'] = 'application/json'; body = JSON.stringify(json); }
  if (form) body = form;
  try {
    const res = await fetch(server + path, { method, headers, body, redirect: 'manual', signal: AbortSignal.timeout(timeoutMs) });
    let data = null;
    if (wantData && res.status >= 200 && res.status < 300 && res.status !== 204) data = await res.json().catch(() => null);
    else await res.body?.cancel().catch(() => {});
    return { status: res.status, data };
  } catch {
    return { status: 'error', data: null };
  }
}

const listOf = (data, key) => (Array.isArray(data) ? data : data?.[key] ?? []);

// The server's explicit membership rejection frame (server/internal/realtime/hub.go).
const MEMBERSHIP_REJECTION = 'not a member of this workspace';

// Real WebSocket probe: first-frame auth. Only an auth_ack ('ack') or the server's explicit
// membership rejection ('denied') is conclusive; transport errors, closes, timeouts and any
// other frame are 'inconclusive'.
export function probeWs(url, token, timeoutMs = 10000) {
  return new Promise((resolve) => {
    const sock = new WebSocket(url);
    const done = (outcome, detail) => { clearTimeout(timer); try { sock.close(); } catch { /* already closed */ } resolve({ outcome, detail }); };
    const timer = setTimeout(() => done('inconclusive', 'timeout'), timeoutMs);
    sock.onopen = () => sock.send(JSON.stringify({ type: 'auth', payload: { token } }));
    sock.onmessage = (ev) => {
      let frame = null;
      try { frame = JSON.parse(String(ev.data)); } catch { /* not JSON */ }
      if (frame?.type === 'auth_ack') done('ack', 'auth_ack');
      else if (frame?.error === MEMBERSHIP_REJECTION) done('denied', 'membership-rejected');
      else done('inconclusive', 'server-error-frame');
    };
    sock.onerror = () => done('inconclusive', 'transport-error');
    sock.onclose = () => done('inconclusive', 'closed');
  });
}

export async function runChecks({ server, sandboxId, sandboxSlug, real, tokens, probeWs: probe = probeWs, requestTimeoutMs = 15000 }) {
  // reader: an identity that can read the real workspace, to prove its fixtures exist (default: seeder).
  const { seeder: S, explorer: E, reader: R = S } = tokens;
  const rows = [];
  let stop = false;
  const request = (token, method, path, opts = {}) => call(server, token, method, path, { timeoutMs: requestTimeoutMs, ...opts });
  const add = (name, expect, got, ok) => { rows.push({ name, expect, got: String(got), ok }); return ok; };

  // An explorer attempt must be denied. A 2xx/3xx is an unexpected success and stops the run;
  // any other non-denial code (400, 5xx) proves nothing and fails the row without stopping.
  const expectDenied = async (name, method, path, opts) => {
    if (stop) return;
    const { status } = await request(E, method, path, opts);
    const ok = DENIED.has(status);
    if (status >= 200 && status < 400) stop = true;
    add(name, '401/403/404', status, ok);
  };

  const slug = `ux-negative-${Date.now().toString(36)}`;
  const created = await request(S, 'POST', '/api/workspaces', {
    data: true, json: { name: 'UX negative check (disposable)', slug, issue_prefix: 'UXN' },
  });
  if (created.status !== 201 && created.status !== 200) throw new Error(`W-neg create failed: ${created.status}`);
  const wneg = created.data.id;

  try {
    const issue = await request(S, 'POST', '/api/issues', { ws: wneg, data: true, json: { title: 'W-neg issue', status: 'todo' } });
    const issueId = issue.data?.id;
    const identifier = issue.data?.identifier;
    const seeded = add('seed W-neg issue', '2xx', issue.status, !!issueId);
    let attachmentId = null;
    if (seeded) {
      const comment = await request(S, 'POST', `/api/issues/${issueId}/comments`, { ws: wneg, json: { content: 'W-neg comment' } });
      add('seed W-neg comment', '2xx', comment.status, comment.status < 300);
      const form = new FormData();
      form.set('file', new Blob(['W-neg file']), 'w-neg.txt');
      form.set('issue_id', issueId);
      const upload = await request(S, 'POST', '/api/upload-file', { ws: wneg, form, data: true });
      attachmentId = upload.data?.id ?? null;
      add('seed W-neg attachment', '2xx', upload.status, !!attachmentId);
    }

    const controlS = await request(S, 'GET', `/api/workspaces/${wneg}`);
    const controlE = await request(E, 'GET', `/api/workspaces/${sandboxId}`);
    const controls = add('control: seeder reads W-neg', '200', controlS.status, controlS.status === 200)
      && add('control: explorer reads sandbox', '200', controlE.status, controlE.status === 200);
    if (!seeded || !controls) return { rows, stop };

    // Controls that make a denial meaningful: E can use the WebSocket where it is a member, and every
    // real fixture exists (an authorized read-only 2xx), else a 404 would pass as a denial.
    const wsBase = `${server.replace(/^http/, 'ws')}/ws?workspace_id=`;
    const wsCtl = await probe(wsBase + sandboxId, E);
    const wsControlOk = add('control: explorer websocket subscribes to sandbox', 'ack', `${wsCtl.outcome} (${wsCtl.detail})`, wsCtl.outcome === 'ack');
    // Download controls need a real 200 (proxy mode) or 302 (presigned redirect); the body is never read.
    const downloadOk = (status) => status === 200 || status === 302;
    let wnegDownloadOk = false;
    if (attachmentId) {
      const { status } = await request(S, 'GET', `/api/attachments/${attachmentId}/download`);
      wnegDownloadOk = add('control: seeder downloads W-neg attachment', '200/302', status, downloadOk(status));
    }
    const fixture = async (name, path, ws, okStatus = (st) => st === 200) => {
      const { status } = await request(R, 'GET', path, { ws });
      return add(`control: cheese-work ${name} exists`, okStatus === downloadOk ? '200/302' : '200', status, okStatus(status));
    };
    const realWs = await fixture('workspace', `/api/workspaces/${real.workspaceId}`);
    const realIssue = await fixture('issue', `/api/issues/${real.issueId}`, real.workspaceId);
    const realAtt = await fixture('attachment download', `/api/attachments/${real.attachmentId}/download`, undefined, downloadOk);

    // 1. Reads against W-neg by known ID.
    await expectDenied('W-neg workspace read', 'GET', `/api/workspaces/${wneg}`);
    await expectDenied('W-neg issue by id', 'GET', `/api/issues/${issueId}`, { ws: wneg });
    await expectDenied('W-neg issue by identifier', 'GET', `/api/issues/${identifier}`, { ws: wneg });
    await expectDenied('W-neg comment list', 'GET', `/api/issues/${issueId}/comments`, { ws: wneg });
    if (wnegDownloadOk) await expectDenied('W-neg attachment download', 'GET', `/api/attachments/${attachmentId}/download`);
    else add('W-neg attachment download', '401/403/404', 'NOT-RUN (no verified attachment download)', false);
    if (!stop) {
      if (wsControlOk) {
        const { outcome, detail } = await probe(wsBase + wneg, E);
        if (outcome === 'ack') stop = true;
        add('W-neg websocket subscribe', 'explicit membership rejection', `${outcome} (${detail})`, outcome === 'denied');
      } else add('W-neg websocket subscribe', 'explicit membership rejection', 'NOT-RUN (websocket control failed)', false);
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
        request(S, 'GET', `/api/issues/${issueId}`, { ws: wneg, data: true }),
        request(S, 'GET', `/api/issues/${issueId}/comments`, { ws: wneg, data: true }),
        request(S, 'GET', `/api/issues/${issueId}/attachments`, { ws: wneg, data: true }),
        request(S, 'GET', '/api/issues', { ws: wneg, data: true }),
      ]);
      const counts = `status=${one.data?.status} comments=${listOf(comments.data, 'comments').length} attachments=${listOf(atts.data, 'attachments').length} issues=${listOf(list.data, 'issues').length}`;
      add('W-neg readback unchanged', 'status=todo comments=1 attachments=1 issues=1', counts,
        counts === 'status=todo comments=1 attachments=1 issues=1');
    }

    // 3. E's own view and the real workspace (reads only).
    if (!stop) {
      const mine = await request(E, 'GET', '/api/workspaces', { data: true });
      const slugs = listOf(mine.data, 'workspaces').map((w) => w.slug);
      // Any workspace other than the sandbox is an unexpected success: stop before probing the real one.
      if (slugs.some((x) => x !== sandboxSlug)) stop = true;
      add('E workspace list is the sandbox only', `[${sandboxSlug}]`, `count=${slugs.length} match=${slugs.length === 1 && slugs[0] === sandboxSlug}`,
        slugs.length === 1 && slugs[0] === sandboxSlug);
    }
    const realDenied = async (exists, name, ...args) => (exists
      ? expectDenied(name, ...args)
      : !stop && add(name, '401/403/404', 'NOT-RUN (fixture unverified)', false));
    await realDenied(realWs, 'cheese-work workspace read', 'GET', `/api/workspaces/${real.workspaceId}`);
    await realDenied(realIssue, 'cheese-work issue by id', 'GET', `/api/issues/${real.issueId}`, { ws: real.workspaceId });
    await realDenied(realAtt, 'cheese-work attachment download', 'GET', `/api/attachments/${real.attachmentId}/download`);
    return { rows, stop };
  } finally {
    const del = await request(S, 'DELETE', `/api/workspaces/${wneg}`).catch(() => ({ status: 'error' }));
    const after = await request(S, 'GET', `/api/workspaces/${wneg}`).catch(() => ({ status: 'error' }));
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
  if (inAgentContext()) { console.error('refusing: running inside an agent task'); process.exit(2); }
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
    tokens: {
      seeder: readPat(keydir, 'seeder'),
      explorer: readPat(keydir, 'explorer'),
      ...(existsSync(join(keydir, 'reader.pat')) && { reader: readPat(keydir, 'reader') }),
    },
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

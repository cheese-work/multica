/**
 * Helpers for the D8a journey: an agent's chat task proposes a run-budget raise
 * in a DM, the signed-in owner approves it with a passkey. Everything the
 * product does not expose to a browser (the agent's task token, the passkey
 * row) is seeded straight into the database, as the other E2E specs do.
 */
import "./env";
import { createHash, generateKeyPairSync, randomBytes } from "node:crypto";
import type { BrowserContext, Page } from "@playwright/test";
import pg from "pg";
import type { TestApiClient } from "./fixtures";

export const API_BASE = process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;
const DATABASE_URL = process.env.DATABASE_URL ?? "";

/** The page origin. The API only accepts a passkey assertion signed for an exact https origin from MULTICA_WEBAUTHN_ORIGINS. */
export const HTTPS_ORIGIN = process.env.E2E_WEBAUTHN_ORIGIN ?? "https://localhost:23627";
/** Where the web app really listens. */
export const WEB_ORIGIN = process.env.E2E_WEB_ORIGIN ?? process.env.PLAYWRIGHT_BASE_URL ?? "http://localhost:3000";
export const RP_ID = new URL(HTTPS_ORIGIN).hostname;

export const BASE_CAP = 10;
export const RAISED_CAP = 25;

export async function withDb<T>(run: (db: pg.Client) => Promise<T>): Promise<T> {
  // The shared dev database is `multica`; a journey that writes agents, tokens and passkeys never runs there.
  if (!DATABASE_URL || new URL(DATABASE_URL).pathname === "/multica") {
    throw new Error("The DM budget journey needs an isolated database (make up), not the shared one");
  }
  const db = new pg.Client(DATABASE_URL);
  await db.connect();
  try {
    return await run(db);
  } finally {
    await db.end();
  }
}

const b64 = (data: Buffer) => data.toString("base64");

/** A passkey for `userId`: the public half goes to `user_passkey`, the private half to the virtual authenticator. */
export interface TestPasskey {
  credentialId: Buffer;
  privateKeyPkcs8: Buffer;
}

/** Credential ids this process inserted, so cleanup removes only those and never a user's pre-existing passkeys. */
const seededCredentials = new Map<string, Buffer[]>();

export async function seedPasskey(userId: string): Promise<TestPasskey> {
  const { privateKey, publicKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  const jwk = publicKey.export({ format: "jwk" });
  const x = Buffer.from(jwk.x!, "base64url");
  const y = Buffer.from(jwk.y!, "base64url");
  // COSE_Key {1: 2 (EC2), 3: -7 (ES256), -1: 1 (P-256), -2: x, -3: y}
  const cose = Buffer.concat([Buffer.from("a5010203262001215820", "hex"), x, Buffer.from("225820", "hex"), y]);
  const credentialId = randomBytes(32);
  await withDb((db) =>
    db.query(
      `INSERT INTO user_passkey (user_id, credential_id, public_key, attestation_type, transports, flags)
       VALUES ($1, $2, $3, 'none', '{internal}', 5)`,
      [userId, credentialId, cose],
    ),
  );
  seededCredentials.set(userId, [...(seededCredentials.get(userId) ?? []), credentialId]);
  return { credentialId, privateKeyPkcs8: privateKey.export({ format: "der", type: "pkcs8" }) };
}

/** Chromium's virtual authenticator holding `passkey`; it answers every prompt as a verified user. */
export async function addVirtualAuthenticator(page: Page, passkey: TestPasskey) {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: false,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  await cdp.send("WebAuthn.addCredential", {
    authenticatorId,
    credential: {
      credentialId: b64(passkey.credentialId),
      isResidentCredential: false,
      rpId: RP_ID,
      privateKey: b64(passkey.privateKeyPkcs8),
      signCount: 0,
    },
  });
}

/**
 * Serves the web app on the https origin the passkey is bound to. Each request
 * is fetched from the real web server and fulfilled, so no certificate is needed.
 * `onApiRequest` sees every API call first; it may take the route (return true).
 */
export async function serveOverHttps(
  context: BrowserContext,
  onApiRequest?: (route: import("@playwright/test").Route) => Promise<boolean>,
) {
  // The dev server's HMR socket is bridged to the real server: the app does not finish loading without it.
  await context.routeWebSocket(/\/_next\/.*hmr/, (page) => {
    const upstream = new WebSocket(page.url().replace(HTTPS_ORIGIN.replace("https:", "wss:"), WEB_ORIGIN.replace("http:", "ws:")));
    const backlog: (string | Buffer)[] = [];
    page.onMessage((message) => (upstream.readyState === WebSocket.OPEN ? upstream.send(message) : backlog.push(message)));
    upstream.onopen = () => backlog.splice(0).forEach((message) => upstream.send(message));
    upstream.binaryType = "arraybuffer";
    upstream.onmessage = (event) => page.send(typeof event.data === "string" ? event.data : Buffer.from(event.data));
    upstream.onclose = () => page.close();
    page.onClose(() => upstream.close());
  });
  // The API allows CORS only for the origins it was started with, not for the passkey origin. The
  // page's API calls go through here, and the answer is given the header the browser asks for.
  await context.route(`${API_BASE}/**`, async (route) => {
    if (await onApiRequest?.(route)) return;
    const request = route.request();
    const cors = {
      "access-control-allow-origin": HTTPS_ORIGIN,
      "access-control-allow-credentials": "true",
      "access-control-allow-headers": request.headers()["access-control-request-headers"] ?? "*",
      "access-control-allow-methods": "GET,POST,PUT,PATCH,DELETE,OPTIONS",
      vary: "Origin",
    };
    if (request.method() === "OPTIONS") return route.fulfill({ status: 204, headers: cors });
    const response = await route.fetch();
    const headers: Record<string, string> = { ...response.headers(), ...cors };
    delete headers["content-encoding"];
    delete headers["content-length"];
    await route.fulfill({ status: response.status(), headers, body: await response.body() });
  });
  await context.route(`${HTTPS_ORIGIN}/**`, async (route) => {
    const url = route.request().url().replace(HTTPS_ORIGIN, WEB_ORIGIN);
    const response = await route.fetch({ url });
    // fetch() already inflated the body; keeping content-encoding would make the browser inflate it again.
    const headers = { ...response.headers() };
    delete headers["content-encoding"];
    delete headers["content-length"];
    await route.fulfill({ status: response.status(), headers, body: await response.body() });
  });
}

/** GET as the signed-in E2E user. */
async function apiGet(api: TestApiClient, workspaceId: string, path: string) {
  const response = await fetch(`${API_BASE}${path}`, {
    headers: { Authorization: `Bearer ${api.getToken()}`, "X-Workspace-ID": workspaceId },
  });
  if (!response.ok) throw new Error(`GET ${path} failed: ${response.status} ${await response.text()}`);
  return response.json();
}

export interface DmBudgetSeed {
  workspaceId: string;
  workspaceSlug: string;
  userId: string;
  agentId: string;
  chatSessionId: string;
  chatTaskId: string;
  issueId: string;
  windowId: string;
  taskToken: string;
}

/** The owner's DM with an agent, a running chat task of that agent with its token, and a run-budgeted issue. */
export async function seedDmBudgetIssue(api: TestApiClient, label: string): Promise<DmBudgetSeed> {
  const workspace = (await api.getWorkspaces())[0];
  if (!workspace) throw new Error("E2E workspace missing");
  api.setWorkspaceId(workspace.id);
  api.setWorkspaceSlug(workspace.slug);
  const issue = await api.createIssue(`D8a budget raise ${label}`);
  const taskToken = `mat_${randomBytes(24).toString("hex")}`;
  const seed = await withDb(async (db) => {
    const one = async (sql: string, args: unknown[]) => (await db.query<{ id: string }>(sql, args)).rows[0]!.id;
    const userId = await one(`SELECT id::text FROM "user" WHERE email = $1`, [api.getEmail()]);
    const runtimeId = await one(
      `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at)
       VALUES ($1, $2, 'cloud', 'e2e_d8a', 'online', 'D8a E2E', '{}'::jsonb, now()) RETURNING id::text`,
      [workspace.id, `D8a runtime ${label}`],
    );
    const agentId = await one(
      `INSERT INTO agent (workspace_id, name, description, instructions, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
       VALUES ($1, $2, '', '', 'cloud', '{}'::jsonb, $3, 'workspace', 1, $4) RETURNING id::text`,
      [workspace.id, `Budget Agent ${label}`, runtimeId, userId],
    );
    const chatSessionId = await one(
      `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
       VALUES ($1, $2, $3, 'D8a budget DM', 'active', now()) RETURNING id::text`,
      [workspace.id, agentId, userId],
    );
    const chatTaskId = await one(
      `INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority)
       VALUES ($1, $2, $3, 'running', 0) RETURNING id::text`,
      [agentId, runtimeId, chatSessionId],
    );
    await db.query(
      `INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at)
       VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour')`,
      [createHash("sha256").update(taskToken).digest("hex"), chatTaskId, agentId, workspace.id, userId],
    );
    const workflowId = await one(
      `INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps, max_runs_per_issue, run_budget_since)
       VALUES ($1, $2, 'todo', '[]'::jsonb, $3, now() - interval '1 hour') RETURNING id::text`,
      [workspace.id, `D8a workflow ${label}`, BASE_CAP],
    );
    await db.query(`UPDATE issue SET workflow_id = $1 WHERE id = $2`, [workflowId, issue.id]);
    return { userId, agentId, chatSessionId, chatTaskId };
  });
  const snapshot = await apiGet(api, workspace.id, `/api/issues/${issue.id}/run-budget`);
  return {
    workspaceId: workspace.id,
    workspaceSlug: workspace.slug,
    ...seed,
    issueId: issue.id,
    windowId: snapshot.window_id as string,
    taskToken,
  };
}

/** The agent's `issue.run_budget.raise` proposal, made with its task token: it lands as a card in the DM. */
export async function proposeRaise(seed: DmBudgetSeed, newCap = RAISED_CAP): Promise<string> {
  const response = await fetch(`${API_BASE}/api/decision-actions/proposals`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${seed.taskToken}`,
      "X-Workspace-ID": seed.workspaceId,
      "X-Agent-ID": seed.agentId,
      "X-Task-ID": seed.chatTaskId,
    },
    body: JSON.stringify({
      action_type: "issue.run_budget.raise",
      version: 1,
      issue_id: seed.issueId,
      reason: "The issue ran out of runs and the work is not finished.",
      params: { issue_id: seed.issueId, new_cap: newCap, window_id: seed.windowId },
    }),
  });
  if (!response.ok) throw new Error(`proposal failed: ${response.status} ${await response.text()}`);
  return withDb(async (db) =>
    (await db.query<{ id: string }>(`SELECT id::text FROM decision_action WHERE requested_by_task_id = $1`, [seed.chatTaskId])).rows[0]!.id,
  );
}

/** Server state the journey asserts on, read from the API and the database, never from the card. */
export async function readCap(api: TestApiClient, seed: DmBudgetSeed) {
  const snapshot = await apiGet(api, seed.workspaceId, `/api/issues/${seed.issueId}/run-budget`);
  return { limit: snapshot.runs_limit as number, source: snapshot.runs_source as string };
}

export async function readOutcome(api: TestApiClient, seed: DmBudgetSeed, actionId: string) {
  return (await apiGet(api, seed.workspaceId, `/api/decision-actions/${actionId}/outcome`)) as {
    id: string;
    state: string;
    result?: { from?: number; to?: number; outcome?: string };
    error_code?: string;
  };
}

export async function countRaisedRows(issueId: string) {
  return withDb(async (db) => ({
    activity: Number((await db.query(`SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'run_budget_raised'`, [issueId])).rows[0].count),
    ledger: Number((await db.query(`SELECT count(*) FROM issue_run_budget_entry WHERE issue_id = $1 AND kind = 'raise'`, [issueId])).rows[0].count),
  }));
}

/** Test-created rows, removed in dependency order; the issue goes with `api.cleanup()`. */
export async function removeSeed(seed: DmBudgetSeed) {
  await withDb(async (db) => {
    await db.query(`DELETE FROM activity_log WHERE issue_id = $1`, [seed.issueId]);
    await db.query(`DELETE FROM issue_run_budget_entry WHERE issue_id = $1`, [seed.issueId]);
    await db.query(`DELETE FROM chat_message WHERE chat_session_id = $1`, [seed.chatSessionId]);
    await db.query(`DELETE FROM decision_action WHERE requested_by_task_id = $1`, [seed.chatTaskId]);
    await db.query(`DELETE FROM task_token WHERE task_id = $1`, [seed.chatTaskId]);
    await db.query(`DELETE FROM agent_task_queue WHERE id = $1`, [seed.chatTaskId]);
    await db.query(`DELETE FROM chat_session WHERE id = $1`, [seed.chatSessionId]);
    await db.query(`DELETE FROM user_passkey WHERE user_id = $1 AND credential_id = ANY($2::bytea[])`, [seed.userId, seededCredentials.get(seed.userId) ?? []]);
    seededCredentials.delete(seed.userId);
  });
}


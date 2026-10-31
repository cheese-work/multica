// Safety of the opt-in streaming issue-key fixture (e2e/streaming-issue-keys.spec.ts).
//
// The fixture writes to a database directly, so it must refuse to run unless it
// can prove the database belongs to a task-owned environment of this checkout,
// and it must leave nothing behind.
//
//   node --test e2e/streaming-issue-keys.safety.mjs
//
// The isolation cases run the real spec against fake PostgreSQL and API
// listeners and need no services. The cleanup cases need a running task-owned
// environment (`bash scripts/dev-env.sh up --ephemeral`) in this checkout and
// run only with STREAMING_ISSUE_KEYS_DISPOSABLE=1.
import { spawn } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { after, before, describe, test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const SPEC = "e2e/streaming-issue-keys.spec.ts";

function runSpec(env) {
  return new Promise((resolve) => {
    const child = spawn("pnpm", ["exec", "playwright", "test", SPEC, "--reporter=line", "--project=chromium"], {
      cwd: repoRoot,
      // A failure marker reaches the child only when a case passes one.
      env: { ...process.env, MULTICA_E2E_STREAMING_ISSUE_KEYS: "1", STREAMING_ISSUE_KEYS_FAIL_AT: "", ...env },
    });
    let output = "";
    child.stdout.on("data", (chunk) => (output += chunk));
    child.stderr.on("data", (chunk) => (output += chunk));
    child.on("close", (code) => resolve({ code, output }));
  });
}

function listen(server) {
  return new Promise((resolve) => server.listen(0, "127.0.0.1", () => resolve(server.address().port)));
}

describe("isolation: the fixture refuses before any write", () => {
  let pgPort;
  let otherPgPort;
  let apiPort;
  let pgConnections = 0;
  let apiRequests = 0;
  const pg = net.createServer((socket) => {
    pgConnections += 1;
    socket.destroy();
  });
  const otherPg = net.createServer((socket) => {
    pgConnections += 1;
    socket.destroy();
  });
  const api = http.createServer((req, res) => {
    apiRequests += 1;
    res.writeHead(500).end();
  });
  const devHome = fs.mkdtempSync(path.join(os.tmpdir(), "streaming-keys-dev-home-"));

  before(async () => {
    pgPort = await listen(pg);
    otherPgPort = await listen(otherPg);
    apiPort = await listen(api);
  });
  after(() => {
    pg.close();
    otherPg.close();
    api.close();
    fs.rmSync(devHome, { recursive: true, force: true });
  });

  function register({ name, owner = "agent", dir = repoRoot, db, backendPort, ttlHours = 24, expiresAt, dbPort = pgPort, dbHost = "127.0.0.1" }) {
    const expiry = expiresAt ?? (ttlHours > 0 ? new Date(Date.now() + ttlHours * 3_600_000).toISOString().replace(/\.\d+Z$/, "Z") : "");
    const envDir = path.join(devHome, "envs", name);
    fs.mkdirSync(path.join(envDir, "logs"), { recursive: true });
    fs.writeFileSync(
      path.join(envDir, "manifest.env"),
      [
        `NAME=${name}`,
        `DIR=${dir}`,
        "CREATED_AT=2026-01-01T00:00:00Z",
        `OWNER=${owner}`,
        `TTL_HOURS=${ttlHours}`,
        `EXPIRES_AT=${expiry}`,
        "ENV_FILE=.env.example",
        "OFFSET=901",
        `BACKEND_PORT=${backendPort}`,
        "FRONTEND_PORT=13901",
        `DB_NAME=${db}`,
        `DATABASE_URL=postgres://multica:multica@${dbHost}:${dbPort}/${db}?sslmode=disable`,
        "PROFILE=dev-streaming-keys-test",
        "WORKSPACES_ROOT=/nonexistent",
        "DESKTOP_RENDERER_PORT=5901",
        `DESKTOP_APP_SUFFIX=${name}`,
        "",
      ].join("\n"),
    );
    return () => fs.rmSync(envDir, { recursive: true, force: true });
  }

  async function expectRefusal(database, reason, apiUrl = `http://127.0.0.1:${apiPort}`) {
    pgConnections = 0;
    apiRequests = 0;
    const { code, output } = await runSpec({
      MULTICA_DEV_HOME: devHome,
      DATABASE_URL: database ?? "",
      NEXT_PUBLIC_API_URL: apiUrl,
      PORT: String(apiPort),
    });
    assert.equal(pgConnections, 0, `${reason}: the spec connected to PostgreSQL before refusing`);
    assert.equal(apiRequests, 0, `${reason}: the spec called the API before refusing`);
    assert.notEqual(code, 0, `${reason}: the spec must fail\n${output}`);
    assert.match(output, /task-owned/, `${reason}: the failure must name the ownership check\n${output}`);
  }

  test("no DATABASE_URL", async () => {
    await expectRefusal(undefined, "missing DATABASE_URL");
  });

  test("no registered environment for this checkout", async () => {
    await expectRefusal(`postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_901`, "unregistered");
  });

  test("environment registered for another checkout", async () => {
    const unregister = register({ name: "other-dir", dir: "/somewhere/else", db: "multica_route_902", backendPort: apiPort });
    try {
      await expectRefusal(`postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_902`, "other checkout");
    } finally {
      unregister();
    }
  });

  test("human-owned environment", async () => {
    const unregister = register({ name: "human-env", owner: "human", db: "multica_route_903", backendPort: apiPort });
    try {
      await expectRefusal(`postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_903`, "human-owned");
    } finally {
      unregister();
    }
  });

  test("database differs from the registered environment", async () => {
    const unregister = register({ name: "db-mismatch", db: "multica_route_904", backendPort: apiPort });
    try {
      await expectRefusal(`postgres://multica:multica@127.0.0.1:${pgPort}/multica`, "database mismatch");
    } finally {
      unregister();
    }
  });

  test("API port differs from the registered environment", async () => {
    const unregister = register({ name: "port-mismatch", db: "multica_route_905", backendPort: apiPort + 1 });
    try {
      await expectRefusal(`postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_905`, "API port mismatch");
    } finally {
      unregister();
    }
  });

  test("non-local database host", async () => {
    const unregister = register({ name: "remote-host", db: "multica_route_906", backendPort: apiPort });
    try {
      await expectRefusal("postgres://multica:multica@db.example.invalid:5432/multica_route_906", "remote host");
    } finally {
      unregister();
    }
  });

  test("database on another local port than the registered one", async () => {
    const unregister = register({ name: "db-endpoint", db: "multica_route_908", backendPort: apiPort });
    try {
      await expectRefusal(`postgres://multica:multica@127.0.0.1:${otherPgPort}/multica_route_908`, "database endpoint mismatch");
    } finally {
      unregister();
    }
  });

  test("registered database on another local host", async () => {
    const unregister = register({ name: "db-host", db: "multica_route_909", backendPort: apiPort, dbHost: "127.0.0.2" });
    try {
      await expectRefusal(`postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_909`, "database host mismatch");
    } finally {
      unregister();
    }
  });

  test("remote API origin at the registered port", async () => {
    const unregister = register({ name: "api-origin", db: "multica_route_910", backendPort: apiPort });
    try {
      await expectRefusal(
        `postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_910`,
        "remote API origin",
        `http://api.example.invalid:${apiPort}`,
      );
    } finally {
      unregister();
    }
  });

  test("agent-owned environment that is not ephemeral", async () => {
    const unregister = register({ name: "not-ephemeral", db: "multica_route_911", backendPort: apiPort, ttlHours: 0 });
    try {
      await expectRefusal(`postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_911`, "no TTL");
    } finally {
      unregister();
    }
  });

  test("ephemeral environment that has expired", async () => {
    const unregister = register({ name: "expired", db: "multica_route_912", backendPort: apiPort, expiresAt: "2026-01-02T00:00:00Z" });
    try {
      await expectRefusal(`postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_912`, "expired");
    } finally {
      unregister();
    }
  });

  // pg reads host, port and user from the query string too, and they win over
  // the URL authority; a URL without a port falls back to PGPORT. Each case
  // points the effective connection at the second fake PostgreSQL, so a guard
  // that only checks the authority shows up as traffic there.
  test("query port overrides the registered endpoint", async () => {
    const unregister = register({ name: "query-port", db: "multica_route_913", backendPort: apiPort });
    try {
      await expectRefusal(
        `postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_913?sslmode=disable&port=${otherPgPort}`,
        "query port override",
      );
    } finally {
      unregister();
    }
  });

  test("query host overrides the registered endpoint", async () => {
    const unregister = register({ name: "query-host", db: "multica_route_914", backendPort: apiPort });
    try {
      await expectRefusal(
        `postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_914?host=db.example.invalid`,
        "query host override",
      );
    } finally {
      unregister();
    }
  });

  test("query user overrides the registered user", async () => {
    const unregister = register({ name: "query-user", db: "multica_route_915", backendPort: apiPort });
    try {
      await expectRefusal(
        `postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_915?user=postgres`,
        "query user override",
      );
    } finally {
      unregister();
    }
  });

  test("URL without a port falls back to PGPORT", async () => {
    const unregister = register({ name: "no-port", db: "multica_route_916", backendPort: apiPort, dbPort: 5432 });
    const previous = process.env.PGPORT;
    process.env.PGPORT = String(otherPgPort);
    try {
      await expectRefusal("postgres://multica:multica@127.0.0.1/multica_route_916", "implicit port");
    } finally {
      if (previous === undefined) delete process.env.PGPORT;
      else process.env.PGPORT = previous;
      unregister();
    }
  });

  test("positive control: a matching task-owned environment gets past the check", async () => {
    const unregister = register({ name: "matching", db: "multica_route_907", backendPort: apiPort });
    try {
      pgConnections = 0;
      apiRequests = 0;
      const { output } = await runSpec({
        MULTICA_DEV_HOME: devHome,
        DATABASE_URL: `postgres://multica:multica@127.0.0.1:${pgPort}/multica_route_907`,
        NEXT_PUBLIC_API_URL: `http://127.0.0.1:${apiPort}`,
        PORT: String(apiPort),
      });
      assert.doesNotMatch(output, /task-owned/, output);
      assert.ok(pgConnections + apiRequests > 0, "the fakes saw no traffic, so the refusal cases prove nothing");
    } finally {
      unregister();
    }
  });
});

// Test selection, checked by listing only: no browser (an empty browser
// directory), no database URL and an unreachable API. The streaming phases must
// be selectable on their own at both viewports, without the issue-page capture,
// and the default run must still list every phase.
describe("selection: streaming phases list on their own, without services", () => {
  const noBrowsers = fs.mkdtempSync(path.join(os.tmpdir(), "streaming-keys-no-browsers-"));
  after(() => fs.rmSync(noBrowsers, { recursive: true, force: true }));

  function listTests(extraArgs) {
    return new Promise((resolve) => {
      const child = spawn("pnpm", ["exec", "playwright", "test", SPEC, "--list", "--project=chromium", ...extraArgs], {
        cwd: repoRoot,
        env: {
          ...process.env,
          MULTICA_E2E_STREAMING_ISSUE_KEYS: "1",
          DATABASE_URL: "",
          NEXT_PUBLIC_API_URL: "http://127.0.0.1:9",
          PLAYWRIGHT_BROWSERS_PATH: noBrowsers,
        },
      });
      let output = "";
      child.stdout.on("data", (chunk) => (output += chunk));
      child.stderr.on("data", (chunk) => (output += chunk));
      child.on("close", (code) => {
        const titles = output.split("\n").filter((line) => line.includes(" › ")).map((line) => line.trim());
        resolve({ code, output, titles });
      });
    });
  }

  test("@streaming selects only the boundary and settle phases, at both viewports", async () => {
    const { code, output, titles } = await listTests(["--grep", "@streaming"]);
    assert.equal(code, 0, output);
    assert.doesNotMatch(output, /task-owned/, "listing must not run the ownership check");
    assert.equal(titles.length, 4, output);
    for (const phase of ["streaming boundary", "streaming settle"]) {
      for (const viewport of ["desktop", "mobile"]) {
        assert.ok(
          titles.some((title) => title.includes(phase) && title.includes(`(${viewport})`)),
          `${phase} (${viewport}) is not selected\n${output}`,
        );
      }
    }
    assert.ok(titles.every((title) => !/issue page|description/i.test(title)), output);
  });

  test("the default run still lists every phase at both viewports", async () => {
    const { code, output, titles } = await listTests([]);
    assert.equal(code, 0, output);
    assert.equal(titles.length, 6, output);
    for (const viewport of ["desktop", "mobile"]) {
      for (const phase of ["streaming boundary", "streaming settle", "issue page"]) {
        assert.ok(
          titles.some((title) => title.includes(phase) && title.includes(`(${viewport})`)),
          `${phase} (${viewport}) is missing from the default run\n${output}`,
        );
      }
    }
  });
});

// Leftover rows of one fixture run, found through the run's own workspace.
async function leftovers(databaseUrl, runId) {
  const { default: pg } = await import("pg");
  const client = new pg.Client(databaseUrl);
  await client.connect();
  try {
    const slug = `e2e-workspace-0-${runId}`;
    const { rows } = await client.query(
      `
      WITH ws AS (SELECT id, issue_prefix FROM workspace WHERE slug = $1)
      SELECT
        (SELECT count(*) FROM ws)::int AS workspaces,
        (SELECT count(*) FROM ws WHERE issue_prefix = 'CHE')::int AS prefix_left_as_che,
        (SELECT count(*) FROM issue WHERE workspace_id IN (SELECT id FROM ws)
           AND (number IN (1, 18, 182, 1824) OR title = 'Streaming keys description host'))::int AS issues,
        (SELECT count(*) FROM comment WHERE workspace_id IN (SELECT id FROM ws))::int AS comments,
        (SELECT count(*) FROM agent_runtime WHERE workspace_id IN (SELECT id FROM ws))::int AS runtimes,
        (SELECT count(*) FROM agent WHERE workspace_id IN (SELECT id FROM ws))::int AS agents,
        (SELECT count(*) FROM chat_session WHERE workspace_id IN (SELECT id FROM ws))::int AS sessions,
        (SELECT count(*) FROM agent_task_queue t JOIN agent a ON a.id = t.agent_id
           WHERE a.workspace_id IN (SELECT id FROM ws))::int AS tasks
      `,
      [slug],
    );
    return rows[0];
  } finally {
    await client.end();
  }
}

const DISPOSABLE = process.env.STREAMING_ISSUE_KEYS_DISPOSABLE === "1";

describe("cleanup: the fixture leaves nothing behind", { skip: DISPOSABLE ? false : "set STREAMING_ISSUE_KEYS_DISPOSABLE=1 with a running task-owned environment" }, () => {
  function envFile() {
    const values = {};
    for (const line of fs.readFileSync(path.join(repoRoot, ".env.worktree"), "utf8").split("\n")) {
      const match = /^([A-Z0-9_]+)=(.*)$/.exec(line);
      if (match) values[match[1]] = match[2];
    }
    return values;
  }

  function assertClean(counts, context) {
    assert.equal(counts.workspaces, 1, `${context}: the run's workspace was not found`);
    for (const [key, value] of Object.entries(counts)) {
      if (key === "workspaces") continue;
      assert.equal(value, 0, `${context}: ${key} left behind: ${JSON.stringify(counts)}`);
    }
  }

  // The child sees an exported DATABASE_URL before .env.worktree (dotenv does
  // not override), so the leftover check must read the same database, and the
  // child gets it explicitly.
  const databaseUrl = () => process.env.DATABASE_URL || envFile().DATABASE_URL;

  test("after a passing run", async () => {
    const runId = `safety-pass-${Date.now().toString(36)}`;
    const { code, output } = await runSpec({ E2E_RUN_ID: runId, DATABASE_URL: databaseUrl() });
    assert.equal(code, 0, output);
    assertClean(await leftovers(databaseUrl(), runId), "passing run");
  });

  // A failure marker left in the caller's environment must not leak into a
  // run that did not ask for one.
  test("after a passing run with an ambient failure marker", async () => {
    const runId = `safety-ambient-${Date.now().toString(36)}`;
    const previous = process.env.STREAMING_ISSUE_KEYS_FAIL_AT;
    process.env.STREAMING_ISSUE_KEYS_FAIL_AT = "after-seed";
    try {
      const { code, output } = await runSpec({ E2E_RUN_ID: runId, DATABASE_URL: databaseUrl() });
      assert.doesNotMatch(output, /injected failure at/, output);
      assert.equal(code, 0, output);
    } finally {
      if (previous === undefined) delete process.env.STREAMING_ISSUE_KEYS_FAIL_AT;
      else process.env.STREAMING_ISSUE_KEYS_FAIL_AT = previous;
    }
    assertClean(await leftovers(databaseUrl(), runId), "passing run with an ambient marker");
  });

  // Each point fails the run on purpose at a different stage of setup. The
  // marker proves the intended failure ran, not some other error.
  for (const point of ["after-api-client", "after-db-connect", "mid-seed", "after-seed"]) {
    test(`after an injected failure at ${point}`, async () => {
      const runId = `safety-${point}-${Date.now().toString(36)}`;
      const { code, output } = await runSpec({
        E2E_RUN_ID: runId,
        DATABASE_URL: databaseUrl(),
        STREAMING_ISSUE_KEYS_FAIL_AT: point,
      });
      assert.notEqual(code, 0, `the injected failure did not fail the run\n${output}`);
      assert.match(output, new RegExp(`injected failure at ${point}`), `the failure at ${point} did not run\n${output}`);
      assert.doesNotMatch(output, /cleanup left rows behind/, output);
      const counts = await leftovers(databaseUrl(), runId);
      // A failure right after the API client exists happens before the
      // fixture touches the workspace; the helper's user and workspace are
      // removed with the task-owned database, not by the fixture.
      assertClean(counts, `failure at ${point}`);
    });
  }
});

// Tests for the C00 migration gate decision tool (CHE-9443 plan r8, step 1; CHE-9586).
// Test-first: every case named in the step 1 row of the plan has a test here.
import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  CLASSIFIER_VERSION,
  CHEESE_USER_ID,
  approvalLine,
  classifyPair,
  decide,
  evaluateApproval,
  inventoryOf,
  ledgerSha256,
  listVersions,
  loadTree,
} from "./migration-gate.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..");
const sha256 = (s) => createHash("sha256").update(s).digest("hex");

// ---- classifier helpers ----------------------------------------------------

const cls = (up, down, extra = {}) =>
  classifyPair({ version: "900_t", up, down: down === undefined ? null : down, goSources: [], ...extra });
const green = (up, down) => {
  const r = cls(up, down);
  assert.equal(r.class, "additive", `expected additive, got ${r.class} (${r.reason}) for: ${up}`);
};
const red = (up, down, reasonPrefix) => {
  const r = cls(up, down);
  assert.equal(r.class, "needs-approval", `expected needs-approval for: ${up}`);
  if (reasonPrefix) {
    assert.ok(r.reason.startsWith(reasonPrefix), `reason ${r.reason} should start with ${reasonPrefix} for: ${up}`);
  }
  return r;
};

// ---- green: each allowed up form -------------------------------------------

test("U1 SET LOCAL timeouts are additive", () => {
  green("SET LOCAL lock_timeout = '2s';");
  green("SET LOCAL statement_timeout TO '0'");
  green("set local lock_timeout = '2s'; set local statement_timeout = '30s';");
});

test("U2 CREATE INDEX forms are additive", () => {
  green("CREATE INDEX idx_a ON t (a);");
  green("CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON ONLY public.t USING btree (a);");
  green("CREATE INDEX idx_a ON t (a COLLATE \"C\" text_pattern_ops DESC NULLS LAST);");
  green("CREATE INDEX idx_a ON t (a) INCLUDE (b, c);");
  green("CREATE INDEX idx_a ON t USING gin (body gin_trgm_ops);");
});

test("U2 index with composite keys and a closed predicate is additive", () => {
  green("CREATE INDEX idx ON t (a, b DESC) WHERE deleted_at IS NULL AND kind IN ('x','y');");
  green("CREATE INDEX idx ON t (a) WHERE (a IS NOT NULL OR b = 'x') AND NOT c;");
  green("CREATE INDEX idx ON t (a) WHERE n >= 3 AND s <> 'q' AND k NOT IN ('a','b') AND f IS TRUE AND g IS NOT FALSE;");
});

test("U3 ADD COLUMN forms are additive", () => {
  green("ALTER TABLE t ADD COLUMN a int;");
  green("ALTER TABLE IF EXISTS ONLY public.t ADD COLUMN IF NOT EXISTS a text NULL;");
  green("ALTER TABLE t ADD a bigint NOT NULL DEFAULT 0;");
  green("ALTER TABLE t ADD COLUMN a jsonb NOT NULL DEFAULT '{}'::jsonb;");
  green("ALTER TABLE t ADD COLUMN a double precision DEFAULT 1;");
  green("ALTER TABLE t ADD COLUMN a character varying(255) DEFAULT 'x';");
  green("ALTER TABLE t ADD COLUMN a text[] DEFAULT NULL;");
});

test("U4 VALIDATE CONSTRAINT is additive", () => {
  green("ALTER TABLE t VALIDATE CONSTRAINT c1;");
  green("ALTER TABLE IF EXISTS ONLY s.t VALIDATE CONSTRAINT \"c-1\";");
});

test("U5 CREATE TABLE forms are additive", () => {
  green("CREATE TABLE t (id uuid PRIMARY KEY, name text NOT NULL);");
  green("CREATE TABLE IF NOT EXISTS s.t (id int NOT NULL DEFAULT 0, at timestamptz DEFAULT now(), d date DEFAULT CURRENT_DATE, ts timestamp DEFAULT CURRENT_TIMESTAMP);");
});

test("U5 table with gen_random_uuid() default, FK ON DELETE CASCADE and UNIQUE (w, id) is additive", () => {
  green(`CREATE TABLE IF NOT EXISTS child (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    w uuid NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    parent_id uuid,
    CONSTRAINT child_pk PRIMARY KEY (id),
    UNIQUE (w, id),
    FOREIGN KEY (parent_id) REFERENCES parent (id) ON DELETE SET NULL ON UPDATE NO ACTION
  );`);
  green("CREATE TABLE t (a numeric(10, 2) DEFAULT 1, b varchar(20) CONSTRAINT b_u UNIQUE);");
});

test("U6 COMMENT ON is additive", () => {
  green("COMMENT ON TABLE t IS 'note';");
  green("COMMENT ON COLUMN t.a IS NULL;");
  green("COMMENT ON INDEX idx IS 'it''s';");
  green("COMMENT ON SEQUENCE s IS 'x';");
});

test("U7 CREATE SEQUENCE is additive", () => {
  green("CREATE SEQUENCE s;");
  green("CREATE SEQUENCE IF NOT EXISTS public.s INCREMENT BY 1 MINVALUE 1 NO MAXVALUE START WITH 5 CACHE 10 NO CYCLE;");
});

test("a multi-statement file of allowed forms, comments and blank statements is additive", () => {
  green(`-- header
    SET LOCAL lock_timeout = '2s'; /* outer /* nested */ still comment */
    ;; CREATE INDEX idx ON t (a);
    COMMENT ON INDEX idx IS 'x'; -- trailing`);
});

// ---- red: unsupported balanced expressions ----------------------------------

test("index body: parenthesised element or function call needs approval", () => {
  red("CREATE INDEX idx ON t (((f(a))));", undefined, "up:index-expression");
  red("CREATE INDEX idx ON t (((f(a))), b);", undefined, "up:index-expression");
  red("CREATE INDEX idx ON t ((f(a)));", undefined, "up:index-expression");
  red("CREATE INDEX idx ON t ((lower(a)));", undefined, "up:index-expression");
  red("CREATE INDEX idx ON t (lower(a));", undefined, "up:index-expression");
  red("CREATE INDEX idx ON t (a, (b + 1));", undefined, "up:index-expression");
});

test("index predicate: function, column comparison and subquery need approval", () => {
  red("CREATE INDEX idx ON t (a) WHERE f(a) > 0;", undefined, "up:index-predicate");
  red("CREATE INDEX idx ON t (a) WHERE a > b;", undefined, "up:index-predicate");
  red("CREATE INDEX idx ON t (a) WHERE a IN (SELECT 1);", undefined, "up:index-predicate");
  red("CREATE INDEX idx ON t (a) WHERE (f(a)) > 0;", undefined, "up:index-predicate");
  red("CREATE INDEX idx ON t (a) WHERE a IS DISTINCT FROM 1;", undefined, "up:index-predicate");
  red("CREATE INDEX idx ON t (a) WHERE a > 0 AND;", undefined, "up:index-predicate");
});

test("index tail: storage parameters and tablespace need approval", () => {
  red("CREATE INDEX idx ON t (a) WITH (fillfactor = 70);");
  red("CREATE INDEX idx ON t (a) TABLESPACE x;");
});

test("table body: CHECK, DEFAULT f(1) and GENERATED need approval", () => {
  red("CREATE TABLE t (a text CHECK (a <> ''));", undefined, "up:table-body:");
  red("CREATE TABLE t (a text, CHECK (a <> ''));", undefined, "up:table-body:");
  red("CREATE TABLE t (a int DEFAULT f(1));", undefined, "up:table-body:");
  red("CREATE TABLE t (id int, b int GENERATED ALWAYS AS (id * 2) STORED);", undefined, "up:table-body:");
  red("CREATE TABLE t (a int, EXCLUDE USING gist (a WITH =));", undefined, "up:table-body:");
  red("CREATE TABLE t (LIKE other);", undefined, "up:table-body:");
  red("CREATE TABLE t (a int DEFAULT nextval('s'));", undefined, "up:table-body:");
  red("CREATE TABLE t (a int DEFAULT (1 + 1));", undefined, "up:table-body:");
});

test("type modifier with an expression needs approval", () => {
  red("ALTER TABLE t ADD COLUMN a varchar((select 1));");
  red("CREATE TABLE t (a varchar((select 1)));");
  red("CREATE TABLE t (a varchar(1, 2, 3));");
});

// ---- red: the r4 set --------------------------------------------------------

test("r4 set: statements outside the closed forms need approval", () => {
  red("ALTER TABLE t ADD COLUMN a int, DROP COLUMN b;");
  red("ALTER TABLE t ADD COLUMN a int, ADD COLUMN b int;");
  red("ALTER TABLE t ADD COLUMN a int NOT NULL;");
  red("ALTER TABLE t ADD COLUMN a timestamptz DEFAULT now();");
  red("ALTER TABLE t ADD COLUMN a uuid REFERENCES o(id);");
  red("ALTER TABLE t ADD COLUMN a serial;");
  red("ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0);");
  red("ALTER TABLE t DROP COLUMN a;");
  red("CREATE TABLE x AS SELECT 1;");
  red("CREATE TABLE x (a int) PARTITION BY RANGE (a);");
  red("CREATE TABLE x (a int) INHERITS (y);");
  red("CREATE INDEX idx ON t (a); DROP TABLE t;");
  red("COMMENT ON TABLE t IS 'x'; DELETE FROM t;");
  red("UPDATE t SET a = 1;");
  red("INSERT INTO t VALUES (1);");
  red("DROP INDEX idx;");
  red("TRUNCATE t;");
  red("BEGIN; CREATE INDEX idx ON t (a); COMMIT;");
  red("SET lock_timeout = '2s';");
  red("SET LOCAL search_path = x;");
});

test("r4 set: opaque files need approval", () => {
  red("COMMENT ON TABLE t IS E'x';", undefined, "up:opaque");
  red("COMMENT ON TABLE t IS e'x';", undefined, "up:opaque");
  red("DO $$ BEGIN PERFORM 1; END $$;", undefined, "up:opaque");
  red("CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1 $body$ LANGUAGE sql;", undefined, "up:opaque");
  red("CREATE INDEX idx ON t (a;", undefined, "up:opaque");
  red("CREATE INDEX idx ON t (a));", undefined, "up:opaque");
  red("COMMENT ON TABLE t IS 'unterminated;", undefined, "up:opaque");
  red("CREATE INDEX \"unterminated ON t (a);", undefined, "up:opaque");
  red("CREATE INDEX idx ON t (a); /* never closed", undefined, "up:opaque");
  red("CREATE INDEX idx ON t (a) \\gexec", undefined, "up:opaque");
});

test("r4 set: a nested comment cannot hide a statement", () => {
  // The outer comment ends at the final "*/". A parser that ends it at the first "*/" would
  // resume inside the comment and see DROP TABLE as live SQL (or the reverse, hide it).
  red("CREATE INDEX idx ON t (a); /* a /* b */ c */ DROP TABLE t;", undefined, "up:unsupported");
  // Live statement after what a flat parser would call the end of the comment.
  red("/* a /* b */ DROP TABLE t; */ CREATE INDEX idx ON t (a); /* c */ DROP TABLE t;", undefined, "up:unsupported");
  // A DROP inside a nested comment stays a comment: additive.
  green("CREATE INDEX idx ON t (a); /* x /* DROP TABLE t; */ DROP TABLE t; */");
  // A line comment cannot swallow the next line's statement and a string cannot hide a comment opener.
  red("CREATE INDEX idx ON t (a); -- harmless\nDROP TABLE t;", undefined, "up:unsupported");
  green("COMMENT ON TABLE t IS '/* not a comment'; CREATE INDEX idx ON t (a);");
  green("COMMENT ON TABLE t IS '-- not a comment'; CREATE INDEX idx ON t (a);");
});

test("keyword case and quoted identifiers do not change the class", () => {
  green("create unique index concurrently if not exists \"Idx-1\" on \"T\" (\"A\" desc);");
  red("Alter Table t Drop Column a;");
});

// ---- pair, hook ----------------------------------------------------------------

test("a down that drops a column loses data: 006_workspace_context", () => {
  const up = readFileSync(join(repoRoot, "server/migrations/006_workspace_context.up.sql"), "utf8");
  const down = readFileSync(join(repoRoot, "server/migrations/006_workspace_context.down.sql"), "utf8");
  const r = classifyPair({ version: "006_workspace_context", up, down, goSources: [] });
  assert.equal(r.class, "needs-approval");
  assert.equal(r.reason, "data-lost-on-down");
});

test("down forms: absent, SET LOCAL, and DROP INDEX of an index the up created are additive", () => {
  const up = "CREATE INDEX idx_a ON t (a);";
  green(up, undefined);
  green(up, "DROP INDEX idx_a;");
  green(up, "SET LOCAL lock_timeout = '2s'; DROP INDEX CONCURRENTLY IF EXISTS public.idx_a RESTRICT;");
  green(up, "");
  green(up, "-- nothing to undo");
  const r = classifyPair({ version: "900_t", up, down: "DROP INDEX idx_a;", goSources: [] });
  assert.equal(r.downHash, sha256("DROP INDEX idx_a;"));
});

test("down-only change from DROP INDEX to DROP COLUMN flips admit to refuse", () => {
  const up = "CREATE INDEX idx_a ON t (a);";
  const withIndexDown = { up, down: "DROP INDEX idx_a;" };
  const withColumnDown = { up, down: "ALTER TABLE t DROP COLUMN a;" };
  const mk = (pair) => classifyPair({ version: "900_t", goSources: [], ...pair });
  assert.equal(mk(withIndexDown).class, "additive");
  const flipped = mk(withColumnDown);
  assert.equal(flipped.class, "needs-approval");
  assert.equal(flipped.reason, "data-lost-on-down");

  // And through decide(): same tree, only the down file differs.
  const decideWith = (down) => {
    const dir = mkTree({ migrations: { "900_t.up.sql": up, "900_t.down.sql": down } });
    try {
      return decide(baseInput(dir, { ledger: [] }));
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  };
  assert.equal(decideWith("DROP INDEX idx_a;").decision, "admit");
  const refused = decideWith("ALTER TABLE t DROP COLUMN a;");
  assert.equal(refused.decision, "refuse");
  assert.equal(refused.reason, "approval-missing");
});

test("down that drops an index the up did not create, or two indexes, loses data", () => {
  const up = "CREATE INDEX idx_a ON t (a);";
  for (const down of ["DROP INDEX other;", "DROP INDEX idx_a, idx_b;", "DROP TABLE t;", "DELETE FROM t;"]) {
    const r = cls(up, down);
    assert.equal(r.class, "needs-approval", down);
    assert.equal(r.reason, "data-lost-on-down", down);
  }
});

test("an opaque down is opaque-down", () => {
  const up = "CREATE INDEX idx_a ON t (a);";
  for (const down of ["DO $$ BEGIN END $$;", "DROP INDEX idx_a; /* open", "COMMENT ON TABLE t IS E'x';"]) {
    const r = cls(up, down);
    assert.equal(r.class, "needs-approval", down);
    assert.equal(r.reason, "opaque-down", down);
  }
});

test("a version string in a non-test migrator Go source is a hook", () => {
  const up = "CREATE INDEX idx_a ON t (a);";
  const r = classifyPair({
    version: "900_t",
    up,
    down: null,
    goSources: [{ path: "server/cmd/migrate/main.go", content: 'hooks["900_t"] = runHook' }],
  });
  assert.equal(r.class, "needs-approval");
  assert.equal(r.reason, "hook");
  const none = classifyPair({
    version: "900_t",
    up,
    down: null,
    goSources: [{ path: "server/cmd/migrate/main.go", content: 'hooks["901_t"] = runHook' }],
  });
  assert.equal(none.class, "additive");
});

test("the first failing reason wins: up grammar, then down, then hook", () => {
  const src = [{ path: "a.go", content: "900_t" }];
  assert.equal(
    classifyPair({ version: "900_t", up: "DROP TABLE t;", down: "DROP TABLE t;", goSources: src }).reason,
    "up:unsupported-statement",
  );
  assert.equal(
    classifyPair({ version: "900_t", up: "CREATE INDEX i ON t (a);", down: "DROP TABLE t;", goSources: src }).reason,
    "data-lost-on-down",
  );
});

test("the class carries the SHA-256 of the exact up and down bytes", () => {
  const up = "CREATE INDEX idx_a ON t (a);\n";
  const r = classifyPair({ version: "900_t", up, down: null, goSources: [] });
  assert.equal(r.upHash, sha256(up));
  assert.equal(r.downHash, "absent");
});

// ---- tree, ledger, pending set, inventory ---------------------------------------

const FIXED_SRC = "package main\n";

function mkTree({ migrations = {}, withCapability = true, goMigrate = FIXED_SRC, goInternal = FIXED_SRC, extraGo = {} } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "mg-test-"));
  mkdirSync(join(dir, "server/migrations"), { recursive: true });
  mkdirSync(join(dir, "server/cmd/migrate"), { recursive: true });
  mkdirSync(join(dir, "server/internal/migrations"), { recursive: true });
  mkdirSync(join(dir, "deploy/cd"), { recursive: true });
  for (const [name, body] of Object.entries(migrations)) writeFileSync(join(dir, "server/migrations", name), body);
  if (goMigrate !== null) writeFileSync(join(dir, "server/cmd/migrate/main.go"), goMigrate);
  if (goInternal !== null) writeFileSync(join(dir, "server/internal/migrations/migrations.go"), goInternal);
  for (const [path, body] of Object.entries(extraGo)) writeFileSync(join(dir, path), body);
  if (withCapability) writeFileSync(join(dir, "deploy/cd/migration-gate.capability"), "migration-decision v1\n");
  return dir;
}

const SRC_SHA = "a".repeat(40);
const TREE_SHA = "b".repeat(40);

function ledgerBytes(versions) {
  return Buffer.from(versions.map((v) => `${v}\n`).join(""));
}

function baseInput(treeRoot, { ledger = [], comments, treeSha = TREE_SHA } = {}) {
  const bytes = ledgerBytes(ledger);
  return {
    treeRoot,
    ledgerBytes: bytes,
    expectedLedgerSha256: sha256(bytes),
    treeSha,
    sourceSha: SRC_SHA,
    checkoutSha: SRC_SHA,
    comments,
  };
}

test("listVersions mirrors Go: *.up.sql basenames in byte order of the file name", () => {
  const dir = mkTree({
    migrations: {
      "002_b.up.sql": "", "002_b.down.sql": "", "001_a.up.sql": "", "99h_x.up.sql": "", "1000_y.up.sql": "",
      "003_only_down.down.sql": "", "notes.txt": "", "004_c.up.sql.bak": "",
      "a.up.sql": "", "a_b.up.sql": "",
    },
  });
  try {
    assert.deepEqual(listVersions(join(dir, "server/migrations")), [
      "001_a", "002_b", "1000_y", "99h_x", "a", "a_b",
    ]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("pending set: an exact ledger row matches; row 627_x does not cover file 653_x", () => {
  const dir = mkTree({
    migrations: {
      "627_x.up.sql": "CREATE INDEX i1 ON t (a);",
      "653_x.up.sql": "CREATE INDEX i2 ON t (a);",
    },
  });
  try {
    const d = decide(baseInput(dir, { ledger: ["627_x"] }));
    assert.equal(d.decision, "admit");
    assert.deepEqual(d.pending.map((p) => p.version), ["653_x"]);
    const all = decide(baseInput(dir, { ledger: [] }));
    assert.deepEqual(all.pending.map((p) => p.version), ["627_x", "653_x"]);
    // a ledger row with no file never becomes pending and a prefix is not a match
    const extra = decide(baseInput(dir, { ledger: ["627_x", "653_x", "654_gone", "65"] }));
    assert.deepEqual(extra.pending, []);
    const prefix = decide(baseInput(dir, { ledger: ["627", "653"] }));
    assert.deepEqual(prefix.pending.map((p) => p.version), ["627_x", "653_x"]);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("no pending version admits without reading approvals", () => {
  const dir = mkTree({ migrations: { "1_a.up.sql": "DROP TABLE t;" } });
  try {
    const d = decide(baseInput(dir, { ledger: ["1_a"], comments: { error: "boom" } }));
    assert.equal(d.decision, "admit");
    assert.deepEqual(d.pending, []);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("every pending version additive admits; one needs-approval refuses and prints the line", () => {
  const dir = mkTree({
    migrations: {
      "1_a.up.sql": "CREATE INDEX i ON t (a);",
      "2_b.up.sql": "ALTER TABLE t DROP COLUMN a;",
    },
  });
  try {
    const only = decide(baseInput(dir, { ledger: ["2_b"] }));
    assert.equal(only.decision, "admit");
    const d = decide(baseInput(dir, { ledger: [] }));
    assert.equal(d.decision, "refuse");
    assert.equal(d.reason, "approval-missing");
    assert.match(d.inventory, /^sha256:[0-9a-f]{64}$/);
    assert.equal(d.approvalLine, `c00-migration-approval v1 inventory=${d.inventory} tree=${TREE_SHA}`);
    assert.equal(d.pending.find((p) => p.version === "2_b").class, "needs-approval");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("inventory hash covers sorted lines, the migrator-source hash and the classifier version", () => {
  const up1 = "CREATE INDEX i ON t (a);";
  const up2 = "ALTER TABLE t DROP COLUMN a;";
  const dir = mkTree({
    migrations: { "2_b.up.sql": up2, "1_a.up.sql": up1, "1_a.down.sql": "DROP INDEX i;" },
    goMigrate: "package main\n// migrate\n",
    goInternal: "package migrations\n",
    extraGo: { "server/cmd/migrate/main_test.go": "package main // 1_a 2_b ignored: test file\n" },
  });
  try {
    const tree = loadTree(dir);
    const goHash = sha256(
      ["server/cmd/migrate/main.go", "package main\n// migrate\n", "server/internal/migrations/migrations.go", "package migrations\n"]
        .map((x) => `${x}\0`).join(""),
    );
    assert.equal(tree.migratorSourceSha256, goHash);
    const lines = [
      `1_a\t${sha256(up1)}\t${sha256("DROP INDEX i;")}\tadditive\tnone`,
      `2_b\t${sha256(up2)}\tabsent\tneeds-approval\tup:unsupported-statement`,
    ];
    const expected = sha256(`${lines.join("\n")}\n` + `migrator-source\t${goHash}\n` + `classifier\t${CLASSIFIER_VERSION}\n`);
    const d = decide(baseInput(dir, { ledger: [] }));
    assert.equal(d.inventory, `sha256:${expected}`);
    assert.equal(d.migratorSourceSha256, goHash);
    assert.equal(d.classifierVersion, CLASSIFIER_VERSION);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("inventory changes when an up byte, a down byte, the migrator source or the pending set changes", () => {
  const mk = (migs, go) => {
    const dir = mkTree({ migrations: migs, goMigrate: go });
    try {
      return decide(baseInput(dir, { ledger: [] })).inventory;
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  };
  const base = { "1_a.up.sql": "CREATE INDEX i ON t (a);" };
  const ref = mk(base, "package main\n");
  assert.notEqual(mk({ "1_a.up.sql": "CREATE INDEX i ON t (a); " }, "package main\n"), ref);
  assert.notEqual(mk({ ...base, "1_a.down.sql": "DROP INDEX i;" }, "package main\n"), ref);
  assert.notEqual(mk(base, "package main // x\n"), ref);
  assert.notEqual(mk({ ...base, "2_b.up.sql": "CREATE INDEX j ON t (b);" }, "package main\n"), ref);
  assert.equal(mk(base, "package main\n"), ref);
});

test("the migrator-source hash ignores test files and covers nested non-test Go files", () => {
  const a = mkTree({ extraGo: { "server/cmd/migrate/x_test.go": "one" } });
  const b = mkTree({ extraGo: { "server/cmd/migrate/x_test.go": "two" } });
  const c = mkTree({ extraGo: { "server/cmd/migrate/y.go": "package main" } });
  try {
    assert.equal(loadTree(a).migratorSourceSha256, loadTree(b).migratorSourceSha256);
    assert.notEqual(loadTree(a).migratorSourceSha256, loadTree(c).migratorSourceSha256);
  } finally {
    for (const d of [a, b, c]) rmSync(d, { recursive: true, force: true });
  }
});

test("hook: a pending version named in the migrator Go source needs approval", () => {
  const dir = mkTree({
    migrations: { "103_x.up.sql": "CREATE INDEX i ON t (a);" },
    goMigrate: 'package main\nvar h = map[string]int{"103_x": 1}\n',
  });
  try {
    const d = decide(baseInput(dir, { ledger: [] }));
    assert.equal(d.decision, "refuse");
    assert.equal(d.pending[0].reason, "hook");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("a version named only in a test Go file is not a hook", () => {
  const dir = mkTree({
    migrations: { "103_x.up.sql": "CREATE INDEX i ON t (a);" },
    extraGo: { "server/cmd/migrate/main_test.go": 'package main\nvar _ = "103_x"\n' },
  });
  try {
    assert.equal(decide(baseInput(dir, { ledger: [] })).decision, "admit");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("the real migrator hook versions at the pin are hooks", () => {
  const sources = loadTree(repoRoot).goSources;
  for (const version of ["103_drop_legacy_daily_rollups", "198_agent_task_attribution_strict_constraint_validate"]) {
    const r = classifyPair({ version, up: "CREATE INDEX i ON t (a);", down: null, goSources: sources });
    assert.equal(r.reason, "hook", version);
  }
  const clean = classifyPair({ version: "006_workspace_context", up: "CREATE INDEX i ON t (a);", down: null, goSources: sources });
  assert.equal(clean.class, "additive");
});

test("ledger refusals: digest mismatch, unreadable, tree unreadable, migrator source missing", () => {
  const dir = mkTree({ migrations: { "1_a.up.sql": "CREATE INDEX i ON t (a);" } });
  try {
    const ok = baseInput(dir);
    assert.equal(decide(ok).decision, "admit");

    const mismatch = decide({ ...ok, expectedLedgerSha256: sha256("other") });
    assert.deepEqual([mismatch.decision, mismatch.reason], ["refuse", "ledger-digest-mismatch"]);
    const noDigest = decide({ ...ok, expectedLedgerSha256: undefined });
    assert.deepEqual([noDigest.decision, noDigest.reason], ["refuse", "ledger-digest-mismatch"]);
    const edited = decide({ ...ok, ledgerBytes: ledgerBytes(["0_extra"]) });
    assert.deepEqual([edited.decision, edited.reason], ["refuse", "ledger-digest-mismatch"]);

    const unreadable = decide({ ...ok, ledgerBytes: null });
    assert.deepEqual([unreadable.decision, unreadable.reason], ["refuse", "ledger-unreadable"]);
    const garbled = decide({ ...ok, ledgerBytes: Buffer.from("1_a\n\0bad\n"), expectedLedgerSha256: sha256(Buffer.from("1_a\n\0bad\n")) });
    assert.deepEqual([garbled.decision, garbled.reason], ["refuse", "ledger-unreadable"]);

    const noTree = decide({ ...ok, treeRoot: join(dir, "does-not-exist") });
    assert.deepEqual([noTree.decision, noTree.reason], ["refuse", "tree-unreadable"]);
    for (const d of [noTree, mismatch, unreadable]) assert.equal(d.approvalLine ?? null, null);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
  const noSrc = mkTree({ migrations: { "1_a.up.sql": "CREATE INDEX i ON t (a);" }, goMigrate: null, goInternal: null });
  try {
    const d = decide(baseInput(noSrc));
    assert.deepEqual([d.decision, d.reason], ["refuse", "migrator-source-missing"]);
  } finally {
    rmSync(noSrc, { recursive: true, force: true });
  }
});

test("ledgerSha256 is the SHA-256 of the psql output bytes", () => {
  const bytes = ledgerBytes(["1_a", "2_b"]);
  assert.equal(ledgerSha256(bytes), sha256("1_a\n2_b\n"));
});

test("a parser crash refuses with gate-error and no approval line", () => {
  const d = decide({ treeRoot: null, ledgerBytes: Buffer.alloc(0), expectedLedgerSha256: sha256(""), sourceSha: SRC_SHA, checkoutSha: SRC_SHA });
  assert.equal(d.decision, "refuse");
  assert.ok(["gate-error", "tree-unreadable"].includes(d.reason));
  const weird = decide(undefined);
  assert.deepEqual([weird.decision, weird.reason], ["refuse", "gate-error"]);
});

// ---- decision table rows before the ledger -----------------------------------------

test("checkout and capability refusals come first and print no approval line", () => {
  const dir = mkTree({ migrations: { "1_a.up.sql": "DROP TABLE t;" } });
  const noCap = mkTree({ migrations: { "1_a.up.sql": "CREATE INDEX i ON t (a);" }, withCapability: false });
  try {
    const input = baseInput(dir);
    for (const bad of [{ checkoutSha: "c".repeat(40) }, { sourceSha: "" }, { sourceSha: undefined }, { checkoutSha: "" }]) {
      const d = decide({ ...input, ...bad });
      assert.deepEqual([d.decision, d.reason], ["refuse", "candidate-checkout-mismatch"], JSON.stringify(bad));
      assert.equal(d.approvalLine ?? null, null);
    }
    const old = decide(baseInput(noCap));
    assert.deepEqual([old.decision, old.reason], ["refuse", "controller-predates-gate"]);
    assert.equal(old.approvalLine ?? null, null);
    writeFileSync(join(noCap, "deploy/cd/migration-gate.capability"), "migration-decision v2\n");
    assert.equal(decide(baseInput(noCap)).reason, "controller-predates-gate");
    writeFileSync(join(noCap, "deploy/cd/migration-gate.capability"), "migration-decision v1");
    assert.equal(decide(baseInput(noCap)).decision, "admit");
    writeFileSync(join(noCap, "deploy/cd/migration-gate.capability"), "migration-decision v1\nextra\n");
    assert.equal(decide(baseInput(noCap)).reason, "controller-predates-gate");
  } finally {
    for (const d of [dir, noCap]) rmSync(d, { recursive: true, force: true });
  }
});

// ---- approvals ------------------------------------------------------------------

const INV = `sha256:${"1".repeat(64)}`;
const LINE = `c00-migration-approval v1 inventory=${INV} tree=${TREE_SHA}`;
const comment = (over = {}) => ({
  id: 1,
  user: { id: CHEESE_USER_ID },
  body: LINE,
  created_at: "2026-11-24T10:00:00Z",
  updated_at: "2026-11-24T10:00:00Z",
  ...over,
});
const evalApproval = (pages, opts = {}) =>
  evaluateApproval({ pages, ...opts }, { inventory: INV, tree: TREE_SHA });

test("Cheese's numeric user ID is 129714106", () => {
  assert.equal(CHEESE_USER_ID, 129714106);
});

test("approval: an exact unedited line by Cheese matches; a trailing newline is allowed", () => {
  assert.deepEqual(evalApproval([[comment()]]), { ok: true, commentId: 1, line: LINE });
  assert.equal(evalApproval([[comment({ body: `${LINE}\n` })]]).ok, true);
  assert.equal(evalApproval([[comment({ body: `${LINE}\r\n` })]]).ok, true);
});

test("approval: none, other inventory, other tree", () => {
  assert.deepEqual(evalApproval([[]]), { ok: false, reason: "approval-missing" });
  assert.deepEqual(evalApproval([]), { ok: false, reason: "approval-missing" });
  assert.equal(
    evalApproval([[comment({ body: LINE.replace("1".repeat(64), "2".repeat(64)) })]]).reason,
    "approval-missing",
  );
  assert.equal(evalApproval([[comment({ body: LINE.replace("b".repeat(40), "c".repeat(40)) })]]).reason, "approval-missing");
});

test("approval: wrong author, including the agent bot 282619718, never matches", () => {
  for (const id of [282619718, 1, 0, "129714106", null, undefined]) {
    assert.equal(evalApproval([[comment({ user: { id } })]]).ok, false, String(id));
  }
  assert.equal(evalApproval([[comment({ user: null })]]).ok, false);
  assert.equal(evalApproval([[{ ...comment(), user: undefined, author: { id: CHEESE_USER_ID } }]]).ok, false);
  // a login is not an identity: only the numeric ID counts
  assert.equal(evalApproval([[comment({ user: { id: 282619718, login: "congvc-dev" } })]]).ok, false);
});

test("approval: an edited comment never matches", () => {
  assert.equal(evalApproval([[comment({ updated_at: "2026-11-24T10:05:00Z" })]]).ok, false);
  assert.equal(evalApproval([[comment({ updated_at: undefined })]]).ok, false);
  assert.equal(evalApproval([[comment({ created_at: undefined, updated_at: undefined })]]).ok, false);
});

test("approval: malformed and two-line bodies never match", () => {
  const bad = [
    `${LINE} `,
    ` ${LINE}`,
    LINE.replace("v1", "v2"),
    LINE.replace("sha256:", ""),
    LINE.replace("c00-migration-approval", "c00-migration-approved"),
    LINE.replace(/ tree=.*/, ""),
    LINE.toUpperCase(),
    `${LINE}\n${LINE}`,
    `${LINE}\nextra`,
    `\n${LINE}`,
    `${LINE}\n\n`,
    `note\n${LINE}`,
    `${LINE.slice(0, -1)}`,
    LINE.replace("1".repeat(64), "1".repeat(63)),
    "",
    LINE.replace(/b{40}/, "B".repeat(40)),
  ];
  for (const body of bad) assert.equal(evalApproval([[comment({ body })]]).ok, false, JSON.stringify(body));
  assert.equal(evalApproval([[comment({ body: undefined })]]).ok, false);
});

test("approval: a match on page 2 is found; an invalid first page does not stop the search", () => {
  const filler = Array.from({ length: 100 }, (_, i) => comment({ id: i + 10, body: "lgtm" }));
  const r = evalApproval([filler, [comment({ id: 777 })]]);
  assert.deepEqual(r, { ok: true, commentId: 777, line: LINE });
});

test("approval: more than 10 pages of 100 comments refuses with no approval line", () => {
  const page = Array.from({ length: 100 }, (_, i) => comment({ id: i + 10, body: "lgtm" }));
  const pages = Array.from({ length: 10 }, () => page);
  assert.deepEqual(evalApproval(pages), { ok: false, reason: "approval-missing" });
  const over = evalApproval([...pages, [comment({ id: 5000 })]]);
  assert.deepEqual(over, { ok: false, reason: "approval-comments-over-limit" });
  // even a valid approval beyond the bound is not trusted
  assert.equal(evalApproval(pages, { truncated: true }).reason, "approval-comments-over-limit");
});

test("approval: an API error refuses with its own reason", () => {
  assert.equal(evalApproval([[comment()]], { error: "HTTP 502" }).reason, "approval-comments-error");
  assert.equal(evalApproval(undefined).reason, "approval-comments-error");
  assert.equal(evalApproval("nope").reason, "approval-comments-error");
  assert.equal(evalApproval([[comment()], "not a page"]).reason, "approval-comments-error");
});

test("approvalLine renders the one line and rejects malformed inputs", () => {
  assert.equal(approvalLine(INV, TREE_SHA), LINE);
  assert.throws(() => approvalLine("1".repeat(64), TREE_SHA));
  assert.throws(() => approvalLine(INV, "b".repeat(39)));
  assert.throws(() => approvalLine(INV, "B".repeat(40)));
});

test("decide: approval for this inventory and tree admits; any mismatch refuses", () => {
  const dir = mkTree({ migrations: { "1_a.up.sql": "ALTER TABLE t DROP COLUMN a;" } });
  try {
    const first = decide(baseInput(dir, { ledger: [] }));
    assert.equal(first.reason, "approval-missing");
    const line = first.approvalLine;
    const ok = decide(baseInput(dir, { ledger: [], comments: { pages: [[comment({ id: 42, body: line })]] } }));
    assert.equal(ok.decision, "admit");
    assert.equal(ok.approval.commentId, 42);
    assert.equal(ok.approval.line, line);

    const otherTree = decide(baseInput(dir, { ledger: [], treeSha: "d".repeat(40), comments: { pages: [[comment({ body: line })]] } }));
    assert.deepEqual([otherTree.decision, otherTree.reason], ["refuse", "approval-missing"]);
    assert.match(otherTree.approvalLine, /tree=d{40}$/);

    const agent = decide(baseInput(dir, { ledger: [], comments: { pages: [[comment({ body: line, user: { id: 282619718 } })]] } }));
    assert.equal(agent.decision, "refuse");

    const apiError = decide(baseInput(dir, { ledger: [], comments: { error: "HTTP 500" } }));
    assert.deepEqual([apiError.decision, apiError.reason], ["refuse", "approval-comments-error"]);
    assert.equal(apiError.approvalLine ?? null, null);

    const over = decide(baseInput(dir, { ledger: [], comments: { pages: [[]], truncated: true } }));
    assert.deepEqual([over.decision, over.reason], ["refuse", "approval-comments-over-limit"]);
    assert.equal(over.approvalLine ?? null, null);

    const noTree = decide(baseInput(dir, { ledger: [], treeSha: "", comments: { pages: [[comment({ body: line })]] } }));
    assert.equal(noTree.decision, "refuse");
    assert.equal(noTree.approvalLine ?? null, null);

    // approval of a different inventory: a changed up byte moves the hash
    writeFileSync(join(dir, "server/migrations/1_a.up.sql"), "ALTER TABLE t DROP COLUMN b;");
    const moved = decide(baseInput(dir, { ledger: [], comments: { pages: [[comment({ body: line })]] } }));
    assert.deepEqual([moved.decision, moved.reason], ["refuse", "approval-missing"]);
    assert.notEqual(moved.inventory, first.inventory);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// ---- imports and CLI ------------------------------------------------------------

test("the tool imports Node built-ins only", () => {
  const src = readFileSync(join(here, "migration-gate.mjs"), "utf8");
  const specs = [...src.matchAll(/(?:^|\n)\s*import\s[^;]*?from\s+["']([^"']+)["']/g)].map((m) => m[1]);
  assert.ok(specs.length > 0);
  for (const s of specs) assert.ok(s.startsWith("node:"), `non-built-in import: ${s}`);
  assert.doesNotMatch(src, /\bimport\s*\(/);
  assert.doesNotMatch(src, /\brequire\s*\(/);
  assert.doesNotMatch(src, /child_process|node:net|node:http|node:https|fetch\s*\(/);
});

function cli(args, { cwd = repoRoot } = {}) {
  const r = spawnSync(process.execPath, [join(here, "migration-gate.mjs"), ...args], { cwd, encoding: "utf8" });
  return { code: r.status, out: r.stdout, err: r.stderr };
}

test("CLI approval-line prints the line; bad input exits 2", () => {
  const ok = cli(["approval-line", "--inventory", INV, "--tree", TREE_SHA]);
  assert.equal(ok.code, 0);
  assert.equal(ok.out, `${LINE}\n`);
  assert.equal(cli(["approval-line", "--inventory", "x", "--tree", TREE_SHA]).code, 2);
  assert.equal(cli(["approval-line"]).code, 2);
  assert.equal(cli(["nope"]).code, 2);
  assert.equal(cli([]).code, 2);
});

test("CLI versions lists up versions of a tree", () => {
  const dir = mkTree({ migrations: { "2_b.up.sql": "", "1_a.up.sql": "", "1_a.down.sql": "" } });
  try {
    const r = cli(["versions", "--tree-root", dir]);
    assert.equal(r.code, 0);
    assert.equal(r.out, "1_a\n2_b\n");
    assert.equal(cli(["versions", "--tree-root", join(dir, "nope")]).code, 1);
    assert.equal(cli(["versions"]).code, 2);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("CLI inventory and decide work from any tree, exit 0 on admit and 1 on refuse", () => {
  const dir = mkTree({ migrations: { "1_a.up.sql": "ALTER TABLE t DROP COLUMN a;" } });
  try {
    const ledger = join(dir, "ledger.txt");
    writeFileSync(ledger, "");
    const common = ["--tree-root", dir, "--ledger", ledger, "--ledger-sha256", sha256("")];
    const inv = cli(["inventory", ...common]);
    assert.equal(inv.code, 0);
    const parsed = JSON.parse(inv.out);
    assert.match(parsed.inventory, /^sha256:[0-9a-f]{64}$/);
    assert.deepEqual(parsed.pending.map((p) => p.version), ["1_a"]);

    const args = ["decide", ...common, "--tree-sha", TREE_SHA, "--source-sha", SRC_SHA, "--checkout-sha", SRC_SHA];
    const refused = cli(args);
    assert.equal(refused.code, 1);
    const rd = JSON.parse(refused.out);
    assert.equal(rd.decision, "refuse");
    assert.equal(rd.approvalLine, `c00-migration-approval v1 inventory=${parsed.inventory} tree=${TREE_SHA}`);

    const comments = join(dir, "comments.json");
    writeFileSync(comments, JSON.stringify({ pages: [[comment({ id: 9, body: rd.approvalLine })]] }));
    const admitted = cli([...args, "--comments", comments]);
    assert.equal(admitted.code, 0);
    assert.equal(JSON.parse(admitted.out).decision, "admit");

    writeFileSync(comments, "{not json");
    const bad = cli([...args, "--comments", comments]);
    assert.equal(bad.code, 1);
    assert.equal(JSON.parse(bad.out).reason, "approval-comments-error");

    const badLedger = cli(["decide", ...common.slice(0, 2), "--ledger", join(dir, "missing.txt"), "--ledger-sha256", sha256(""), "--tree-sha", TREE_SHA, "--source-sha", SRC_SHA, "--checkout-sha", SRC_SHA]);
    assert.equal(badLedger.code, 1);
    assert.equal(JSON.parse(badLedger.out).reason, "ledger-unreadable");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("the tool runs from any working directory with no git and an empty environment", () => {
  const dir = mkTree({ migrations: { "1_a.up.sql": "CREATE INDEX i ON t (a);" } });
  const elsewhere = mkdtempSync(join(tmpdir(), "mg-cwd-"));
  try {
    const ledger = join(dir, "ledger.txt");
    writeFileSync(ledger, "");
    const r = spawnSync(
      process.execPath,
      [join(here, "migration-gate.mjs"), "decide", "--tree-root", dir, "--ledger", ledger, "--ledger-sha256", sha256(""), "--tree-sha", TREE_SHA, "--source-sha", SRC_SHA, "--checkout-sha", SRC_SHA],
      { cwd: elsewhere, env: {}, encoding: "utf8" },
    );
    assert.equal(r.status, 0, r.stderr);
    assert.equal(JSON.parse(r.stdout).decision, "admit");
  } finally {
    rmSync(dir, { recursive: true, force: true });
    rmSync(elsewhere, { recursive: true, force: true });
  }
});

test("the real tree at the pin classifies without a crash and is deterministic", () => {
  // inventoryOf skips the capability row: the capability file lands in a later step.
  const input = { treeRoot: repoRoot, ledgerBytes: Buffer.alloc(0), expectedLedgerSha256: sha256("") };
  const a = inventoryOf(input);
  const b = inventoryOf(input);
  assert.equal(a.ok, true);
  assert.equal(a.inventory, b.inventory);
  assert.ok(a.pending.length > 1000);
  assert.ok(a.pending.every((p) => ["additive", "needs-approval"].includes(p.class) && p.reason));
  assert.ok(a.pending.some((p) => p.class === "additive"));
  assert.ok(a.pending.some((p) => p.class === "needs-approval"));
});

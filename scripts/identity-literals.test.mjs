import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test, { after } from "node:test";
import { literalsOf } from "./identity-literals.mjs";

// CHE-8812 (plan r9 S2a): fixture tests on throwaway git repos. The script reads a tree object; the
// denylist and baseline come from a trusted directory outside that tree, as the CI job reads them
// from `workflow_sha`, never from the PR head.
const script = fileURLToPath(new URL("./identity-literals.mjs", import.meta.url));
const FIX = "move the value to a workspace setting, guard policy or plugin config";
const UUID = "11111111-2222-4333-8444-555555555555";
const denylist = {
  fix: FIX,
  patterns: [
    { id: "workspace-uuid", pattern: UUID },
    { id: "host-token", pattern: "x99|c00|mbp", wholeWord: true, ignoreCase: true },
    { id: "person-name", pattern: "Cheese|Jev|Hermes", wholeWord: true },
  ],
};

// CHE-10231: the protected names are read from the pattern, so the tests below never spell them out.
const personPattern = denylist.patterns.find((p) => p.id === "person-name");
personPattern.exemptMatches = ["Hermes"];
const protectedNames = personPattern.pattern.split("|").filter((name) => !personPattern.exemptMatches.includes(name));

const scratch = mkdtempSync(join(tmpdir(), "identity-literals-"));
after(() => rmSync(scratch, { recursive: true, force: true }));
let counter = 0;
const dir = (prefix) => mkdtempSync(join(scratch, `${prefix}${counter++}-`));

const git = (cwd, ...args) => execFileSync("git", args, { cwd, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
function write(root, files) {
  for (const [path, content] of Object.entries(files)) {
    mkdirSync(dirname(join(root, path)), { recursive: true });
    writeFileSync(join(root, path), content);
  }
}
function repo(files) {
  const root = dir("repo");
  git(root, "init", "-q");
  git(root, "config", "user.email", "t@example.com");
  git(root, "config", "user.name", "t");
  write(root, files);
  git(root, "add", "-A");
  git(root, "commit", "-q", "-m", "fixture");
  return root;
}
function trusted(baseline, deny = denylist, exempt = {}) {
  const root = dir("trusted");
  writeFileSync(join(root, "denylist.json"), JSON.stringify(deny));
  writeFileSync(join(root, "baseline.json"), JSON.stringify(baseline));
  writeFileSync(join(root, "exempt.json"), JSON.stringify(exempt));
  return { denylist: join(root, "denylist.json"), baseline: join(root, "baseline.json"), exempt: join(root, "exempt.json") };
}
function run(root, baseline = {}, extra = [], deny = denylist, exempt = {}) {
  const paths = trusted(baseline, deny, exempt);
  const result = spawnSync(process.execPath, [script, "--tree", "HEAD", "--denylist", paths.denylist, "--baseline", paths.baseline, "--exempt", paths.exempt, ...extra], { cwd: root, encoding: "utf8" });
  return { status: result.status, out: `${result.stdout}${result.stderr}`, stdout: result.stdout, stderr: result.stderr };
}

test("clean tree passes", () => {
  const root = repo({ "server/internal/a/a.go": 'package a\n\nvar name = "billing"\n' });
  const result = run(root);
  assert.equal(result.status, 0, result.out);
});

test("a new literal in a new file fails with file, line, pattern id and the fix", () => {
  const root = repo({ "server/internal/a/a.go": 'package a\n\nvar host = "c00"\n' });
  const result = run(root);
  assert.equal(result.status, 1, result.out);
  assert.match(result.stderr, /server\/internal\/a\/a\.go:3/);
  assert.match(result.stderr, /host-token/);
  assert.ok(result.stderr.includes(FIX));
});

test("each denylist family fails in a new file under server, packages and apps", () => {
  for (const [path, line, id] of [
    ["server/x.go", `var id = "${UUID}"`, "workspace-uuid"],
    ["packages/core/x.ts", 'export const owner = "Jev";', "person-name"],
    ["apps/web/x.tsx", "const host = 'X99';", "host-token"],
  ]) {
    const result = run(repo({ [path]: `${line}\n` }));
    assert.equal(result.status, 1, `${path}: ${result.out}`);
    assert.ok(result.stderr.includes(id), `${path}: ${result.out}`);
  }
});

test("a second hit in a baselined file fails", () => {
  const root = repo({ "server/internal/a/a.go": 'var a = "c00"\nvar b = "c00"\n' });
  const result = run(root, { "server/internal/a/a.go": { "host-token": 1 } });
  assert.equal(result.status, 1, result.out);
  assert.match(result.stderr, /server\/internal\/a\/a\.go:1/);
  assert.match(result.stderr, /server\/internal\/a\/a\.go:2/);
  assert.match(result.stderr, /baseline allows 1, found 2/);
});

test("a baselined file keeps passing at its baseline count", () => {
  const root = repo({ "server/internal/a/a.go": 'var a = "c00"\nvar b = "Cheese"\n' });
  const result = run(root, { "server/internal/a/a.go": { "host-token": 1, "person-name": 1 } });
  assert.equal(result.status, 0, result.out);
});

test("a new pattern id in a baselined file fails", () => {
  const root = repo({ "server/internal/a/a.go": 'var a = "c00"\nvar b = "Hermes"\n' });
  const result = run(root, { "server/internal/a/a.go": { "host-token": 1 } });
  assert.equal(result.status, 1, result.out);
  assert.match(result.stderr, /person-name/);
});

// CHE-10231: upstream text that names the runtime product is exempt by exact literal, never by count, so
// no allowance is left over for a new protected name.
const LANDING = "apps/web/features/landing/i18n/en.ts";
const UPSTREAM = "Hermes keeps its locally installed dependencies between runs.";
const exemptList = { [LANDING]: [UPSTREAM] };
const landing = (...lines) => repo({ [LANDING]: `export const a = [${lines.map((line) => JSON.stringify(line)).join(", ")}];\n` });

test("an exempt upstream literal passes with no baseline", () => {
  const result = run(landing(UPSTREAM, UPSTREAM), {}, [], denylist, exemptList);
  assert.equal(result.status, 0, result.out);
});

test("a new bare Hermes person literal fails next to exempt upstream literals", () => {
  const root = repo({ [LANDING]: `export const a = [${JSON.stringify(UPSTREAM)}];\nconst maintainer = "Hermes";\n` });
  const result = run(root, {}, [], denylist, exemptList);
  assert.equal(result.status, 1, result.out);
  assert.match(result.stderr, /en\.ts:2.*person-name/);
  assert.doesNotMatch(result.stderr, /en\.ts:1 /);
});

test("a new Hermes sentence that is not on the exempt list fails", () => {
  const result = run(landing(UPSTREAM, "Hermes approved the release."), {}, [], denylist, exemptList);
  assert.equal(result.status, 1, result.out);
  assert.match(result.stderr, /en\.ts:1.*person-name/);
});

test("an exempt literal does not hide another protected name in the same literal", () => {
  for (const name of protectedNames) {
    const text = `${UPSTREAM} ${name}`;
    const result = run(landing(text), {}, [], denylist, { [LANDING]: [text] });
    assert.equal(result.status, 1, `${name}: ${result.out}`);
  }
});

test("an exempt literal applies only to its own file", () => {
  const root = repo({ "apps/web/other.ts": `export const a = ${JSON.stringify(UPSTREAM)};\n` });
  assert.equal(run(root, {}, [], denylist, exemptList).status, 1);
});

test("a malformed exempt list makes the check unable to run", () => {
  const result = run(landing(UPSTREAM), {}, [], denylist, { [LANDING]: "not-a-list" });
  assert.equal(result.status, 2, result.out);
});

test("a lower count passes and prints the baseline line to remove", () => {
  const root = repo({ "server/internal/a/a.go": 'var a = "c00"\n', "server/internal/b/b.go": "package b\n" });
  const result = run(root, { "server/internal/a/a.go": { "host-token": 3 }, "server/internal/b/b.go": { "person-name": 1 } });
  assert.equal(result.status, 0, result.out);
  assert.match(result.stdout, /server\/internal\/a\/a\.go host-token 3 -> 1/);
  assert.match(result.stdout, /server\/internal\/b\/b\.go person-name 1 -> 0/);
  assert.match(result.stdout, /remove|lower/i);
});

test("a baseline edited in the head cannot raise the allowance", () => {
  const root = repo({
    "server/internal/a/a.go": 'var a = "c00"\nvar b = "c00"\n',
    "scripts/identity-literals/baseline.json": JSON.stringify({ "server/internal/a/a.go": { "host-token": 99 } }),
    "scripts/identity-literals/denylist.json": JSON.stringify({ fix: FIX, patterns: [] }),
  });
  const result = run(root, { "server/internal/a/a.go": { "host-token": 1 } });
  assert.equal(result.status, 1, result.out);
});

test("the default denylist and baseline come from the script directory, not the tree", () => {
  const root = repo({
    "server/a.go": 'var a = "c00"\n',
    "scripts/identity-literals/baseline.json": JSON.stringify({ "server/a.go": { "host-token": 99 } }),
  });
  const result = spawnSync(process.execPath, [script, "--tree", "HEAD"], { cwd: root, encoding: "utf8" });
  assert.equal(result.status, 1, `${result.stdout}${result.stderr}`);
});

test("test files, testdata, docs and migrations are out of scope", () => {
  const root = repo({
    "server/internal/a/a_test.go": 'var a = "c00"\n',
    "packages/core/a.test.ts": 'const a = "c00";\n',
    "packages/core/a.test.tsx": 'const a = "c00";\n',
    "server/internal/a/testdata/a.json": '{"host": "c00"}\n',
    "packages/core/testdata/b.json": '{"host": "c00"}\n',
    "apps/docs/content/a.mdx": 'Run on "c00".\n',
    "server/migrations/001_a.up.sql": "INSERT INTO t VALUES ('c00');\n",
    "scripts/a.mjs": 'export const a = "c00";\n',
    "README.md": 'host "c00"\n',
  });
  const result = run(root);
  assert.equal(result.status, 0, result.out);
});

test("comments are not string literals; a literal before a trailing comment still counts", () => {
  const clean = repo({
    "server/a.go": '// host "c00" in a comment\nvar a = 1 // "c00"\n',
    "server/b.py": '# host "c00"\nx = 1  # "c00"\n',
    "packages/c.ts": 'const x = 1; // c00 and "Cheese"\n',
    "server/d.go": "// Cheese runs on c00\nvar host = HostC00\n",
  });
  assert.equal(run(clean).status, 0, run(clean).out);
  const dirty = repo({ "server/a.go": 'var a = "c00" // trailing note\n' });
  assert.equal(run(dirty).status, 1);
});

test("a token outside a string literal or inside a longer word is not a hit", () => {
  const root = repo({
    "server/a.go": 'const HostC00 = 1\nvar abc00d = "abc00d"\nvar s = "x990"\nvar t = "Cheeseburger"\nvar u = "prefix_mbp"\n',
  });
  const result = run(root);
  assert.equal(result.status, 0, result.out);
});

test("whole-word hits inside larger literals count", () => {
  const root = repo({ "server/a.go": 'var u = "https://x99.example.test/path"\nvar v = "host: C00"\n' });
  const result = run(root);
  assert.equal(result.status, 1, result.out);
  assert.match(result.stderr, /found 2/);
});

test("a rename drops the old baseline entry and the new path fails", () => {
  const root = repo({ "server/old/a.go": 'var a = "c00"\n' });
  const baseline = { "server/old/a.go": { "host-token": 1 } };
  assert.equal(run(root, baseline).status, 0);
  git(root, "mv", "server/old/a.go", "server/new_a.go");
  git(root, "commit", "-q", "-m", "rename");
  const result = run(root, baseline);
  assert.equal(result.status, 1, result.out);
  assert.match(result.stderr, /server\/new_a\.go:1/);
  assert.match(result.stdout, /server\/old\/a\.go host-token 1 -> 0/);
});

test("the script reads the tree object, not the working directory", () => {
  const root = repo({ "server/a.go": 'var a = "ok"\n' });
  write(root, { "server/b.go": 'var b = "c00"\n' });
  assert.equal(run(root).status, 0, "an uncommitted file is not in the tree");
  git(root, "add", "-A");
  git(root, "commit", "-q", "-m", "add b");
  assert.equal(run(root).status, 1);
  rmSync(join(root, "server/b.go"));
  assert.equal(run(root).status, 1, "a deleted working file stays in the committed tree");
  const first = git(root, "rev-list", "--max-parents=0", "HEAD");
  const paths = trusted({});
  const older = spawnSync(process.execPath, [script, "--tree", first, "--denylist", paths.denylist, "--baseline", paths.baseline], { cwd: root, encoding: "utf8" });
  assert.equal(older.status, 0, older.stderr);
});

test("an unknown tree or a malformed baseline is an error, not a pass", () => {
  const root = repo({ "server/a.go": 'var a = "ok"\n' });
  const paths = trusted({});
  const missing = spawnSync(process.execPath, [script, "--tree", "no-such-ref", "--denylist", paths.denylist, "--baseline", paths.baseline], { cwd: root, encoding: "utf8" });
  assert.equal(missing.status, 2, missing.stderr);
  assert.equal(run(root, { "server/a.go": { "host-token": "many" } }).status, 2);
  assert.equal(run(root, [1]).status, 2);
});

test("--print-baseline prints counts that then pass", () => {
  const root = repo({ "server/a.go": 'var a = "c00"\nvar b = "Jev"\nvar c = "x99"\n', "packages/b.ts": 'const n = "Hermes";\n' });
  const printed = run(root, {}, ["--print-baseline"]);
  assert.equal(printed.status, 0, printed.out);
  const baseline = JSON.parse(printed.stdout);
  assert.deepEqual(baseline, { "packages/b.ts": { "person-name": 1 }, "server/a.go": { "host-token": 2, "person-name": 1 } });
  assert.equal(run(root, baseline).status, 0);
});

test("literalsOf strips // and # comments and keeps quoted text", () => {
  assert.deepEqual(literalsOf('a := "x" // "y"\n').map((l) => l.text), ["x"]);
  assert.deepEqual(literalsOf("b = 'p'  # 'q'\n").map((l) => l.text), ["p"]);
  assert.deepEqual(literalsOf('u := "http://h/#frag"\n').map((l) => l.text), ["http://h/#frag"]);
  assert.deepEqual(literalsOf('s := "a\\"b" + `c`\n').map((l) => l.text), ['a\\"b', "c"]);
});

test("literalsOf follows a multi-line raw string and numbers its lines", () => {
  const found = literalsOf("a := `one\n// two\n# three`\nb := 1\n");
  assert.deepEqual(found.map((l) => [l.line, l.text]), [[1, "one"], [2, "// two"], [3, "# three"]]);
});

test("the committed denylist and baseline parse and have unique pattern ids", () => {
  const base = new URL("./identity-literals/", import.meta.url);
  const list = JSON.parse(readFileSync(new URL("denylist.json", base), "utf8"));
  const ids = list.patterns.map((p) => p.id);
  assert.equal(new Set(ids).size, ids.length);
  assert.ok(ids.includes("workspace-uuid") && ids.includes("host-token") && ids.includes("person-name"));
  assert.ok(list.fix.includes("workspace setting"));
  const baseline = JSON.parse(readFileSync(new URL("baseline.json", base), "utf8"));
  for (const [file, counts] of Object.entries(baseline)) {
    for (const [id, count] of Object.entries(counts)) {
      assert.ok(ids.includes(id), `${file}: unknown pattern id ${id}`);
      assert.ok(Number.isInteger(count) && count > 0, `${file} ${id}`);
    }
  }
});

test("the committed exempt list parses and exempts only plain Hermes mentions", () => {
  const exempt = JSON.parse(readFileSync(new URL("./identity-literals/exempt-literals.json", import.meta.url), "utf8"));
  for (const [file, texts] of Object.entries(exempt)) {
    assert.ok(texts.length > 0, file);
    for (const text of texts) {
      assert.match(text, /\bHermes\b/, `${file}: ${text}`);
      assert.doesNotMatch(text, new RegExp(protectedNames.join("|")), `${file}: ${text}`);
    }
  }
});

// CHE-8818 (plan r9 S2b): the CI wiring. `pull_request_target` runs the base definition, so the job
// reads the PR head as data (fetched, never checked out) and the denylist and baseline from `workflow_sha`.
test("the identity-literals job reads the PR head as data with no secret and no write permission", () => {
  const workflow = readFileSync(new URL("../.github/workflows/pr-guards.yml", import.meta.url), "utf8").replace(/^\s*#.*$/gm, "");
  const start = workflow.indexOf("\n  identity-literals:");
  assert.notEqual(start, -1, "pr-guards.yml has an identity-literals job");
  const rest = workflow.slice(start + 1);
  const next = rest.slice(1).search(/^ {2}[\w-]+:$/m);
  const job = next === -1 ? rest : rest.slice(0, next + 1);
  assert.match(job, /permissions:\n {6}contents: read\n {4}steps:/);
  assert.match(job, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(job, /persist-credentials: false/);
  assert.match(job, /git fetch --no-tags origin "refs\/pull\/\$PR_NUMBER\/head"/);
  assert.match(job, /test "\$\(git rev-parse FETCH_HEAD\)" = "\$EXPECTED_HEAD"/);
  assert.match(job, /run: node scripts\/identity-literals\.mjs$/m);
  assert.doesNotMatch(job, /secrets\.|: write|continue-on-error|--tree|--denylist|--baseline|pull_request\.head\.(ref|repo)/);
});

test("literalsOf: an apostrophe in prose opens a quote to the end of the line, so it can only add hits (known limit, CHE-8812 P2)", () => {
  assert.deepEqual(literalsOf("<p>don't use c00 here</p>\n").map((l) => l.text), ["t use c00 here</p>"]);
  assert.deepEqual(literalsOf("<p>it's fine</p> <b>c00</b>\n").map((l) => l.text), ["s fine</p> <b>c00</b>"]);
});

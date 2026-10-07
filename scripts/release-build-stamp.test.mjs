import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import test from "node:test";

const root = new URL("../", import.meta.url);
const workflow = readFileSync(new URL(".github/workflows/release.yml", root), "utf8");
const goreleaser = readFileSync(new URL(".goreleaser.yml", root), "utf8");

test("the vendored shared helper matches the published CHE-1308 revision", () => {
  const helper = readFileSync(new URL("scripts/release-stamp.sh", root));
  assert.equal(createHash("sha256").update(helper).digest("hex"), "689f5b64bdb31796de1d0980f930e6db36e69260393c47d9c7b82c3f906be389");
});

test("release artifacts share one ICT stamp from the canonical helper", () => {
  assert.equal(workflow.match(/sh scripts\/release-stamp\.sh/g)?.length, 1);
  assert.match(workflow, /build_stamp: \$\{\{ steps\.build_meta\.outputs\.build_stamp \}\}/);
  assert.match(workflow, /RELEASE_BUILD_STAMP: \$\{\{ needs\.verify\.outputs\.build_stamp \}\}/);
  assert.match(goreleaser, /main\.version=\{\{\.Version\}\}\{\{if \.Env\.RELEASE_BUILD_STAMP\}\}-\{\{\.Env\.RELEASE_BUILD_STAMP\}\}\{\{end\}\}/);
  assert.match(workflow, /VERSION=\$\{\{ needs\.verify\.outputs\.display_version \}\}/);
  assert.match(workflow, /NEXT_PUBLIC_APP_VERSION=\$\{\{ needs\.verify\.outputs\.display_version \}\}/);
  assert.equal(workflow.match(/type=raw,value=\$\{\{ needs\.verify\.outputs\.display_version \}\}/g)?.length, 2);
});

test("development and PR builds do not invoke the release stamp helper", () => {
  const ci = readFileSync(new URL(".github/workflows/ci.yml", root), "utf8");
  const makefile = readFileSync(new URL("Makefile", root), "utf8");
  assert.doesNotMatch(ci, /sh scripts\/release-stamp\.sh|RELEASE_BUILD_STAMP=/);
  assert.doesNotMatch(makefile, /release-stamp\.sh|RELEASE_BUILD_STAMP/);
});

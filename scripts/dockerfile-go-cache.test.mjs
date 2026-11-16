import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-3983: the CD BuildKit builder is deleted after every job (CHE-4314), so a
// `--mount=type=cache` Go build cache is always cold. Third-party and standard
// library packages must be compiled in a layer that depends only on the import
// set, so the registry layer cache can restore it.
const text = readFileSync(new URL("../Dockerfile", import.meta.url), "utf8");
const stage = (name) => {
  const start = text.search(new RegExp(`^FROM .* AS ${name}$`, "m"));
  assert.notEqual(start, -1, `Dockerfile has no stage named ${name}`);
  const rest = text.slice(start + 1);
  const next = rest.search(/^FROM /m);
  return next === -1 ? rest : rest.slice(0, next);
};

test("deps-compiled builds the dependency packages into GOCACHE", () => {
  const body = stage("deps-compiled");
  assert.match(body, /GOCACHE=\/gocache\/server/);
  assert.match(body, /GOCACHE=\/gocache\/daemon/);
  assert.doesNotMatch(body, /COPY server\//, "source edits would invalidate the layer");
});

// A pipe hides a failing `go list`: a partial package list would silently
// shrink the cached layer instead of failing the build.
test("deps-list writes each package list without piping go list", () => {
  const body = stage("deps-list");
  assert.match(body, /go list /);
  assert.doesNotMatch(body, /go list [^\n]*(?<!\|)\|(?!\|)/, "pipe after go list masks its exit status");
});

for (const name of ["builder", "daemon-builder"]) {
  test(`${name} starts from the precompiled GOCACHE`, () => {
    const body = stage(name);
    assert.match(body, /COPY --from=deps-compiled \/gocache\/\w+ \/gocache/);
    assert.match(body, /ENV GOCACHE=\/gocache/);
  });
  test(`${name} does not rely on a Go build-cache mount`, () => {
    assert.doesNotMatch(stage(name), /target=\/root\/\.cache\/go-build/);
  });
}

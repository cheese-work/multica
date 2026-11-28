import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-5342: pnpm reads every root `pnpm.patchedDependencies` file during
// `pnpm install`, even when the owning workspace is not in the image. A Dockerfile
// that installs without copying a patch file fails with ENOENT.
const read = (path) => readFileSync(new URL(`../${path}`, import.meta.url), "utf8");
const patchPaths = Object.values(JSON.parse(read("package.json")).pnpm.patchedDependencies);

for (const dockerfile of ["Dockerfile.web", "Dockerfile.docs"]) {
  const text = read(dockerfile);
  // The first stage that runs `pnpm install` is the one that needs the patch files.
  const beforeInstall = text.slice(0, text.indexOf("pnpm install"));
  const copiedDirs = [...beforeInstall.matchAll(/^COPY\s+(\S+)\s+\S+\s*$/gm)].map((m) => m[1]);
  for (const patch of patchPaths) {
    test(`${dockerfile} copies ${patch} before pnpm install`, () => {
      const copied = copiedDirs.some(
        (src) => src === patch || (src.endsWith("/") && patch.startsWith(src)),
      );
      assert.ok(copied, `${dockerfile} does not COPY ${patch} before its first pnpm install`);
    });
  }
}

// CHE-10703: the docs `postinstall` runs fumadocs-mdx, which loads source.config.ts and
// its relative imports. The deps stage must COPY each of them before `pnpm install`.
{
  const text = read("Dockerfile.docs");
  const beforeInstall = text.slice(0, text.indexOf("pnpm install"));
  const copied = [...beforeInstall.matchAll(/^COPY\s+(\S+)\s+\S+\s*$/gm)].map((m) => m[1]);
  const imports = [...read("apps/docs/source.config.ts").matchAll(/from "\.\/([^"]+)"/g)];
  for (const [, rel] of imports) {
    const path = `apps/docs/${rel}.ts`;
    test(`Dockerfile.docs copies ${path} before pnpm install`, () => {
      assert.ok(
        copied.some((src) => src === path || (src.endsWith("/") && path.startsWith(src))),
        `Dockerfile.docs does not COPY ${path} before its first pnpm install`,
      );
    });
  }
}

// @vitest-environment node
/**
 * The dashboard layout wraps every workspace route, so whatever it imports
 * statically ships to inbox, chat and projects as well as issues (CHE-3018).
 * This walks its static import graph and fails if a heavy, rarely-opened
 * surface (modals, chat window, search palette, issue detail, editor) is reachable without a
 * dynamic `import()`. Dynamic imports are not followed: they split a chunk.
 */
import { existsSync, readFileSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

const webRoot = resolve(__dirname, "..");
const viewsRoot = resolve(webRoot, "../../packages/views");
const viewsExports: Record<string, string> = JSON.parse(
  readFileSync(join(viewsRoot, "package.json"), "utf8"),
).exports;

// `import type` is erased at build time, so it is not an edge.
const STATIC_IMPORT = /^\s*(?:import|export)\s(?!type\s)[^;'"]*?from\s*["']([^"']+)["']|^\s*import\s*["']([^"']+)["']/gm;

function file(path: string): string | null {
  for (const candidate of [path, `${path}.ts`, `${path}.tsx`, join(path, "index.ts"), join(path, "index.tsx")]) {
    if (existsSync(candidate) && statSync(candidate).isFile()) return candidate;
  }
  return null;
}

function resolveImport(from: string, spec: string): string | null {
  if (spec.startsWith(".")) return file(resolve(dirname(from), spec));
  if (spec.startsWith("@/")) return file(join(webRoot, spec.slice(2)));
  if (spec.startsWith("@multica/views/")) {
    const target = viewsExports[`./${spec.slice("@multica/views/".length)}`];
    return target ? file(join(viewsRoot, target)) : null;
  }
  return null; // node_modules and the other workspace packages are leaves
}

function staticGraph(entry: string): Set<string> {
  const seen = new Set<string>();
  const queue = [entry];
  while (queue.length) {
    const current = queue.pop()!;
    if (seen.has(current)) continue;
    seen.add(current);
    for (const match of readFileSync(current, "utf8").matchAll(STATIC_IMPORT)) {
      const next = resolveImport(current, (match[1] ?? match[2])!);
      if (next) queue.push(next);
    }
  }
  return seen;
}

/** Files in the static graph that import Tiptap or ProseMirror directly (the editor engine, ~100 KiB). */
function engineImporters(entry: string): string[] {
  return [...staticGraph(entry)].filter((f) =>
    [...readFileSync(f, "utf8").matchAll(STATIC_IMPORT)].some((m) => /^@tiptap\//.test((m[1] ?? m[2])!)),
  );
}

const dashboard = join(webRoot, "app/[workspaceSlug]/(dashboard)");
const reachableFrom = (entry: string, suffix: string) =>
  [...staticGraph(join(dashboard, entry))].filter((f) => f.endsWith(suffix));

describe("dashboard layout static import graph", () => {
  it.each([
    "views/modals/create-issue-dialog.tsx",
    "views/modals/create-project.tsx",
    "views/modals/create-squad.tsx",
    "views/chat/components/chat-window.tsx",
    "views/search/search-command.tsx",
    "views/issues/components/index.ts",
    "views/issues/components/issue-detail.tsx",
    "views/editor/content-editor.tsx",
  ])("does not statically reach %s", (suffix) => {
    expect(reachableFrom("layout.tsx", suffix)).toEqual([]);
  });
});

// Issue detail and the editor belong to the detail route and the chat composer;
// the list-style pages and chat (composer editor) must load them on demand only (CHE-3018).
describe("list pages static import graph", () => {
  it.each(["chat/page.tsx", "inbox/page.tsx", "issues/page.tsx", "projects/page.tsx"])(
    "%s does not statically reach issue detail or the editor",
    (entry) => {
      expect(reachableFrom(entry, "views/issues/components/issue-detail.tsx")).toEqual([]);
      expect(reachableFrom(entry, "views/editor/content-editor.tsx")).toEqual([]);
    },
  );

  // The composer defers its editor, but a helper that imports Tiptap would drag the engine back in.
  it.each(["chat/page.tsx", "inbox/page.tsx", "issues/page.tsx", "projects/page.tsx"])(
    "%s does not statically reach the Tiptap engine",
    (entry) => {
      expect(engineImporters(join(dashboard, entry))).toEqual([]);
    },
  );
});

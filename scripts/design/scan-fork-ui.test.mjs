import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  classifySurface,
  countHexColors,
  countRawElements,
  scan,
} from "./scan-fork-ui.mjs";

test("counts raw button/input/select/textarea tags, not components or lookalikes", () => {
  const src = `
    <button onClick={a}>x</button>
    <Button>ok</Button>
    <input type="text" />
    <InputGroup />
    <select><option/></select>
    <textarea/>
    <buttonish />
  `;
  assert.equal(countRawElements(src), 4);
});

test("counts hex colours in literals, not issue refs or url fragments", () => {
  assert.equal(countHexColors(`const c = "#fff"; const d = '#A1B2C3'; x = "#12345678"`), 3);
  assert.equal(countHexColors(`// see #1234 and (#7113) and href="#section" and #abcdefg`), 0);
});

test("classifies surfaces; wakeup and hierarchy by name beat directory", () => {
  const cases = {
    "packages/views/issues/components/wakeup-pickers.tsx": "wakeups",
    "packages/views/settings/components/wakeups-tab.tsx": "wakeups",
    "packages/views/chat/components/scheduled-wakeups-chip.tsx": "wakeups",
    "packages/views/issues/components/hierarchy/hierarchy-dialog.tsx": "hierarchy",
    "packages/views/issues/components/issue-header.tsx": "issues",
    "packages/views/chat/components/chat-annotations.tsx": "chat/DM",
    "packages/views/settings/components/general-tab.tsx": "settings",
    "packages/views/agents/components/agent-list.tsx": "other",
  };
  for (const [path, surface] of Object.entries(cases)) {
    assert.equal(classifySurface(path), surface, path);
  }
});

function git(cwd, ...args) {
  return execFileSync("git", args, { cwd, encoding: "utf8" });
}

test("scan lists only added non-test tsx files in scope and reports totals", () => {
  const dir = mkdtempSync(join(tmpdir(), "scan-fork-ui-"));
  try {
    git(dir, "init", "-q", "-b", "main");
    git(dir, "config", "user.email", "t@t");
    git(dir, "config", "user.name", "t");
    const put = (p, s) => {
      mkdirSync(join(dir, p, ".."), { recursive: true });
      writeFileSync(join(dir, p), s);
    };
    put("packages/views/issues/components/shared.tsx", "<button/>");
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "upstream");
    git(dir, "branch", "up");
    put("packages/views/issues/components/wakeup-pickers.tsx", "<button/><input/>");
    put("packages/views/chat/components/ann.tsx", `const c = "#abc";`);
    put("packages/views/chat/components/ann.test.tsx", "<button/>");
    put("packages/views/chat/components/plain.tsx", "<Button/>");
    put("apps/web/page.tsx", "<button/>");
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "fork");

    const r = scan({ cwd: dir, ref: "HEAD", base: "up", scope: "packages/views" });
    assert.equal(r.forkOnlyFiles, 3);
    assert.equal(r.withRawElements, 1);
    assert.equal(r.scopeFiles, 4);
    assert.equal(r.scopeWithRawElements, 2);
    assert.equal(r.withHex, 1);
    assert.deepEqual(
      r.files.map((f) => [f.path, f.surface, f.raw, f.hex]),
      [
        ["packages/views/chat/components/ann.tsx", "chat/DM", 0, 1],
        ["packages/views/chat/components/plain.tsx", "chat/DM", 0, 0],
        ["packages/views/issues/components/wakeup-pickers.tsx", "wakeups", 2, 0],
      ],
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("scan flags fork files that git sees as renamed upstream files", () => {
  const dir = mkdtempSync(join(tmpdir(), "scan-fork-ui-"));
  try {
    git(dir, "init", "-q", "-b", "main");
    git(dir, "config", "user.email", "t@t");
    git(dir, "config", "user.name", "t");
    mkdirSync(join(dir, "packages/views/issues"), { recursive: true });
    const body = "<button/>\n".repeat(20);
    writeFileSync(join(dir, "packages/views/issues/old.tsx"), body);
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "upstream");
    git(dir, "branch", "up");
    git(dir, "mv", "packages/views/issues/old.tsx", "packages/views/issues/new.tsx");
    writeFileSync(join(dir, "packages/views/issues/fresh.tsx"), "<Button/>");
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "fork");

    const r = scan({ cwd: dir, ref: "HEAD", base: "up", scope: "packages/views" });
    assert.equal(r.forkOnlyFiles, 2);
    assert.equal(r.renamedFromUpstream, 1);
    assert.deepEqual(
      r.files.map((f) => [f.path, f.renamedFrom ?? null]),
      [
        ["packages/views/issues/fresh.tsx", null],
        ["packages/views/issues/new.tsx", "packages/views/issues/old.tsx"],
      ],
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

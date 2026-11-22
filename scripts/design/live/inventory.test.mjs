import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { buildInventory, checkInventory, scanSource, serializeInventory } from "./inventory.mjs";

const SCRIPT = fileURLToPath(new URL("./inventory.mjs", import.meta.url));
const REPO = fileURLToPath(new URL("../../../", import.meta.url));

const DECISION = `
export type DecisionState = "pending" | "effective" | "disclaimed";
export type DecisionCommentKind = "ask" | "quote";
export interface DecisionRequestCard {
  id: string;
  state: "open" | "answered";
  batch_id?: string;
}
export interface DecisionRequestAnswer { state: "recorded" | "delivered"; }
export interface DecisionBatchItem { action_id: string; }
export interface DecisionCoveredCard { id: string; }
export interface DecisionRequestAction { id: string; }
`;
const DIALOG = `
import { Dialog } from "@multica/ui";
export function CreateIssue() {
  return (
    <Dialog open>
      <DialogContent />
    </Dialog>
  );
}
`;
const TOAST = `
import { toast } from "sonner";
export function save() {
  toast.success("saved");
}
`;

function fixture(files = {}) {
  const root = mkdtempSync(join(tmpdir(), "inventory-"));
  const all = {
    "packages/core/types/decision.ts": DECISION,
    "packages/views/issues/create.tsx": DIALOG,
    "apps/web/lib/save.ts": TOAST,
    ...files,
  };
  for (const [path, text] of Object.entries(all)) {
    mkdirSync(dirname(join(root, path)), { recursive: true });
    writeFileSync(join(root, path), text);
  }
  return root;
}
const write = (root, path, text) => {
  mkdirSync(dirname(join(root, path)), { recursive: true });
  writeFileSync(join(root, path), text);
};
const regenerate = (root) => write(root, "design/live-inventory.json", serializeInventory(buildInventory(root)));
const ids = (items) => items.map((i) => i.id);
const run = (root, ...args) => spawnSync(process.execPath, [SCRIPT, "--root", root, ...args], { encoding: "utf8" });

test("a new toast call is inventory-stale until the file is regenerated", () => {
  const root = fixture();
  try {
    regenerate(root);
    assert.deepEqual(checkInventory(root), []);
    assert.equal(run(root, "--check").status, 0);

    write(root, "apps/web/lib/save.ts", `${TOAST}\nexport function drop() {\n  toast.error("dropped");\n}\n`);
    const errors = checkInventory(root);
    assert.deepEqual(errors.map((e) => e.code), ["inventory-stale"]);
    assert.match(errors[0].detail, /apps\/web\/lib\/save\.ts#drop#1/);
    const cli = run(root, "--check");
    assert.equal(cli.status, 1);
    assert.match(cli.stderr, /inventory-stale/);

    assert.equal(run(root, "--write").status, 0);
    assert.deepEqual(checkInventory(root), []);
    assert.equal(run(root, "--check").status, 0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("a missing inventory file is inventory-stale", () => {
  const root = fixture();
  try {
    assert.deepEqual(checkInventory(root).map((e) => e.code), ["inventory-stale"]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("moving a dialog down 10 lines keeps its id", () => {
  const root = fixture();
  try {
    const before = buildInventory(root).dialogs;
    assert.deepEqual(ids(before), ["packages/views/issues/create.tsx#CreateIssue#1"]);
    write(root, "packages/views/issues/create.tsx", DIALOG.replace("export function", `${"// pad\n".repeat(10)}export function`));
    const after = buildInventory(root).dialogs;
    assert.deepEqual(ids(after), ids(before));
    assert.notEqual(after[0].line, before[0].line);
    // The file differs only by the line number, so the committed file must be regenerated to match.
    assert.equal(after[0].line - before[0].line, 10);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("every decision union literal and card shape is listed", () => {
  const root = fixture();
  try {
    const v = buildInventory(root).decisionVariants;
    assert.deepEqual(ids(v), [
      "packages/core/types/decision.ts#DecisionBatchItem#shape",
      "packages/core/types/decision.ts#DecisionCommentKind#ask",
      "packages/core/types/decision.ts#DecisionCommentKind#quote",
      "packages/core/types/decision.ts#DecisionCoveredCard#shape",
      "packages/core/types/decision.ts#DecisionRequestAction#shape",
      "packages/core/types/decision.ts#DecisionRequestCard.state#answered",
      "packages/core/types/decision.ts#DecisionRequestCard.state#open",
      "packages/core/types/decision.ts#DecisionState#disclaimed",
      "packages/core/types/decision.ts#DecisionState#effective",
      "packages/core/types/decision.ts#DecisionState#pending",
    ]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("a decision union that is not found fails loudly", () => {
  const root = fixture({ "packages/core/types/decision.ts": DECISION.replace("DecisionState", "DecisionStatus") });
  try {
    assert.throws(() => buildInventory(root), /decision-union-missing.*DecisionState/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("the real decision.ts lists each literal of its three unions", () => {
  const v = buildInventory(REPO).decisionVariants.map((x) => x.id.replace("packages/core/types/decision.ts#", ""));
  for (const id of [
    "DecisionRequestCard.state#open",
    "DecisionRequestCard.state#answered",
    "DecisionRequestCard.state#withdrawn",
    "DecisionState#pending",
    "DecisionState#effective",
    "DecisionState#disclaimed",
    ...["ask", "quote", "disclaim", "delivery", "cancel", "veto"].map((k) => `DecisionCommentKind#${k}`),
    "DecisionBatchItem#shape",
    "DecisionCoveredCard#shape",
    "DecisionRequestAction#shape",
  ]) {
    assert.ok(v.includes(id), `missing ${id}`);
  }
});

test("Dialog, AlertDialog, Sheet and Drawer count; DialogContent and a lowercase tag do not", () => {
  const src = `
    export function A() {
      return (<><Dialog /><AlertDialog></AlertDialog><Sheet /><Drawer /><DialogContent /><dialog /></>);
    }`;
  const { dialogs } = scanSource("packages/views/a.tsx", src);
  assert.deepEqual(dialogs.map((d) => d.element), ["Dialog", "AlertDialog", "Sheet", "Drawer"]);
  assert.deepEqual(ids(dialogs), ["A#1", "A#2", "A#3", "A#4"].map((x) => `packages/views/a.tsx#${x}`));
});

test("toast calls: bare, member, aliased import; a toast from elsewhere is ignored", () => {
  const sonner = scanSource(
    "apps/web/t.tsx",
    `import { toast as notify } from "sonner";
     export const Panel = () => {
       const onSave = () => { notify("a"); notify.promise(p, {}); };
       return <b onClick={() => notify.error("x")} />;
     };`,
  ).toasts;
  assert.deepEqual(sonner.map((t) => [t.id.split("#").slice(1).join("#"), t.method]), [
    ["Panel.onSave#1", "toast"],
    ["Panel.onSave#2", "promise"],
    ["Panel#1", "error"],
  ]);
  const other = scanSource("apps/web/u.ts", `import { toast } from "./local-toast"; export function f() { toast.success("x"); }`).toasts;
  assert.deepEqual(other, []);
  const none = scanSource("apps/web/v.ts", `export function f() { toast.success("x"); }`).toasts;
  assert.deepEqual(none, []);
});

test("ids use <module> outside a named symbol and are unique and sorted", () => {
  const { toasts } = scanSource("apps/web/m.ts", `import { toast } from "sonner"; toast("a"); toast("b");`);
  assert.deepEqual(ids(toasts), ["apps/web/m.ts#<module>#1", "apps/web/m.ts#<module>#2"]);
});

test("tests, stories, e2e and build output are not scanned", () => {
  const root = fixture({
    "packages/views/issues/create.test.tsx": DIALOG,
    "packages/views/__tests__/x.tsx": DIALOG,
    "packages/views/issues/create.stories.tsx": DIALOG,
    "apps/web/e2e/x.ts": TOAST,
    "apps/web/.next/x.tsx": DIALOG,
    "packages/views/node_modules/x/y.tsx": DIALOG,
    "packages/ui/outside.tsx": DIALOG,
  });
  try {
    const inv = buildInventory(root);
    assert.deepEqual(ids(inv.dialogs), ["packages/views/issues/create.tsx#CreateIssue#1"]);
    assert.deepEqual(ids(inv.toasts), ["apps/web/lib/save.ts#save#1"]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("counts per kind are in the file and the CLI prints them", () => {
  const root = fixture();
  try {
    const inv = buildInventory(root);
    assert.deepEqual(inv.counts, { dialogs: 1, toasts: 1, decisionVariants: 10 });
    regenerate(root);
    assert.deepEqual(JSON.parse(readFileSync(join(root, "design/live-inventory.json"), "utf8")).counts, inv.counts);
    const out = run(root, "--check");
    assert.match(out.stdout, /dialogs: 1, toasts: 1, decision variants: 10/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("the CLI exits 2 without a mode", () => {
  assert.equal(run(fixture()).status, 2);
});

test("a dialog and a toast in one symbol get different ids, and moving lines keeps both", () => {
  const src = (pad) => `import { toast } from "sonner";
${pad}export function DeleteViewDialog() {
  const onDelete = () => {};
  toast.success("a");
  return <AlertDialog open />;
}
`;
  const a = scanSource("packages/views/d.tsx", src(""));
  const all = [...a.dialogs, ...a.toasts].map((x) => x.id);
  assert.equal(new Set(all).size, all.length, `ids collide: ${all.join(", ")}`);
  const b = scanSource("packages/views/d.tsx", src("// pad\n".repeat(10)));
  assert.deepEqual([...b.dialogs, ...b.toasts].map((x) => x.id), all);
});

test("the real inventory has no id shared across kinds", () => {
  const inv = buildInventory(REPO);
  const all = [...inv.dialogs, ...inv.toasts, ...inv.decisionVariants].map((x) => x.id);
  assert.equal(new Set(all).size, all.length);
});

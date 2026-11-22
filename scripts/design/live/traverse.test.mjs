// CHE-6795 (CHE-6753 L6): the traversal against a real local doop server.
//
// The integration tests need a built doop checkout at the pinned SHA and a
// Chromium. They are skipped (and say so) unless DOOP_LOCAL_DIR points at that
// checkout. Run:
//   DOOP_LOCAL_DIR=/path/to/doop node --test scripts/design/live/traverse.test.mjs
import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { after, before, describe, test } from "node:test";

import { startLocalDoop } from "./doop-local.mjs";
import { launchBrowser, normalizeEdges, traverse } from "./traverse.mjs";

const doopDir = process.env.DOOP_LOCAL_DIR;
const ready = Boolean(doopDir && existsSync(`${doopDir}/dist/index.html`));

describe("normalizeEdges (pure)", () => {
  test("a click edge needs a selector; an unknown trigger is refused", () => {
    assert.throws(() => normalizeEdges([{ from: "a", on: "click", to: "b" }]), { code: "invalid-edge" });
    assert.throws(() => normalizeEdges([{ from: "a", on: "hover", selector: "#x", to: "b" }]), { code: "invalid-edge" });
    assert.throws(() => normalizeEdges([{ from: "a", on: "delay:abc", to: "b" }]), { code: "invalid-edge" });
    assert.throws(() => normalizeEdges([{ from: "a", on: "key:", to: "b" }]), { code: "invalid-edge" });
  });
  test("valid edges keep their trigger kind and payload", () => {
    const [click, key, delay] = normalizeEdges([
      { from: "a", on: "click", selector: "#go", to: "b" },
      { from: "a", on: "key:Enter", to: "b" },
      { from: "a", on: "delay:300", to: "b" },
    ]);
    assert.deepEqual([click.kind, key.kind, key.key, delay.kind, delay.ms], ["click", "key", "Enter", "delay", 300]);
  });
});

const doc = (body, attrs = "") =>
  `<!doctype html><html><head><style>*{margin:0}button{display:block;width:160px;height:60px}</style></head><body ${attrs}>${body}</body></html>`;
const arr = (items) => `data-doop-interactions='${JSON.stringify(items)}'`;

describe("traverse on a local doop server", { skip: ready ? false : "set DOOP_LOCAL_DIR to a built doop checkout at the pinned SHA" }, () => {
  let doop;
  let browser;

  before(async () => {
    doop = await startLocalDoop({ doopDir });
    browser = await launchBrowser();
  });
  after(async () => {
    await browser?.close();
    await doop?.stop();
  });

  // frames: { name: (ids) => html }. The first is the index (flow start).
  async function build(frames) {
    const canvas = await doop.createCanvas("traverse");
    const ids = {};
    for (const name of Object.keys(frames)) ids[name] = await doop.createFrame(canvas.id, name, "<p>x</p>");
    for (const [name, html] of Object.entries(frames)) await doop.setFrameHtml(ids[name], html(ids));
    await doop.mcp("set_prototype_start", { canvas_id: canvas.id, frame_id: ids[Object.keys(frames)[0]], agent_name: "L6" });
    return { canvasId: canvas.id, ids };
  }
  const run = async (built, edges, extra = {}) => {
    const context = await browser.newContext({ viewport: { width: 1000, height: 700 } });
    await context.addCookies(doop.cookies());
    try {
      const page = await context.newPage();
      return await traverse({ page, base: doop.base, canvasId: built.canvasId, indexFrameId: built.ids.idx, edges, settleMs: 1200, ...extra });
    } finally {
      await context.close();
    }
  };
  const protoProblems = async (built) => {
    const proto = await doop.mcp("get_prototype", { canvas_id: built.canvasId, agent_name: "L6" });
    return proto.interactions.filter((i) => i.problem);
  };
  const home = (ids) => ({ on: "key:Home", action: `navigate:${ids.idx}` });

  test("(a) an interaction missing its action: get_prototype has zero problems, the traversal gives edge-unreachable", async () => {
    const make = (action) => ({
      idx: (ids) => doc(`<button id="go" data-doop-on="click"${action ? ` data-doop-action="navigate:${ids.b}"` : ""}>Go</button>`, arr([])),
      b: (ids) => doc("<p>B</p>", arr([home(ids)])),
    });
    const red = await build(make(false));
    assert.deepEqual(await protoProblems(red), []);
    const edges = (b) => [{ id: "go", from: b.ids.idx, on: "click", selector: "#go", to: b.ids.b }];
    const bad = await run(red, edges(red));
    assert.equal(bad.ok, false);
    assert.deepEqual(bad.problems.map((p) => [p.code, p.edge]), [["edge-unreachable", "go"]]);

    const green = await build(make(true));
    const good = await run(green, edges(green));
    assert.deepEqual(good.problems, []);
    assert.equal(good.ok, true);
  });

  test("(b) a motion frame: the single pair loses Home, the interactions array keeps the delay and Home", async () => {
    const motion = (form) => ({
      idx: (ids) => doc(`<button id="go" data-doop-on="click" data-doop-action="navigate:${ids.m}">Go</button>`),
      m: (ids) =>
        form === "pair"
          ? // two single pairs on <body>: the second set of attributes is dropped by the HTML parser
            doc("<p>motion</p>", `data-doop-on="delay:400" data-doop-action="navigate:${ids.b}" data-doop-on="key:Home" data-doop-action="navigate:${ids.idx}"`)
          : doc("<p>motion</p>", arr([{ on: "delay:400", action: `navigate:${ids.b}` }, home(ids)])),
      b: (ids) => doc("<p>B</p>", arr([home(ids)])),
    });
    const edges = (b) => [
      { id: "to-motion", from: b.ids.idx, on: "click", selector: "#go", to: b.ids.m },
      { id: "delay", from: b.ids.m, on: "delay:400", to: b.ids.b },
    ];
    const pair = await build(motion("pair"));
    const bad = await run(pair, edges(pair));
    assert.equal(bad.ok, false);
    const lost = bad.problems.find((p) => p.on === "key:Home" && p.frame === pair.ids.m);
    assert.ok(lost, `Home on the motion frame must fail: ${JSON.stringify(bad.problems)}`);
    assert.equal(lost.code, "edge-unreachable");

    const array = await build(motion("array"));
    const good = await run(array, edges(array));
    assert.deepEqual(good.problems, []);
    assert.equal(good.ok, true);
    // the checks that ran: the delay edge, Home on the motion frame before the delay, and the index after twice the delay
    assert.ok(good.checked.some((c) => c.on === "delay:400" && c.reached === array.ids.b));
    assert.ok(good.checked.some((c) => c.on === "key:Home" && c.frame === array.ids.m && c.reached === array.ids.idx && c.heldMs >= 800));
  });

  test("(c) a key-step frame: the key reaches its target and Home reaches the index; a wrong target fails", async () => {
    const frames = {
      idx: (ids) => doc(`<button id="go" data-doop-on="click" data-doop-action="navigate:${ids.k}">Go</button>`),
      k: (ids) => doc("<p>key step</p>", arr([{ on: "key:Enter", action: `navigate:${ids.b}` }, home(ids)])),
      b: (ids) => doc("<p>B</p>", arr([home(ids)])),
      c: (ids) => doc("<p>C</p>", arr([home(ids)])),
    };
    const built = await build(frames);
    const edges = (to) => [
      { id: "to-key", from: built.ids.idx, on: "click", selector: "#go", to: built.ids.k },
      { id: "enter", from: built.ids.k, on: "key:Enter", to },
    ];
    const good = await run(built, edges(built.ids.b));
    assert.deepEqual(good.problems, []);
    assert.ok(good.checked.some((c) => c.on === "key:Enter" && c.reached === built.ids.b));
    assert.ok(good.checked.some((c) => c.on === "key:Home" && c.frame === built.ids.k && c.reached === built.ids.idx));

    const wrong = await run(built, edges(built.ids.c));
    assert.equal(wrong.ok, false);
    const p = wrong.problems.find((x) => x.edge === "enter");
    assert.equal(p.code, "edge-unreachable");
    assert.equal(p.expected, built.ids.c);
    assert.equal(p.actual, built.ids.b);
  });
});

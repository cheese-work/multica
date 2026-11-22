import assert from "node:assert/strict";
import test from "node:test";

import { writeInteractions } from "./interactions.mjs";

// Key map of the server the writer writes to: key -> frame id.
const IDS = { "route/b/default/web-1440/dark": "frB", "route/c/default/web-1440/dark": "frC", "route/a/default/web-1440/dark": "frA" };
const K = (s) => `route/${s}/default/web-1440/dark`;
const BOX = { width: 1440, height: 900 };
const IN = { x: 10, y: 10, width: 100, height: 30 };
const doc = (body) => `<!DOCTYPE html><html data-live-key="${K("a")}"><head><style>a{color:red}</style></head><body class="x">${body}</body></html>`;
const spec = (over = {}) => ({
  resolve: (key) => IDS[key],
  indexFrameId: "frIdx",
  frameBox: BOX,
  edges: [],
  boxes: {},
  keyEdges: [],
  delays: [],
  links: {},
  ...over,
});
const run = (html, over) => writeInteractions(html, spec(over));
const codes = (r) => r.errors.map((e) => e.code);
const bodyItems = (html) => {
  const m = /<body[^>]*\sdata-doop-interactions="([^"]*)"/.exec(html);
  return m ? JSON.parse(m[1].replaceAll("&quot;", '"').replaceAll("&amp;", "&")) : null;
};
const home = { on: "key:Home", action: "navigate:frIdx" };

test("data-live-edge becomes the pair on the same element", () => {
  const r = run(doc(`<button id="go" data-live-edge="e1">Go</button>`), {
    edges: [{ id: "e1", trigger: "click", to: K("b") }],
    boxes: { e1: IN },
  });
  assert.deepEqual(r.errors, []);
  assert.match(r.html, /<button id="go" data-doop-on="click" data-doop-action="navigate:frB">Go<\/button>/);
  assert.doesNotMatch(r.html, /data-live-edge/);
});

test("type and drag edges map to doop triggers on their own element", () => {
  const r = run(doc(`<input data-live-edge="t"><div data-live-edge="d">card</div>`), {
    edges: [
      { id: "t", trigger: "click", to: K("b") },
      { id: "d", trigger: "press", to: K("c") },
    ],
    boxes: { t: IN, d: IN },
  });
  assert.deepEqual(r.errors, []);
  assert.match(r.html, /<input data-doop-on="click" data-doop-action="navigate:frB">/);
  assert.match(r.html, /<div data-doop-on="press" data-doop-action="navigate:frC">card<\/div>/);
});

test("edge transition and duration are written as the pair's companions", () => {
  const r = run(doc(`<a data-live-edge="e1">x</a>`), {
    edges: [{ id: "e1", trigger: "click", to: K("b"), transition: "dissolve", duration: 150 }],
    boxes: { e1: IN },
  });
  assert.match(r.html, /data-doop-on="click" data-doop-action="navigate:frB" data-doop-transition="dissolve" data-doop-duration="150"/);
});

test("an edge without an action is edge-incomplete", () => {
  assert.deepEqual(codes(run(doc(`<button data-live-edge="e1">x</button>`), { edges: [{ id: "e1", trigger: "click" }], boxes: { e1: IN } })), ["edge-incomplete"]);
  assert.deepEqual(codes(run(doc(`<button data-live-edge="e1">x</button>`), { edges: [{ id: "e1", to: K("b") }], boxes: { e1: IN } })), ["edge-incomplete"]);
  // an element marked with an edge id the spec does not know has no action either
  assert.deepEqual(codes(run(doc(`<button data-live-edge="zz">x</button>`))), ["edge-incomplete"]);
});

test("an element with one half of the pair is edge-incomplete", () => {
  assert.deepEqual(codes(run(doc(`<button data-doop-on="click">x</button>`))), ["edge-incomplete"]);
  assert.deepEqual(codes(run(doc(`<button data-doop-action="navigate:frB">x</button>`))), ["edge-incomplete"]);
});

test("an array item without on or action is edge-incomplete", () => {
  const bad = (items) => `<div data-doop-interactions="${JSON.stringify(items).replaceAll('"', "&quot;")}">x</div>`;
  assert.deepEqual(codes(run(doc(bad([{ on: "click" }])))), ["edge-incomplete"]);
  assert.deepEqual(codes(run(doc(bad([{ action: "back" }])))), ["edge-incomplete"]);
  assert.deepEqual(codes(run(doc(`<div data-doop-interactions="not json">x</div>`))), ["edge-incomplete"]);
  // a key edge or a delay without a target gives an item without an action
  assert.deepEqual(codes(run(doc(""), { keyEdges: [{ key: "Enter" }] })), ["edge-incomplete"]);
  assert.deepEqual(codes(run(doc(""), { delays: [{ ms: 500 }] })), ["edge-incomplete"]);
});

test("a frame with a key edge gets one array with the key item and the Home item", () => {
  const r = run(doc("<p>hi</p>"), { keyEdges: [{ key: "Enter", to: K("b") }] });
  assert.deepEqual(r.errors, []);
  assert.equal(r.html.match(/data-doop-interactions=/g).length, 1);
  assert.doesNotMatch(r.html, /<body[^>]*data-doop-on/);
  assert.deepEqual(bodyItems(r.html), [{ on: "key:Enter", action: "navigate:frB" }, home]);
  assert.match(r.html, /<body class="x" data-doop-interactions=/);
});

test("a frame without edges still gets the array with the Home item only", () => {
  const r = run(doc(""));
  assert.deepEqual(bodyItems(r.html), [home]);
});

test("a motion frame's array holds its delay item and Home", () => {
  const r = run(doc(""), { delays: [{ ms: 450, to: K("c") }] });
  assert.deepEqual(r.errors, []);
  assert.deepEqual(bodyItems(r.html), [{ on: "delay:450", action: "navigate:frC" }, home]);
});

test("a key item and a delay item can share one frame's array", () => {
  const r = run(doc(""), { keyEdges: [{ key: "Escape", to: K("b") }], delays: [{ ms: 100, to: K("c") }] });
  assert.deepEqual(bodyItems(r.html).map((i) => i.on), ["key:Escape", "delay:100", "key:Home"]);
});

test("key:Home as an edge is edge-key-reserved", () => {
  assert.deepEqual(codes(run(doc(""), { keyEdges: [{ key: "Home", to: K("b") }] })), ["edge-key-reserved"]);
});

test("two body items with one trigger are edge-conflict", () => {
  assert.deepEqual(
    codes(run(doc(""), { keyEdges: [{ key: "Enter", to: K("b") }, { key: "Enter", to: K("c") }] })),
    ["edge-conflict"],
  );
  assert.deepEqual(
    codes(run(doc(""), { delays: [{ ms: 300, to: K("b") }, { ms: 300, to: K("c") }] })),
    ["edge-conflict"],
  );
});

test("an edge element outside the frame box is edge-box-outside", () => {
  const edges = [{ id: "e1", trigger: "click", to: K("b") }];
  const html = doc(`<button data-live-edge="e1">x</button>`);
  const outside = (box) => codes(run(html, { edges, boxes: { e1: box } }));
  assert.deepEqual(outside({ x: 10, y: 1000, width: 50, height: 20 }), ["edge-box-outside"]);
  assert.deepEqual(outside({ x: 1500, y: 10, width: 50, height: 20 }), ["edge-box-outside"]);
  assert.deepEqual(outside({ x: -80, y: 10, width: 50, height: 20 }), ["edge-box-outside"]);
  assert.deepEqual(outside({ x: 10, y: 10, width: 0, height: 0 }), ["edge-box-outside"]);
  assert.deepEqual(run(html, { edges, boxes: {} }).errors.map((e) => e.code), ["edge-box-outside"]);
  // a box that overlaps the frame box is still clickable; the full-height frame box is honoured
  assert.deepEqual(outside({ x: 1400, y: 10, width: 100, height: 20 }), []);
  assert.deepEqual(codes(run(html, { edges, boxes: { e1: { x: 0, y: 2000, width: 50, height: 20 } }, frameBox: { width: 1440, height: 4000 } })), []);
});

test("a same-app link to a static route resolves to its frame", () => {
  const r = run(doc(`<a href="#" data-live-href="/login">Sign in</a>`), { links: { "/login": K("b") } });
  assert.deepEqual(r.errors, []);
  assert.match(r.html, /<a href="#" data-live-href="\/login" data-doop-on="click" data-doop-action="navigate:frB">Sign in<\/a>/);
  assert.equal(r.unresolvedLinks, 0);
});

test("a link to an unseeded id stays # with no interaction and is counted", () => {
  const html = doc(`<a href="#" data-live-href="/acme/issues/zzz">x</a><a href="#" data-live-href="/acme/issues/yyy">y</a>`);
  const r = run(html, { links: { "/acme/issues/seed": K("b") } });
  assert.deepEqual(r.errors, []);
  assert.equal(r.unresolvedLinks, 2);
  assert.doesNotMatch(r.html.replace(/data-doop-interactions="[^"]*"/, ""), /data-doop-on|data-doop-action/);
  assert.match(r.html, /<a href="#" data-live-href="\/acme\/issues\/zzz">x<\/a>/);
});

test("an element with an edge keeps only the edge, also when it is a resolvable link", () => {
  const r = run(doc(`<a href="#" data-live-href="/login" data-live-edge="e1">Sign in</a>`), {
    links: { "/login": K("b") },
    edges: [{ id: "e1", trigger: "click", to: K("c") }],
    boxes: { e1: IN },
  });
  assert.deepEqual(r.errors, []);
  assert.match(r.html, /data-doop-on="click" data-doop-action="navigate:frC"/);
  assert.equal(r.html.match(/data-doop-on=/g).length, 1);
  assert.doesNotMatch(r.html, /navigate:frB/);
  assert.equal(r.unresolvedLinks, 0);
});

test("a target key missing from the key map is edge-target-missing", () => {
  const r = run(doc(`<button data-live-edge="e1">x</button>`), { edges: [{ id: "e1", trigger: "click", to: K("nope") }], boxes: { e1: IN } });
  assert.deepEqual(codes(r), ["edge-target-missing"]);
  assert.equal(r.html, null);
  assert.deepEqual(codes(run(doc(""), { keyEdges: [{ key: "Enter", to: K("nope") }] })), ["edge-target-missing"]);
  assert.deepEqual(codes(run(doc(`<a href="#" data-live-href="/x">x</a>`), { links: { "/x": K("nope") } })), ["edge-target-missing"]);
  assert.deepEqual(codes(run(doc(""), { indexFrameId: undefined })), ["edge-target-missing"]);
});

test("a listed edge with no element in the frame is edge-selector-missing", () => {
  assert.deepEqual(codes(run(doc("<p>x</p>"), { edges: [{ id: "e1", trigger: "click", to: K("b") }], boxes: { e1: IN } })), ["edge-selector-missing"]);
});

test("markup inside style, script and comments is not read as elements", () => {
  const html = `<html><head><style>/* <a data-live-href="/login"> */</style></head><body><!-- <button data-live-edge="e9"> --><p>ok</p></body></html>`;
  const r = run(html, { links: { "/login": K("b") } });
  assert.deepEqual(r.errors, []);
  assert.doesNotMatch(r.html, /navigate:frB/);
});

test("a document without a body is refused", () => {
  assert.deepEqual(codes(run("<html><p>x</p></html>")), ["body-missing"]);
});

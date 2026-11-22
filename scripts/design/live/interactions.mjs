/**
 * Interaction writer for DOM Live frames (CHE-6753 plan r6 A2, AM-1.7 pass 2).
 *
 * Pure function on an HTML string: takes the pass 1 document of one frame and returns the pass 2
 * document. No DOM, no I/O, no doop client. The caller supplies the key map of the server it writes
 * to (`resolve`), so the same logical edges give each server its own frame ids.
 *
 * Doop interaction attributes (shared/prototype.ts at doop 8d9fbcb4ba):
 *   element: data-doop-on + data-doop-action (+ data-doop-transition, data-doop-duration), one interaction;
 *   <body>:  one data-doop-interactions JSON array, never the single pair.
 *
 * Error codes: edge-incomplete, edge-key-reserved, edge-conflict, edge-box-outside,
 * edge-selector-missing, edge-target-missing, body-missing.
 */

const FRAME_ID = /^[\w-]{1,64}$/;
const RAW_TEXT = new Set(["script", "style", "textarea", "title"]);
const ATTR = /\s*([^\s"'<>/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>`]+)))?/y;

const decode = (v) =>
  v.replace(/&(amp|quot|lt|gt|#39|apos);/g, (_, c) => ({ amp: "&", quot: '"', lt: "<", gt: ">", "#39": "'", apos: "'" })[c]);
const escapeAttr = (v) => String(v).replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;");

// Start tags outside comments and raw-text elements, each with its attributes.
function scanTags(html) {
  const tags = [];
  let at = 0;
  while (at < html.length) {
    const lt = html.indexOf("<", at);
    if (lt === -1) break;
    if (html.startsWith("<!--", lt)) {
      const end = html.indexOf("-->", lt + 4);
      at = end === -1 ? html.length : end + 3;
      continue;
    }
    const nameMatch = /^<([A-Za-z][^\s/>]*)/.exec(html.slice(lt, lt + 64));
    if (!nameMatch) {
      at = lt + 1;
      continue;
    }
    const name = nameMatch[1].toLowerCase();
    const attrs = [];
    let p = lt + nameMatch[0].length;
    let selfClose = false;
    while (p < html.length) {
      while (p < html.length && /\s/.test(html[p])) p++;
      if (html[p] === ">") break;
      if (html[p] === "/") {
        selfClose = html[p + 1] === ">";
        p++;
        continue;
      }
      ATTR.lastIndex = p;
      const m = ATTR.exec(html);
      if (!m || !m[0].length) {
        p++;
        continue;
      }
      attrs.push({ name: m[1].toLowerCase(), raw: m[0].trim(), value: m[2] !== undefined ? decode(m[2]) : m[3] !== undefined ? decode(m[3]) : m[4] !== undefined ? m[4] : null });
      p = ATTR.lastIndex;
    }
    const end = Math.min(p + 1, html.length);
    tags.push({ name, start: lt, end, attrs, selfClose });
    at = end;
    if (RAW_TEXT.has(name) && !selfClose) {
      const close = html.toLowerCase().indexOf(`</${name}`, at);
      at = close === -1 ? html.length : close;
    }
  }
  return tags;
}

const get = (tag, name) => tag.attrs.find((a) => a.name === name);
const render = (tag, attrs) => `<${tag.name}${attrs.length ? " " + attrs.map((a) => a.raw).join(" ") : ""}${tag.selfClose ? " /" : ""}>`;
const withAttr = (attrs, name, value) => [...attrs.filter((a) => a.name !== name), { name, raw: `${name}="${escapeAttr(value)}"` }];

/**
 * @param {string} html pass 1 document of one frame (no data-doop-* attribute)
 * @param {object} spec
 * @param {(key: string) => string | undefined} spec.resolve key map of the target server: key -> frame id
 * @param {string} spec.indexFrameId frame id of the index frame (target of key:Home)
 * @param {{width: number, height: number}} spec.frameBox frame box, origin 0,0
 * @param {{id: string, trigger?: "click"|"press", to?: string, transition?: string, duration?: number}[]} spec.edges
 *   edges that start on this frame; trigger: click for click and type edges, press for drag edges
 * @param {Record<string, {x: number, y: number, width: number, height: number}>} spec.boxes measured box per edge id
 * @param {{key: string, to?: string}[]} spec.keyEdges `key` edges of this frame (a <body> item each)
 * @param {{ms: number, to?: string}[]} spec.delays `delay` items of a motion frame
 * @param {Record<string, string>} spec.links same-app path -> key: static route paths and seeded dynamic paths
 * @returns {{html: string | null, errors: {code: string, detail: string}[], unresolvedLinks: number}}
 *   html is null when any error is returned: the run writes nothing.
 */
export function writeInteractions(html, spec) {
  const errors = [];
  const err = (code, detail) => errors.push({ code, detail });
  const { resolve, indexFrameId, frameBox, edges = [], boxes = {}, keyEdges = [], delays = [], links = {} } = spec;
  const frameId = (key, what) => {
    const id = typeof key === "string" ? resolve(key) : undefined;
    if (typeof id === "string" && FRAME_ID.test(id)) return id;
    err("edge-target-missing", `${what}: no frame id for ${key === undefined ? "(no target)" : key}`);
    return undefined;
  };
  const edgeById = new Map(edges.map((e) => [e.id, e]));
  const tags = scanTags(html);
  const edits = []; // [start, end, replacement]
  const seenEdges = new Set();
  let unresolvedLinks = 0;
  let body;

  for (const tag of tags) {
    if (tag.name === "body" && !body) body = tag;
    const on = get(tag, "data-doop-on");
    const action = get(tag, "data-doop-action");
    if (!on !== !action) err("edge-incomplete", `<${tag.name}> has ${on ? "data-doop-on" : "data-doop-action"} without its pair`);
    const arr = get(tag, "data-doop-interactions");
    if (arr) {
      let items;
      try {
        items = JSON.parse(arr.value ?? "");
      } catch {
        items = null;
      }
      if (!Array.isArray(items)) err("edge-incomplete", `<${tag.name}> data-doop-interactions is not a JSON array`);
      else items.forEach((it, i) => (!it?.on || !it?.action) && err("edge-incomplete", `<${tag.name}> array item ${i} lacks on or action`));
    }

    const edgeAttr = get(tag, "data-live-edge");
    const href = get(tag, "data-live-href");
    if (edgeAttr) {
      const edge = edgeById.get(edgeAttr.value);
      seenEdges.add(edgeAttr.value);
      const id = edge?.to ? frameId(edge.to, `edge ${edgeAttr.value}`) : undefined;
      if (!edge || !edge.trigger || !edge.to) {
        err("edge-incomplete", `edge ${edgeAttr.value} has no ${!edge ? "definition" : !edge.trigger ? "trigger" : "action"}`);
        continue;
      }
      if (id) {
        let attrs = tag.attrs.filter((a) => a.name !== "data-live-edge");
        attrs = withAttr(withAttr(attrs, "data-doop-on", edge.trigger), "data-doop-action", `navigate:${id}`);
        if (edge.transition) attrs = withAttr(attrs, "data-doop-transition", edge.transition);
        if (edge.duration !== undefined) attrs = withAttr(attrs, "data-doop-duration", String(edge.duration));
        edits.push([tag.start, tag.end, render(tag, attrs)]);
      }
    } else if (tag.name === "a" && href) {
      const key = Object.hasOwn(links, href.value) ? links[href.value] : undefined;
      if (key === undefined) {
        unresolvedLinks++;
        continue;
      }
      const id = frameId(key, `link ${href.value}`);
      if (id) edits.push([tag.start, tag.end, render(tag, withAttr(withAttr(tag.attrs, "data-doop-on", "click"), "data-doop-action", `navigate:${id}`))]);
    }
  }

  // Every listed edge needs its element, and its box must touch the frame box.
  for (const edge of edges) {
    if (!seenEdges.has(edge.id)) err("edge-selector-missing", `edge ${edge.id} has no element in the frame`);
    const b = boxes[edge.id];
    const overlaps = b && b.width > 0 && b.height > 0 && b.x < frameBox.width && b.x + b.width > 0 && b.y < frameBox.height && b.y + b.height > 0;
    if (!overlaps) err("edge-box-outside", `edge ${edge.id} ${b ? "is outside" : "has no measured box in"} the frame box`);
  }

  // <body>: one array. Key items, delay items, then the reserved Home item.
  const items = [];
  const triggers = new Set();
  const item = (on, to, what) => {
    if (triggers.has(on)) err("edge-conflict", `${what}: two body items with trigger ${on}`);
    triggers.add(on);
    const id = to === undefined ? undefined : frameId(to, what);
    if (to === undefined) err("edge-incomplete", `${what}: ${on} item has no action`);
    if (id) items.push({ on, action: `navigate:${id}` });
  };
  for (const k of keyEdges) {
    if (k.key === "Home") err("edge-key-reserved", "key:Home is reserved for the index link");
    else item(`key:${k.key}`, k.to, `key edge ${k.key}`);
  }
  for (const d of delays) item(`delay:${d.ms}`, d.to, `delay ${d.ms}`);
  if (typeof indexFrameId === "string" && FRAME_ID.test(indexFrameId)) items.push({ on: "key:Home", action: `navigate:${indexFrameId}` });
  else err("edge-target-missing", "key:Home: no index frame id");

  if (!body) err("body-missing", "the document has no <body>");
  else {
    const attrs = body.attrs.filter((a) => !a.name.startsWith("data-doop-"));
    edits.push([body.start, body.end, render(body, withAttr(attrs, "data-doop-interactions", JSON.stringify(items)))]);
  }

  if (errors.length) return { html: null, errors, unresolvedLinks };
  let out = "";
  let from = 0;
  for (const [s, e, r] of edits.sort((a, b) => a[0] - b[0])) {
    out += html.slice(from, s) + r;
    from = e;
  }
  return { html: out + html.slice(from), errors, unresolvedLinks };
}

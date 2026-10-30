#!/usr/bin/env node
// Pre-drain admission check for the feature-flag file the Jev overlay mounts
// (CHE-1854). The backend refuses to start on a malformed file, and both
// colours mount the same file, so a bad file found after the incumbent is
// drained cannot be recovered by restarting it. C00 has no YAML library, so
// this admits only a documented subset of the backend schema
// (server/pkg/featureflag/config.go) and refuses everything else, including
// some files the backend would accept (unknown fields, anchors, YAML 1.1
// booleans, numeric list items). The corpus in deploy/cd/fixtures/flags is
// classified by the real Go loader in server/internal/featureflags.
//
// Admitted shape (2-space indents, no tabs):
//   <flag>:                      # unique, plain name
//     default: true|false
//     variant|allow_by|deny_by: <string>
//     allow|deny: ["a", b]       # or a block list of `- <string>` at 4 spaces
//     percent:
//       percent: <integer 0..9223372036854775807, no leading zeros>
//       by: <string>
// Strings are "double-quoted" (no backslash), 'single-quoted' (no inner quote)
// or plain tokens that are not booleans, null, numbers or dates.
//
// Usage: flagfile-check.mjs <file>   (exit 0 admitted; exit 1 refused)
import { readFileSync } from "node:fs";

const file = process.argv[2];
let line = 0;
const refuse = (why) => {
  console.error(`flag file ${file}: line ${line}: ${why}`);
  process.exit(1);
};

const RESERVED = /^(true|false|null|yes|no|on|off|y|n|~)$/i;
const NUMERIC = /^[-+]?(\d[\d_]*)(\.\d*)?([eE][-+]?\d+)?$|^0[xob][0-9a-fA-F_]+$|^\d{4}-\d{2}-\d{2}/;
const PLAIN = /^[A-Za-z0-9_][A-Za-z0-9_.\/:@+-]*$/;

// scalar parses one string scalar at the start of `text` and returns
// [value, rest]; rest is what follows the token.
function scalar(text) {
  const quote = text[0];
  if (quote === '"' || quote === "'") {
    const end = text.indexOf(quote, 1);
    if (end < 0) refuse("unterminated quoted string");
    const body = text.slice(1, end);
    if (quote === '"' && body.includes("\\")) refuse("backslash escapes are not admitted");
    if (quote === "'" && text[end + 1] === "'") refuse("escaped single quote is not admitted");
    return [body, text.slice(end + 1)];
  }
  const m = /^[^\s,\]#]+/.exec(text);
  if (!m) refuse("expected a string value");
  const token = m[0];
  if (!PLAIN.test(token) || RESERVED.test(token) || NUMERIC.test(token)) {
    refuse(`plain value is not an admitted string (quote it): ${token.length > 40 ? "too long" : token}`);
  }
  return [token, text.slice(token.length)];
}

const tail = (rest) => {
  if (!/^\s*(#.*)?$/.test(rest)) refuse("unexpected text after value");
};

function flowList(text) {
  if (text[0] !== "[") refuse("expected a [list]");
  let rest = text.slice(1).trimStart();
  if (rest[0] === "]") return tail(rest.slice(1));
  for (;;) {
    rest = scalar(rest)[1].trimStart();
    if (rest[0] === ",") { rest = rest.slice(1).trimStart(); continue; }
    if (rest[0] === "]") return tail(rest.slice(1));
    refuse("unclosed or malformed [list]");
  }
}

const SCALAR_FIELDS = new Set(["variant", "allow_by", "deny_by"]);
const LIST_FIELDS = new Set(["allow", "deny"]);

const lines = readFileSync(file, "utf8").split("\n");
const flags = new Set();
const defaults = new Map();
const percentages = new Map();
let flagName;
let fields = null; // fields seen under the current flag
let block = null; // "list" | "percent" while a nested block is open
let percentKeys = null;

for (const raw of lines) {
  line += 1;
  if (/^ *\t/.test(raw)) refuse("tab indentation");
  const text = raw.replace(/\s+$/, "");
  if (text.trim() === "" || text.trim().startsWith("#")) continue;
  const indent = text.length - text.trimStart().length;
  const body = text.trimStart();

  if (indent === 0) {
    const m = /^([A-Za-z0-9_][A-Za-z0-9_.-]*):\s*(#.*)?$/.exec(body);
    if (!m) refuse("top level must be `<flag>:`");
    if (RESERVED.test(m[1]) || NUMERIC.test(m[1])) refuse("flag name must not look like a boolean, null or number");
    if (flags.has(m[1])) refuse(`duplicate flag ${m[1]}`);
    flags.add(m[1]);
    flagName = m[1];
    defaults.set(flagName, false);
    fields = new Set();
    block = null;
  } else if (indent === 2) {
    if (!fields) refuse("field outside a flag");
    const m = /^([a-z_]+):(.*)$/.exec(body);
    if (!m) refuse("expected `<field>: <value>`");
    const [, name, rest] = m;
    if (fields.has(name)) refuse(`duplicate field ${name}`);
    fields.add(name);
    block = null;
    const value = rest.trimStart();
    if (name === "default") {
      const v = /^(true|false)(?=\s|$)/.exec(value)?.[1];
      if (v === undefined) refuse("default must be true or false");
      tail(value.slice(v.length));
      defaults.set(flagName, v === "true");
    } else if (SCALAR_FIELDS.has(name)) {
      tail(scalar(value)[1]);
    } else if (LIST_FIELDS.has(name)) {
      if (value === "" || value.startsWith("#")) block = "list";
      else flowList(value);
    } else if (name === "percent") {
      if (!(value === "" || value.startsWith("#"))) refuse("percent must be a nested block");
      block = "percent";
      percentages.set(flagName, 0n);
      percentKeys = new Set();
    } else {
      refuse(`unknown field ${name}`);
    }
  } else if (indent === 4) {
    if (block === "list") {
      const m = /^-\s+(.*)$/.exec(body);
      if (!m) refuse("expected `- <string>`");
      tail(scalar(m[1])[1]);
    } else if (block === "percent") {
      const m = /^(percent|by):\s*(.*)$/.exec(body);
      if (!m) refuse("percent admits only `percent:` and `by:`");
      if (percentKeys.has(m[1])) refuse(`duplicate field ${m[1]}`);
      percentKeys.add(m[1]);
      if (m[1] === "percent") {
        // Canonical decimal only: the loader reads a leading zero as octal (or
        // wraps a long zero-padded value), and values past Go's int64 fail to load.
        const n = /^(0|[1-9]\d*)(?=\s|$)/.exec(m[2]);
        if (!n) refuse("percent must be a plain decimal integer without leading zeros");
        if (BigInt(n[1]) > 9223372036854775807n) refuse("percent exceeds the 64-bit integer range");
        tail(m[2].slice(n[1].length));
        percentages.set(flagName, BigInt(n[1]));
      } else {
        tail(scalar(m[2])[1]);
      }
    } else {
      refuse("unexpected nested line");
    }
  } else {
    refuse("indentation must be 0, 2 or 4 spaces");
  }
}

if (process.argv[3] === "--config-stdin") {
  let config;
  try {
    config = JSON.parse(readFileSync(0, "utf8"));
  } catch {
    refuse("frontend flag verification: /api/config is not valid JSON");
  }
  if (!config || typeof config.feature_flags !== "object" || !config.feature_flags || Array.isArray(config.feature_flags)) {
    refuse("frontend flag verification: /api/config has no feature_flags object");
  }
  for (const key of ["project_workflows_v1", "plugins_v1", "composio_mcp_apps", "billing_workspace_subscriptions"]) {
    if (!flags.has(key)) continue;
    let expected = defaults.get(key);
    if (percentages.has(key)) {
      let hash = 2166136261;
      for (const byte of Buffer.from(`${key}\0`)) hash = Math.imul(hash ^ byte, 16777619) >>> 0;
      expected = BigInt(hash % 100) < percentages.get(key);
    }
    if (config.feature_flags[key] !== expected) {
      refuse(`frontend flag mismatch: ${key} (expected ${expected})`);
    }
  }
}

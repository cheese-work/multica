#!/usr/bin/env node
//
// C00 migration gate: the pure decision tool (CHE-9443 plan r8, approach items 1 to 6; CHE-9586).
//
// It decides whether a candidate tree may reach a C00 deploy: every migration that the live
// ledger has not applied must be "additive", or the owner must have approved the exact
// inventory and tree. It never deploys, never talks to a network and never runs git. All
// inputs are files or arguments, so it runs from any checked-out tree (the no-deploy mode).
//
// Commands:
//   versions      --tree-root DIR
//   inventory     --tree-root DIR --ledger FILE --ledger-sha256 HEX
//   approval-line --inventory sha256:HEX --tree HEX40
//   decide        --tree-root DIR --ledger FILE --ledger-sha256 HEX --tree-sha HEX40
//                 --source-sha SHA --checkout-sha SHA [--comments FILE]
//
// Exit codes: 0 admit or success, 1 refuse or failure (fail closed), 2 usage error.
//
// The classifier is a closed grammar: a pair is "additive" only when the whole up file matches
// the allowed forms U1 to U7 and the down file matches the allowed down forms. Anything the
// grammar does not name needs approval. No reviewed table and no agent text lowers a class.

import { createHash } from "node:crypto";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const CLASSIFIER_VERSION = "c00-migration-classifier/v1";
export const CAPABILITY_LINE = "migration-decision v1";
// Cheese's numeric GitHub user ID. Agents post as 282619718. A login is never an identity.
export const CHEESE_USER_ID = 129714106;
export const MAX_COMMENT_PAGES = 10;

const sha256 = (data) => createHash("sha256").update(data).digest("hex");
const HEX40 = /^[0-9a-f]{40}$/;
const INVENTORY_RE = /^sha256:[0-9a-f]{64}$/;

// ---------------------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------------------

class Opaque extends Error {
  constructor(kind) {
    super(kind);
    this.kind = kind;
  }
}
class Reject extends Error {
  constructor(reason) {
    super(reason);
    this.reason = reason;
  }
}

// Words that PostgreSQL does not accept as an unquoted column, table or index name.
const RESERVED = new Set(
  (
    "ALL ANALYSE ANALYZE AND ANY ARRAY AS ASC ASYMMETRIC BOTH CASE CAST CHECK COLLATE COLUMN CONSTRAINT " +
    "CREATE CURRENT_CATALOG CURRENT_DATE CURRENT_ROLE CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER DEFAULT " +
    "DEFERRABLE DESC DISTINCT DO ELSE END EXCEPT FALSE FETCH FOR FOREIGN FROM GRANT GROUP HAVING IN " +
    "INITIALLY INTERSECT INTO LATERAL LEADING LIKE LIMIT LOCALTIME LOCALTIMESTAMP NOT NULL OFFSET ON ONLY OR " +
    "ORDER PLACING PRIMARY REFERENCES RETURNING SELECT SESSION_USER SOME SYMMETRIC SYSTEM_USER TABLE THEN TO " +
    "TRAILING TRUE UNION UNIQUE USER USING VARIADIC WHEN WHERE WINDOW WITH"
  ).split(" "),
);

const OP_CHARS = "+-*/<>=~!@#%^&|?";
const SPACE = " \t\n\r\f\v";
const isIdStart = (c) => /[A-Za-z_]/.test(c) || c.charCodeAt(0) >= 0x80;
const isIdPart = (c) => /[A-Za-z0-9_$]/.test(c) || c.charCodeAt(0) >= 0x80;
const isDigit = (c) => c >= "0" && c <= "9";

// Returns an array of statements; each statement is an array of tokens.
// Token kinds: id (unquoted word, with .u upper case), qid (quoted identifier), str, num,
// op, p (punctuation), dc (::). Comments are dropped, nested block comments included.
// Throws Opaque for what the grammar cannot read: dollar quotes, E'' strings, unknown
// characters, unterminated strings or comments, unbalanced parentheses.
export function tokenize(sql) {
  const statements = [];
  let toks = [];
  let depth = 0;
  const n = sql.length;
  let i = 0;
  const prev = () => toks[toks.length - 1];
  while (i < n) {
    const c = sql[i];
    if (SPACE.includes(c)) {
      i++;
      continue;
    }
    if (c === "-" && sql[i + 1] === "-") {
      while (i < n && sql[i] !== "\n") i++;
      continue;
    }
    if (c === "/" && sql[i + 1] === "*") {
      let level = 1;
      i += 2;
      while (i < n && level > 0) {
        if (sql[i] === "/" && sql[i + 1] === "*") {
          level++;
          i += 2;
        } else if (sql[i] === "*" && sql[i + 1] === "/") {
          level--;
          i += 2;
        } else {
          i++;
        }
      }
      if (level > 0) throw new Opaque("unterminated-comment");
      continue;
    }
    if (c === "'") {
      let v = "";
      i++;
      for (;;) {
        if (i >= n) throw new Opaque("unterminated-string");
        if (sql[i] === "'") {
          if (sql[i + 1] === "'") {
            v += "'";
            i += 2;
            continue;
          }
          i++;
          break;
        }
        v += sql[i++];
      }
      toks.push({ k: "str", v });
      continue;
    }
    if (c === '"') {
      let v = "";
      i++;
      for (;;) {
        if (i >= n) throw new Opaque("unterminated-identifier");
        if (sql[i] === '"') {
          if (sql[i + 1] === '"') {
            v += '"';
            i += 2;
            continue;
          }
          i++;
          break;
        }
        v += sql[i++];
      }
      if (v === "") throw new Opaque("empty-identifier");
      toks.push({ k: "qid", v });
      continue;
    }
    if (c === "$") throw new Opaque("dollar");
    if (isIdStart(c)) {
      let j = i + 1;
      while (j < n && isIdPart(sql[j])) j++;
      const v = sql.slice(i, j);
      const lower = v.toLowerCase();
      if (lower === "e" && sql[j] === "'") throw new Opaque("escape-string");
      if (lower === "u" && sql[j] === "&" && (sql[j + 1] === "'" || sql[j + 1] === '"')) throw new Opaque("unicode-escape");
      toks.push({ k: "id", v, u: v.toUpperCase() });
      i = j;
      continue;
    }
    const p = prev();
    const afterName = p && (p.k === "id" || p.k === "qid" || (p.k === "p" && (p.v === ")" || p.v === "]")));
    if (isDigit(c) || (c === "." && isDigit(sql[i + 1] ?? "") && !afterName)) {
      const m = /(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?/y;
      m.lastIndex = i;
      const r = m.exec(sql);
      toks.push({ k: "num", v: r[0] });
      i += r[0].length;
      continue;
    }
    if (c === ":") {
      if (sql[i + 1] !== ":") throw new Opaque("unknown-character");
      toks.push({ k: "dc", v: "::" });
      i += 2;
      continue;
    }
    if (c === "(") {
      depth++;
      toks.push({ k: "p", v: c });
      i++;
      continue;
    }
    if (c === ")") {
      if (depth === 0) throw new Opaque("unbalanced-parenthesis");
      depth--;
      toks.push({ k: "p", v: c });
      i++;
      continue;
    }
    if (c === ";") {
      i++;
      if (depth === 0) {
        if (toks.length > 0) statements.push(toks);
        toks = [];
      } else {
        toks.push({ k: "p", v: c });
      }
      continue;
    }
    if (c === "," || c === "." || c === "[" || c === "]") {
      toks.push({ k: "p", v: c });
      i++;
      continue;
    }
    if (OP_CHARS.includes(c)) {
      let j = i;
      while (j < n && OP_CHARS.includes(sql[j])) {
        if ((sql[j] === "-" && sql[j + 1] === "-") || (sql[j] === "/" && sql[j + 1] === "*")) break;
        j++;
      }
      toks.push({ k: "op", v: sql.slice(i, j) });
      i = j;
      continue;
    }
    throw new Opaque("unknown-character");
  }
  if (depth !== 0) throw new Opaque("unbalanced-parenthesis");
  if (toks.length > 0) statements.push(toks);
  return statements;
}

// ---------------------------------------------------------------------------------------
// Closed-grammar statement parser
// ---------------------------------------------------------------------------------------

const UNSUP = "up:unsupported-statement";
const INDEX_EXPR = "up:index-expression";
const INDEX_PRED = "up:index-predicate";
const TYPE_MOD = "up:type-modifier";
const TYPE_WORDS = new Set(["PRECISION", "VARYING", "WITH", "WITHOUT", "TIME", "ZONE"]);
const SERIAL = new Set(["serial", "serial2", "serial4", "serial8", "smallserial", "bigserial"]);
const CMP_OPS = new Set(["=", "<>", "!=", "<", ">", "<=", ">="]);
const FK_ACTIONS = [["CASCADE"], ["RESTRICT"], ["SET", "NULL"], ["SET", "DEFAULT"], ["NO", "ACTION"]];

class Cursor {
  constructor(tokens) {
    this.t = tokens;
    this.i = 0;
  }
  peek(o = 0) {
    return this.t[this.i + o];
  }
  next() {
    return this.t[this.i++];
  }
  done() {
    return this.i >= this.t.length;
  }
  kw(w, o = 0) {
    const x = this.peek(o);
    return Boolean(x) && x.k === "id" && x.u === w;
  }
  isP(v, o = 0) {
    const x = this.peek(o);
    return Boolean(x) && x.k === "p" && x.v === v;
  }
  eatKw(w) {
    if (this.kw(w)) {
      this.i++;
      return true;
    }
    return false;
  }
  eatP(v) {
    if (this.isP(v)) {
      this.i++;
      return true;
    }
    return false;
  }
  eatSeq(words) {
    if (words.every((w, o) => this.kw(w, o))) {
      this.i += words.length;
      return true;
    }
    return false;
  }
  need(ok, reason = UNSUP) {
    if (!ok) throw new Reject(reason);
  }
  needKw(w, reason = UNSUP) {
    this.need(this.eatKw(w), reason);
  }
  needP(v, reason = UNSUP) {
    this.need(this.eatP(v), reason);
  }
  needEnd(reason = UNSUP) {
    this.need(this.done(), reason);
  }
  // An unquoted name that is not a reserved word, or a quoted name. Returns its value.
  ident(reason = UNSUP) {
    const x = this.peek();
    this.need(Boolean(x) && (x.k === "qid" || (x.k === "id" && !RESERVED.has(x.u))), reason);
    this.i++;
    return x.k === "qid" ? x.v : x.v.toLowerCase();
  }
  qname(reason = UNSUP) {
    const parts = [this.ident(reason)];
    while (this.isP(".")) {
      this.i++;
      parts.push(this.ident(reason));
    }
    return parts;
  }
  cols(reason = UNSUP) {
    this.needP("(", reason);
    do this.ident(reason);
    while (this.eatP(","));
    this.needP(")", reason);
  }
  number(reason = UNSUP) {
    const x = this.next();
    this.need(Boolean(x) && x.k === "num", reason);
  }
  // <type> = <qname> plus words from TYPE_WORDS, an optional (n[, n]) and optional [] suffixes.
  type() {
    const q = this.qname(UNSUP);
    let modifier = false;
    for (;;) {
      const x = this.peek();
      if (x && x.k === "id" && TYPE_WORDS.has(x.u)) {
        this.i++;
      } else if (this.isP("(") && !modifier) {
        modifier = true;
        this.i++;
        this.number(TYPE_MOD);
        if (this.eatP(",")) this.number(TYPE_MOD);
        this.needP(")", TYPE_MOD);
      } else {
        break;
      }
    }
    while (this.isP("[")) {
      this.i++;
      this.needP("]");
    }
    return q;
  }
  // <literal> = string, number, TRUE, FALSE, NULL, optionally ::<type>.
  literal(reason = UNSUP) {
    const x = this.next();
    this.need(
      Boolean(x) && (x.k === "str" || x.k === "num" || (x.k === "id" && (x.u === "TRUE" || x.u === "FALSE" || x.u === "NULL"))),
      reason,
    );
    if (this.peek() && this.peek().k === "dc") {
      this.i++;
      this.type();
    }
  }
}

// U2 predicate: AND / OR / NOT / parentheses over three atoms only.
function parsePredicate(c) {
  const orExpr = () => {
    andExpr();
    while (c.eatKw("OR")) andExpr();
  };
  const andExpr = () => {
    notExpr();
    while (c.eatKw("AND")) notExpr();
  };
  const notExpr = () => {
    if (c.eatKw("NOT")) notExpr();
    else primary();
  };
  const primary = () => {
    if (c.eatP("(")) {
      orExpr();
      c.needP(")");
      return;
    }
    c.ident();
    if (c.eatKw("IS")) {
      c.eatKw("NOT");
      const x = c.next();
      c.need(Boolean(x) && x.k === "id" && (x.u === "NULL" || x.u === "TRUE" || x.u === "FALSE"));
      return;
    }
    const x = c.peek();
    if (x && x.k === "op" && CMP_OPS.has(x.v)) {
      c.i++;
      c.literal();
      return;
    }
    const negated = c.kw("NOT") && c.kw("IN", 1);
    if (negated || c.kw("IN")) {
      if (negated) c.i++;
      c.i++;
      c.needP("(");
      do c.literal();
      while (c.eatP(","));
      c.needP(")");
    }
    // otherwise a bare column
  };
  try {
    orExpr();
    c.needEnd();
  } catch (e) {
    if (e instanceof Reject) throw new Reject(INDEX_PRED);
    throw e;
  }
}

function parseIndex(c) {
  c.eatKw("UNIQUE");
  c.needKw("INDEX");
  c.eatKw("CONCURRENTLY");
  if (c.eatKw("IF")) {
    c.needKw("NOT");
    c.needKw("EXISTS");
  }
  const name = c.ident();
  c.needKw("ON");
  c.eatKw("ONLY");
  c.qname();
  if (c.eatKw("USING")) c.ident();
  c.needP("(");
  do {
    if (c.isP("(")) throw new Reject(INDEX_EXPR);
    c.ident();
    if (c.isP("(")) throw new Reject(INDEX_EXPR);
    if (c.eatKw("COLLATE")) c.qname();
    const x = c.peek();
    if (x && (x.k === "qid" || (x.k === "id" && !RESERVED.has(x.u) && x.u !== "NULLS"))) c.qname();
    if (!c.eatKw("ASC")) c.eatKw("DESC");
    if (c.eatKw("NULLS")) c.need(c.eatKw("FIRST") || c.eatKw("LAST"));
  } while (c.eatP(","));
  c.needP(")");
  if (c.kw("INCLUDE")) {
    c.i++;
    c.cols();
  }
  if (c.eatKw("WHERE")) parsePredicate(c);
  c.needEnd("up:index-tail");
  return { indexName: name };
}

function parseForeignKey(c) {
  c.qname();
  if (c.isP("(")) c.cols();
  while (c.eatKw("ON")) {
    c.need(c.eatKw("DELETE") || c.eatKw("UPDATE"));
    c.need(FK_ACTIONS.some((a) => c.eatSeq(a)));
  }
}

function parseTableDefault(c) {
  const x = c.peek();
  if (x && x.k === "id" && (x.u === "CURRENT_TIMESTAMP" || x.u === "CURRENT_DATE")) {
    c.i++;
    return;
  }
  if (x && x.k === "id" && (x.v.toLowerCase() === "now" || x.v.toLowerCase() === "gen_random_uuid") && c.isP("(", 1) && c.isP(")", 2)) {
    c.i += 3;
    return;
  }
  try {
    c.literal();
  } catch (e) {
    if (e instanceof Reject) throw new Reject("up:table-body:default");
    throw e;
  }
}

function parseTable(c) {
  c.needKw("TABLE");
  if (c.eatKw("IF")) {
    c.needKw("NOT");
    c.needKw("EXISTS");
  }
  c.qname();
  c.needP("(", "up:table-body:unsupported");
  const body = (reason) => (cond) => c.need(cond, reason);
  do {
    const first = c.peek();
    const word = first && first.k === "id" ? first.u : "";
    if (word === "CHECK" || word === "EXCLUDE" || word === "LIKE") throw new Reject(`up:table-body:${word.toLowerCase()}`);
    if (word === "CONSTRAINT" || word === "PRIMARY" || word === "UNIQUE" || word === "FOREIGN") {
      if (c.eatKw("CONSTRAINT")) c.ident("up:table-body:constraint");
      const x = c.peek();
      const u = x && x.k === "id" ? x.u : "";
      if (u === "PRIMARY") {
        c.i++;
        body("up:table-body:primary-key")(c.eatKw("KEY"));
        c.cols("up:table-body:primary-key");
      } else if (u === "UNIQUE") {
        c.i++;
        c.cols("up:table-body:unique");
      } else if (u === "FOREIGN") {
        c.i++;
        body("up:table-body:foreign-key")(c.eatKw("KEY"));
        c.cols("up:table-body:foreign-key");
        body("up:table-body:foreign-key")(c.eatKw("REFERENCES"));
        try {
          parseForeignKey(c);
        } catch (e) {
          throw e instanceof Reject ? new Reject("up:table-body:foreign-key") : e;
        }
      } else {
        throw new Reject(`up:table-body:${(u || "constraint").toLowerCase()}`);
      }
      continue;
    }
    // column definition
    c.ident("up:table-body:column");
    try {
      c.type();
    } catch (e) {
      throw e instanceof Reject && e.reason !== TYPE_MOD ? new Reject("up:table-body:column-type") : e;
    }
    for (;;) {
      if (c.isP(",") || c.isP(")")) break;
      if (c.eatKw("CONSTRAINT")) c.ident("up:table-body:constraint");
      const x = c.peek();
      const u = x && x.k === "id" ? x.u : "";
      if (u === "NOT" && c.kw("NULL", 1)) c.i += 2;
      else if (u === "NULL") c.i++;
      else if (u === "PRIMARY" && c.kw("KEY", 1)) c.i += 2;
      else if (u === "UNIQUE") c.i++;
      else if (u === "DEFAULT") {
        c.i++;
        parseTableDefault(c);
      } else if (u === "REFERENCES") {
        c.i++;
        try {
          parseForeignKey(c);
        } catch (e) {
          throw e instanceof Reject ? new Reject("up:table-body:references") : e;
        }
      } else {
        throw new Reject(`up:table-body:${(u || x?.k || "end").toLowerCase()}`);
      }
    }
  } while (c.eatP(","));
  c.needP(")", "up:table-body:unsupported");
  c.needEnd("up:table-body:trailing");
}

function parseSequence(c) {
  c.needKw("SEQUENCE");
  if (c.eatKw("IF")) {
    c.needKw("NOT");
    c.needKw("EXISTS");
  }
  c.qname();
  while (!c.done()) {
    if (c.eatKw("INCREMENT")) {
      c.eatKw("BY");
      c.number("up:sequence-option");
    } else if (c.eatKw("MINVALUE") || c.eatKw("MAXVALUE") || c.eatKw("CACHE")) {
      c.number("up:sequence-option");
    } else if (c.eatKw("START")) {
      c.eatKw("WITH");
      c.number("up:sequence-option");
    } else if (c.eatKw("NO")) {
      c.need(c.eatKw("MINVALUE") || c.eatKw("MAXVALUE") || c.eatKw("CYCLE"), "up:sequence-option");
    } else if (c.eatKw("CYCLE")) {
      // ok
    } else if (c.eatKw("AS")) {
      c.need(["SMALLINT", "INTEGER", "INT", "BIGINT"].some((w) => c.eatKw(w)), "up:sequence-option");
    } else {
      throw new Reject("up:sequence-option");
    }
  }
}

function parseAlterTable(c) {
  c.needKw("TABLE");
  if (c.eatKw("IF")) c.needKw("EXISTS");
  c.eatKw("ONLY");
  c.qname();
  if (c.eatKw("VALIDATE")) {
    c.needKw("CONSTRAINT");
    c.ident();
    c.needEnd();
    return;
  }
  c.needKw("ADD");
  c.eatKw("COLUMN");
  if (c.eatKw("IF")) {
    c.needKw("NOT");
    c.needKw("EXISTS");
  }
  c.ident();
  const type = c.type();
  c.need(!SERIAL.has(type[type.length - 1]), "up:add-column-type");
  let notNull = false;
  let hasDefault = false;
  for (;;) {
    if (c.eatKw("NULL")) continue;
    if (c.kw("NOT") && c.kw("NULL", 1)) {
      c.i += 2;
      notNull = true;
      continue;
    }
    if (c.eatKw("DEFAULT")) {
      c.literal("up:add-column-default");
      hasDefault = true;
      continue;
    }
    break;
  }
  if (c.isP(",")) throw new Reject("up:multiple-actions");
  c.needEnd("up:add-column-constraint");
  c.need(!notNull || hasDefault, "up:add-column-not-null-without-default");
}

function parseSetLocal(c) {
  c.needKw("SET");
  c.needKw("LOCAL");
  c.need(c.eatKw("LOCK_TIMEOUT") || c.eatKw("STATEMENT_TIMEOUT"));
  const x = c.next();
  c.need(Boolean(x) && ((x.k === "op" && x.v === "=") || (x.k === "id" && x.u === "TO")));
  const s = c.next();
  c.need(Boolean(s) && s.k === "str");
  c.needEnd();
}

function parseComment(c) {
  c.needKw("COMMENT");
  c.needKw("ON");
  c.need(["TABLE", "COLUMN", "INDEX", "SEQUENCE"].some((w) => c.eatKw(w)));
  c.qname();
  c.needKw("IS");
  const x = c.next();
  c.need(Boolean(x) && (x.k === "str" || (x.k === "id" && x.u === "NULL")));
  c.needEnd();
}

// Parses one up statement against forms U1 to U7. Returns facts (index name) or throws Reject.
function parseUpStatement(tokens) {
  const c = new Cursor(tokens);
  const first = c.next();
  if (!first || first.k !== "id") throw new Reject(UNSUP);
  c.i = 0;
  switch (first.u) {
    case "SET":
      parseSetLocal(c);
      return {};
    case "COMMENT":
      parseComment(c);
      return {};
    case "ALTER":
      c.i = 1;
      parseAlterTable(c);
      return {};
    case "CREATE": {
      c.i = 1;
      if (c.kw("UNIQUE") || c.kw("INDEX")) return parseIndex(c);
      if (c.kw("TABLE")) {
        parseTable(c);
        return {};
      }
      if (c.kw("SEQUENCE")) {
        parseSequence(c);
        return {};
      }
      throw new Reject(UNSUP);
    }
    default:
      throw new Reject(UNSUP);
  }
}

function toText(input) {
  if (typeof input === "string") return input;
  return new TextDecoder("utf-8", { fatal: true }).decode(input);
}

function classifyUp(up) {
  let statements;
  try {
    statements = tokenize(toText(up));
  } catch (e) {
    if (e instanceof Opaque) return { ok: false, reason: `up:opaque:${e.kind}` };
    if (e instanceof TypeError) return { ok: false, reason: "up:opaque:encoding" };
    throw e;
  }
  const indexNames = [];
  for (const s of statements) {
    try {
      const facts = parseUpStatement(s);
      if (facts.indexName !== undefined) indexNames.push(facts.indexName);
    } catch (e) {
      if (e instanceof Reject) return { ok: false, reason: e.reason };
      throw e;
    }
  }
  return { ok: true, indexNames };
}

// Down: absent, or every statement is U1 or DROP INDEX of one index that the paired up created.
function classifyDown(down, indexNames) {
  let statements;
  try {
    statements = tokenize(toText(down));
  } catch (e) {
    if (e instanceof Opaque || e instanceof TypeError) return { ok: false, reason: "opaque-down" };
    throw e;
  }
  for (const s of statements) {
    const c = new Cursor(s);
    try {
      if (c.kw("SET")) {
        parseSetLocal(c);
        continue;
      }
      c.needKw("DROP");
      c.needKw("INDEX");
      c.eatKw("CONCURRENTLY");
      if (c.eatKw("IF")) c.needKw("EXISTS");
      const name = c.qname().join(".");
      c.eatKw("RESTRICT");
      c.needEnd();
      c.need(indexNames.some((n) => name === n || name === `public.${n}`));
    } catch (e) {
      if (e instanceof Reject) return { ok: false, reason: "data-lost-on-down" };
      throw e;
    }
  }
  return { ok: true };
}

// Classifies one migration pair. up and down are exact file contents (string or Buffer);
// down is null when the file is absent. goSources are the non-test Go sources of the
// migrator, {path, content}. First failing reason wins: up grammar, down, hook.
export function classifyPair({ version, up, down, goSources }) {
  const upHash = sha256(up);
  const downHash = down === null || down === undefined ? "absent" : sha256(down);
  const result = (cls, reason) => ({ version, class: cls, reason, upHash, downHash });
  const u = classifyUp(up);
  if (!u.ok) return result("needs-approval", u.reason);
  if (downHash !== "absent") {
    const d = classifyDown(down, u.indexNames);
    if (!d.ok) return result("needs-approval", d.reason);
  }
  for (const src of goSources ?? []) {
    const text = typeof src.content === "string" ? src.content : Buffer.from(src.content).toString("latin1");
    if (text.includes(version)) return result("needs-approval", "hook");
  }
  return result("additive", "none");
}

// ---------------------------------------------------------------------------------------
// Tree, ledger, inventory
// ---------------------------------------------------------------------------------------

class GateRefusal extends Error {
  constructor(reason) {
    super(reason);
    this.gateReason = reason;
  }
}

const byteCompare = (a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b));

// Up versions in Go's order: sort the *.up.sql file names bytewise, then strip the suffix.
export function listVersions(migrationsDir) {
  return readdirSync(migrationsDir)
    .filter((name) => name.endsWith(".up.sql"))
    .sort(byteCompare)
    .map((name) => name.slice(0, -".up.sql".length));
}

function walkGo(root, rel, out) {
  let entries;
  try {
    entries = readdirSync(join(root, rel), { withFileTypes: true });
  } catch {
    return;
  }
  for (const e of entries) {
    const r = `${rel}/${e.name}`;
    if (e.isDirectory()) walkGo(root, r, out);
    else if (e.name.endsWith(".go") && !e.name.endsWith("_test.go")) out.push(r);
  }
}

const GO_DIRS = ["server/cmd/migrate", "server/internal/migrations"];

// Reads a candidate tree. Throws GateRefusal("tree-unreadable") when the migrations
// directory cannot be read.
export function loadTree(root) {
  if (typeof root !== "string" || root === "") throw new GateRefusal("tree-unreadable");
  const migDir = join(root, "server/migrations");
  let versions;
  try {
    if (!statSync(migDir).isDirectory()) throw new Error("not a directory");
    versions = listVersions(migDir);
  } catch {
    throw new GateRefusal("tree-unreadable");
  }
  const pairs = new Map();
  try {
    for (const v of versions) {
      const up = readFileSync(join(migDir, `${v}.up.sql`));
      let down = null;
      try {
        down = readFileSync(join(migDir, `${v}.down.sql`));
      } catch (e) {
        if (e.code !== "ENOENT") throw e;
      }
      pairs.set(v, { up, down });
    }
  } catch {
    throw new GateRefusal("tree-unreadable");
  }
  const goPaths = [];
  const perDir = GO_DIRS.map((d) => {
    const found = [];
    walkGo(root, d, found);
    goPaths.push(...found);
    return found.length;
  });
  goPaths.sort(byteCompare);
  const goSources = goPaths.map((path) => ({ path, content: readFileSync(join(root, path)) }));
  const hash = createHash("sha256");
  for (const s of goSources) {
    hash.update(`${s.path}\0`);
    hash.update(s.content);
    hash.update("\0");
  }
  let capability = null;
  try {
    capability = readFileSync(join(root, "deploy/cd/migration-gate.capability"), "utf8");
  } catch {
    capability = null;
  }
  return {
    versions,
    pairs,
    goSources,
    goSourcesComplete: perDir.every((n) => n > 0),
    migratorSourceSha256: hash.digest("hex"),
    capability,
  };
}

// The ledger is the psql output of "SELECT version ... ORDER BY version": one version per line.
export function ledgerSha256(bytes) {
  return sha256(bytes);
}

function parseLedger(bytes) {
  if (bytes === null || bytes === undefined) throw new GateRefusal("ledger-unreadable");
  let text;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    throw new GateRefusal("ledger-unreadable");
  }
  if (text === "") return new Set();
  if (!text.endsWith("\n")) throw new GateRefusal("ledger-unreadable");
  const lines = text.slice(0, -1).split("\n");
  for (const l of lines) {
    if (l === "" || /[\x00-\x1f\x7f]/.test(l)) throw new GateRefusal("ledger-unreadable");
  }
  return new Set(lines);
}

function buildInventory(tree, ledger) {
  const pending = tree.versions
    .filter((v) => !ledger.has(v))
    .map((v) => {
      const { up, down } = tree.pairs.get(v);
      return classifyPair({ version: v, up, down, goSources: tree.goSources });
    })
    .sort((a, b) => (a.version < b.version ? -1 : a.version > b.version ? 1 : 0));
  const lines = pending.map((p) => `${p.version}\t${p.upHash}\t${p.downHash}\t${p.class}\t${p.reason}\n`);
  const text = `${lines.join("")}migrator-source\t${tree.migratorSourceSha256}\nclassifier\t${CLASSIFIER_VERSION}\n`;
  return { pending, inventory: `sha256:${sha256(text)}` };
}

function readyInventory(input, tree) {
  if (!tree.goSourcesComplete) throw new GateRefusal("migrator-source-missing");
  const ledger = parseLedger(input.ledgerBytes);
  if (typeof input.expectedLedgerSha256 !== "string" || ledgerSha256(input.ledgerBytes) !== input.expectedLedgerSha256) {
    throw new GateRefusal("ledger-digest-mismatch");
  }
  return buildInventory(tree, ledger);
}

export function inventoryOf(input) {
  try {
    const tree = loadTree(input.treeRoot);
    const { pending, inventory } = readyInventory(input, tree);
    return {
      ok: true,
      inventory,
      pending,
      migratorSourceSha256: tree.migratorSourceSha256,
      classifierVersion: CLASSIFIER_VERSION,
      ledgerSha256: ledgerSha256(input.ledgerBytes),
    };
  } catch (e) {
    return { ok: false, reason: e instanceof GateRefusal ? e.gateReason : "gate-error" };
  }
}

// ---------------------------------------------------------------------------------------
// Approval
// ---------------------------------------------------------------------------------------

export function approvalLine(inventory, tree) {
  if (typeof inventory !== "string" || !INVENTORY_RE.test(inventory)) throw new Error("inventory must be sha256:<64 lowercase hex>");
  if (typeof tree !== "string" || !HEX40.test(tree)) throw new Error("tree must be 40 lowercase hex");
  return `c00-migration-approval v1 inventory=${inventory} tree=${tree}`;
}

// comments = {pages: [[comment, ...], ...], error?, truncated?}. A comment is the GitHub
// commit-comment object. Matches only an unedited comment by user ID 129714106 whose whole
// body is the one approval line (one trailing newline allowed).
export function evaluateApproval(comments, { inventory, tree }) {
  const fail = (reason) => ({ ok: false, reason });
  if (!comments || typeof comments !== "object" || comments.error) return fail("approval-comments-error");
  const { pages } = comments;
  if (!Array.isArray(pages) || !pages.every(Array.isArray)) return fail("approval-comments-error");
  if (comments.truncated === true || pages.length > MAX_COMMENT_PAGES) return fail("approval-comments-over-limit");
  const line = approvalLine(inventory, tree);
  for (const page of pages) {
    for (const cm of page) {
      if (!cm || typeof cm !== "object") continue;
      if (!cm.user || cm.user.id !== CHEESE_USER_ID) continue;
      if (typeof cm.created_at !== "string" || cm.created_at !== cm.updated_at) continue;
      if (typeof cm.body !== "string") continue;
      const body = cm.body.endsWith("\r\n") ? cm.body.slice(0, -2) : cm.body.endsWith("\n") ? cm.body.slice(0, -1) : cm.body;
      if (body === line) return { ok: true, commentId: cm.id, line };
    }
  }
  return fail("approval-missing");
}

// ---------------------------------------------------------------------------------------
// Decision
// ---------------------------------------------------------------------------------------

// input: {treeRoot, ledgerBytes, expectedLedgerSha256, treeSha, sourceSha, checkoutSha, comments}
// Fails closed: any error is a refusal. Never throws.
export function decide(input) {
  const refuse = (reason, extra = {}) => ({ decision: "refuse", reason, pending: [], ...extra });
  try {
    const { sourceSha, checkoutSha } = input;
    if (typeof sourceSha !== "string" || sourceSha === "" || sourceSha !== checkoutSha) {
      return refuse("candidate-checkout-mismatch");
    }
    const tree = loadTree(input.treeRoot);
    if (tree.capability === null || tree.capability.replace(/\n$/, "") !== CAPABILITY_LINE) {
      return refuse("controller-predates-gate");
    }
    const inv = readyInventory(input, tree);
    const base = {
      inventory: inv.inventory,
      pending: inv.pending,
      migratorSourceSha256: tree.migratorSourceSha256,
      classifierVersion: CLASSIFIER_VERSION,
      ledgerSha256: ledgerSha256(input.ledgerBytes),
      treeSha: input.treeSha ?? null,
      sourceSha,
    };
    if (inv.pending.length === 0) return { decision: "admit", reason: "no-pending", ...base };
    if (inv.pending.every((p) => p.class === "additive")) return { decision: "admit", reason: "all-additive", ...base };
    if (typeof input.treeSha !== "string" || !HEX40.test(input.treeSha)) return { decision: "refuse", reason: "tree-sha-missing", ...base };
    const ev = evaluateApproval(input.comments === undefined ? { pages: [] } : input.comments, {
      inventory: inv.inventory,
      tree: input.treeSha,
    });
    if (ev.ok) return { decision: "admit", reason: "approved", ...base, approval: { commentId: ev.commentId, line: ev.line } };
    if (ev.reason === "approval-missing") {
      return { decision: "refuse", reason: ev.reason, ...base, approvalLine: approvalLine(inv.inventory, input.treeSha) };
    }
    return { decision: "refuse", reason: ev.reason, ...base };
  } catch (e) {
    return refuse(e instanceof GateRefusal ? e.gateReason : "gate-error");
  }
}

// ---------------------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------------------

function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i += 2) {
    const k = argv[i];
    if (!k.startsWith("--") || i + 1 >= argv.length) throw new Error(`bad argument: ${k}`);
    out[k.slice(2)] = argv[i + 1];
  }
  return out;
}

function readOrNull(path) {
  try {
    return readFileSync(path);
  } catch {
    return null;
  }
}

function readComments(path) {
  if (path === undefined) return undefined;
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (e) {
    return { error: `unreadable comments file: ${e.message}` };
  }
}

export function main(argv, io = { out: (s) => process.stdout.write(s), err: (s) => process.stderr.write(s) }) {
  const [cmd, ...rest] = argv;
  let a;
  try {
    a = parseArgs(rest);
  } catch (e) {
    io.err(`${e.message}\n`);
    return 2;
  }
  const need = (...keys) => keys.every((k) => typeof a[k] === "string");
  const json = (v) => io.out(`${JSON.stringify(v, null, 2)}\n`);
  switch (cmd) {
    case "approval-line":
      if (!need("inventory", "tree")) return (io.err("usage: approval-line --inventory sha256:HEX --tree HEX40\n"), 2);
      try {
        io.out(`${approvalLine(a.inventory, a.tree)}\n`);
        return 0;
      } catch (e) {
        io.err(`${e.message}\n`);
        return 2;
      }
    case "versions":
      if (!need("tree-root")) return (io.err("usage: versions --tree-root DIR\n"), 2);
      try {
        io.out(listVersions(join(resolve(a["tree-root"]), "server/migrations")).map((v) => `${v}\n`).join(""));
        return 0;
      } catch (e) {
        io.err(`tree-unreadable: ${e.message}\n`);
        return 1;
      }
    case "inventory": {
      if (!need("tree-root", "ledger", "ledger-sha256")) return (io.err("usage: inventory --tree-root DIR --ledger FILE --ledger-sha256 HEX\n"), 2);
      const r = inventoryOf({
        treeRoot: resolve(a["tree-root"]),
        ledgerBytes: readOrNull(a.ledger),
        expectedLedgerSha256: a["ledger-sha256"],
      });
      json(r);
      return r.ok ? 0 : 1;
    }
    case "decide": {
      if (!need("tree-root", "ledger", "ledger-sha256", "source-sha", "checkout-sha")) {
        io.err("usage: decide --tree-root DIR --ledger FILE --ledger-sha256 HEX --tree-sha HEX40 --source-sha SHA --checkout-sha SHA [--comments FILE]\n");
        return 2;
      }
      const d = decide({
        treeRoot: resolve(a["tree-root"]),
        ledgerBytes: readOrNull(a.ledger),
        expectedLedgerSha256: a["ledger-sha256"],
        treeSha: a["tree-sha"],
        sourceSha: a["source-sha"],
        checkoutSha: a["checkout-sha"],
        comments: readComments(a.comments),
      });
      json(d);
      return d.decision === "admit" ? 0 : 1;
    }
    default:
      io.err("usage: migration-gate.mjs <versions|inventory|approval-line|decide> [--option value ...]\n");
      return 2;
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = main(process.argv.slice(2));
}

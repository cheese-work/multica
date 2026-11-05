/**
 * Keeps secrets out of an MCP JSON draft. A key whose name looks like a
 * credential (`env`, `headers`, `token`, `authorization`, ...) is dropped at any
 * depth, inside arrays too, when the draft is stored. It is read back from the
 * saved server config when the draft is restored. A new server has no saved
 * config, so those values are typed again.
 *
 * Text that does not parse yet (an unfinished edit) is kept up to its first
 * credential key.
 */
const SECRET_KEY = /env|header|token|secret|passw|auth|key|credential|cookie/i;

type Json = Record<string, unknown>;
const isObject = (v: unknown): v is Json =>
  typeof v === "object" && v !== null && !Array.isArray(v);

function stripValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stripValue);
  return isObject(value) ? stripMcpSecrets(value) : value;
}

export function stripMcpSecrets(value: Json): Json {
  const out: Json = {};
  for (const [key, v] of Object.entries(value)) {
    if (!SECRET_KEY.test(key)) out[key] = stripValue(v);
  }
  return out;
}

function stringEnd(text: string, start: number): number {
  for (let i = start + 1; i < text.length; i++) {
    if (text[i] === "\\") i++;
    else if (text[i] === '"') return i + 1;
  }
  return text.length;
}

/**
 * JSON editor text that is safe to store. Valid JSON is redacted by key. Text
 * that does not parse (an unfinished edit) fails closed: everything from the
 * first credential-like key to the end is dropped, because where its value ends
 * cannot be told reliably (multiline PEM, `Bearer x`, a missing quote).
 */
export function redactMcpJsonText(text: string): string {
  try {
    const parsed: unknown = JSON.parse(text);
    if (isObject(parsed)) return JSON.stringify(stripMcpSecrets(parsed), null, 2);
  } catch {
    // Not valid yet: cut at the first credential key.
  }
  const cut = firstSecretKeyStart(text);
  return cut < 0 ? text : text.slice(0, cut);
}

/** Start index of the first key, quoted or bare, whose name looks like a credential; -1 if none. */
function firstSecretKeyStart(text: string): number {
  const bareWord = /[\w.-]+/y;
  let i = 0;
  while (i < text.length) {
    const c = text[i]!;
    if (c === '"') {
      const end = stringEnd(text, i);
      if (/^\s*:/.test(text.slice(end)) && isSecretKey(text.slice(i, end))) return i;
      i = end;
    } else if (/[\w.-]/.test(c)) {
      bareWord.lastIndex = i;
      const end = i + bareWord.exec(text)![0].length;
      if (/^['"]?\s*:/.test(text.slice(end)) && SECRET_KEY.test(text.slice(i, end))) return i;
      i = end;
    } else {
      i++;
    }
  }
  return -1;
}

function isSecretKey(literal: string): boolean {
  try {
    return SECRET_KEY.test(JSON.parse(literal) as string);
  } catch {
    return SECRET_KEY.test(literal); // Unterminated or malformed key: match its raw text.
  }
}

function restoreValue(saved: unknown, draft: unknown): unknown {
  if (Array.isArray(draft)) {
    return draft.map((item, index) =>
      restoreValue(Array.isArray(saved) ? saved[index] : undefined, item),
    );
  }
  if (isObject(draft)) return restoreMcpSecrets(isObject(saved) ? saved : {}, draft);
  return draft;
}

/** `draft` laid over `saved`: credential keys always come from `saved`, whatever the draft holds. */
export function restoreMcpSecrets(saved: Json, draft: Json): Json {
  const out: Json = {};
  for (const [key, v] of Object.entries(saved)) {
    if (SECRET_KEY.test(key)) out[key] = v;
  }
  for (const [key, v] of Object.entries(draft)) {
    if (!SECRET_KEY.test(key)) out[key] = restoreValue(saved[key], v);
  }
  return out;
}

/**
 * A restored draft marks a withheld credential as `null`. Before saving, swap
 * each such placeholder for the saved value, or drop it when none was saved.
 * A credential the user actually typed is left alone.
 */
export function fillRedactedSecrets(saved: Json, config: Json): Json {
  const out: Json = {};
  for (const [key, v] of Object.entries(config)) {
    if (SECRET_KEY.test(key) && v === null) {
      if (saved[key] !== undefined) out[key] = saved[key];
    } else {
      out[key] = fillValue(saved[key], v);
    }
  }
  return out;
}

/** Arrays pair with the saved array by position, like `restoreValue`. */
function fillValue(saved: unknown, value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map((item, index) => fillValue(Array.isArray(saved) ? saved[index] : undefined, item));
  }
  return isObject(value) ? fillRedactedSecrets(isObject(saved) ? saved : {}, value) : value;
}

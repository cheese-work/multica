/**
 * Keeps secrets out of an MCP JSON draft. A key whose name looks like a
 * credential (`env`, `headers`, `token`, `authorization`, ...) is dropped at any
 * depth, inside arrays too, when the draft is stored. It is read back from the
 * saved server config when the draft is restored. A new server has no saved
 * config, so those values are typed again.
 *
 * Text that does not parse yet (an unfinished edit) is kept too: a scanner
 * replaces each credential key's value with `null` without needing valid JSON.
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

/** The end index of the JSON value starting at `start`; text length if it never closes. */
function valueEnd(text: string, start: number): number {
  const first = text[start];
  if (first === '"') return stringEnd(text, start);
  if (first === "{" || first === "[") {
    let depth = 0;
    for (let i = start; i < text.length; i++) {
      const c = text[i];
      if (c === '"') i = stringEnd(text, i) - 1;
      else if (c === "{" || c === "[") depth++;
      else if (c === "}" || c === "]") {
        depth--;
        if (depth === 0) return i + 1;
      }
    }
    return text.length;
  }
  let i = start;
  while (i < text.length && !/[\s,}\]]/.test(text[i]!)) i++;
  return i;
}

function stringEnd(text: string, start: number): number {
  for (let i = start + 1; i < text.length; i++) {
    if (text[i] === "\\") i++;
    else if (text[i] === '"') return i + 1;
  }
  return text.length;
}

/** JSON editor text with every credential key's value replaced by `null`. Works on unfinished JSON. */
export function redactMcpJsonText(text: string): string {
  try {
    const parsed: unknown = JSON.parse(text);
    if (isObject(parsed)) return JSON.stringify(stripMcpSecrets(parsed), null, 2);
  } catch {
    // Not valid yet: scan instead.
  }
  let out = "";
  let i = 0;
  while (i < text.length) {
    if (text[i] !== '"') {
      out += text[i++];
      continue;
    }
    const end = stringEnd(text, i);
    const literal = text.slice(i, end);
    out += literal;
    i = end;
    let j = i;
    while (j < text.length && /\s/.test(text[j]!)) j++;
    if (text[j] !== ":") continue;
    let key = literal.slice(1, literal.endsWith('"') && literal.length > 1 ? -1 : undefined);
    try {
      key = JSON.parse(literal) as string;
    } catch {
      // Unterminated or malformed key: match its raw text.
    }
    if (!SECRET_KEY.test(key)) continue;
    let k = j + 1;
    while (k < text.length && /\s/.test(text[k]!)) k++;
    if (k >= text.length) continue;
    out += `${text.slice(i, j + 1)} null`;
    i = valueEnd(text, k);
  }
  return out;
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
    } else if (isObject(v)) {
      out[key] = fillRedactedSecrets(isObject(saved[key]) ? (saved[key] as Json) : {}, v);
    } else {
      out[key] = v;
    }
  }
  return out;
}

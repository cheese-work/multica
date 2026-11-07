/**
 * Keeps secrets out of an MCP JSON draft. A key whose name looks like a
 * credential (`env`, `headers`, `token`, `authorization`, ...) is dropped at any
 * depth, inside arrays too, when the draft is stored. It is read back from the
 * saved server config when the draft is restored. A new server has no saved
 * config, so those values are typed again.
 *
 * A restored draft never brings saved credentials back onto an edit that moved
 * the destination or removed a credential (`credentialsDropped`), nor onto a
 * draft whose destination differs from the server saved now (`destinationMoved`,
 * checked on restore: the saved server may have changed since the draft was
 * made). The user would otherwise send a token to a host it was not saved for,
 * or find a deleted one again.
 *
 * Text that does not parse yet (an unfinished edit) is not stored: finding
 * where a malformed credential value ends is a guess, and a wrong guess leaks.
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

/**
 * JSON editor text that is safe to store: a finished object with credential keys
 * removed. Text that is not valid JSON yet, or not an object, returns null.
 */
export function redactMcpJsonText(text: string): string | null {
  try {
    const parsed: unknown = JSON.parse(text);
    if (isObject(parsed)) return JSON.stringify(stripMcpSecrets(parsed), null, 2);
  } catch {
    // Not valid yet.
  }
  return null;
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

const DESTINATION_KEY = /^(url|uri|endpoint|command|args|type)$/i;

function collect(value: unknown, path: string, secret: boolean, dest: Json, creds: Set<string>): void {
  if (value === null && secret) creds.add(`${path}.*`);
  if (Array.isArray(value)) {
    value.forEach((item, i) => collect(item, `${path}[${i}]`, secret, dest, creds));
    return;
  }
  if (isObject(value)) {
    for (const [key, v] of Object.entries(value)) {
      const child = `${path}.${key}`;
      const childSecret = secret || SECRET_KEY.test(key);
      if (childSecret) creds.add(child);
      else if (DESTINATION_KEY.test(key)) dest[child] = v;
      collect(v, child, childSecret, dest, creds);
    }
  }
}

/** A `null` placeholder stands for the whole saved subtree below it. */
function hasNullAncestor(creds: Set<string>, path: string): boolean {
  for (let end = path.lastIndexOf("."); end > 0; end = path.lastIndexOf(".", end - 1)) {
    if (creds.has(`${path.slice(0, end)}.*`)) return true;
  }
  return false;
}

const destinationKey = (dest: Json) => JSON.stringify(dest, Object.keys(dest).sort());

/** Whether `current` points at a different destination (url, command, args, type) than `saved`. */
export function destinationMoved(saved: Json, current: Json): boolean {
  const savedDest: Json = {};
  const dest: Json = {};
  collect(saved, "", false, savedDest, new Set());
  collect(current, "", false, dest, new Set());
  return destinationKey(savedDest) !== destinationKey(dest);
}

/**
 * Whether `current` moved a destination or lost a credential that `saved`
 * holds. A `null` placeholder counts as still present.
 */
export function credentialsDropped(saved: Json, current: Json): boolean {
  const savedCreds = new Set<string>();
  collect(saved, "", false, {}, savedCreds);
  if (savedCreds.size === 0) return false;
  const creds = new Set<string>();
  collect(current, "", false, {}, creds);
  return (
    destinationMoved(saved, current) ||
    [...savedCreds].some((path) => !creds.has(path) && !hasNullAncestor(creds, path))
  );
}

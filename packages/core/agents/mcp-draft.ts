/**
 * Keeps secrets out of an MCP JSON draft. A key whose name looks like a
 * credential (`env`, `headers`, `token`, `authorization`, ...) is dropped at any
 * depth when the draft is stored, and read back from the saved server config
 * when the draft is restored. A new server has no saved config, so those values
 * are typed again; an edited server keeps its saved ones.
 */
const SECRET_KEY = /env|header|token|secret|passw|auth|key|credential|cookie/i;

type Json = Record<string, unknown>;
const isObject = (v: unknown): v is Json =>
  typeof v === "object" && v !== null && !Array.isArray(v);

export function stripMcpSecrets(value: Json): Json {
  const out: Json = {};
  for (const [key, v] of Object.entries(value)) {
    if (SECRET_KEY.test(key)) continue;
    out[key] = isObject(v) ? stripMcpSecrets(v) : v;
  }
  return out;
}

/** `draft` (secret-free) laid over `saved`: secret keys always come from `saved`. */
export function restoreMcpSecrets(saved: Json, draft: Json): Json {
  const out: Json = {};
  for (const [key, v] of Object.entries(saved)) {
    if (SECRET_KEY.test(key)) out[key] = v;
  }
  for (const [key, v] of Object.entries(draft)) {
    const base = saved[key];
    out[key] = isObject(v) && isObject(base) ? restoreMcpSecrets(base, v) : v;
  }
  return out;
}

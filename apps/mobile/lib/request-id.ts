/**
 * Per-request ID. 8 random base36 chars + 4 base36 timestamp suffix —
 * collision-resistant enough for client-side telemetry, short enough to
 * eyeball in logs. Sent as `X-Request-ID` header so backend can correlate
 * its own log lines with the client.
 *
 * Mirrors the role of `createRequestId` in packages/core/utils.ts but
 * mobile owns its own implementation (zero core import).
 */
export function createRequestId(): string {
  const rand = Math.random().toString(36).slice(2, 10);
  const ts = Date.now().toString(36).slice(-4);
  return `${rand}${ts}`;
}

/**
 * Idempotency key (`client_message_id`, attention `request_id`): the server
 * dedupes on it and rejects a `request_id` that is not a UUID. Uses
 * `crypto.randomUUID` when the runtime has it (Hermes/Expo may not), else an
 * RFC 4122 v4 UUID built from `Math.random`.
 */
export function createIdempotencyKey(): string {
  const uuid = globalThis.crypto?.randomUUID?.();
  if (uuid) return uuid;
  return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
    const r = Math.floor(Math.random() * 16);
    return (c === "x" ? r : (r & 0x3) | 0x8).toString(16);
  });
}

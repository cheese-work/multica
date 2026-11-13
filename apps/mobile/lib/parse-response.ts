/**
 * Mobile-owned parseWithFallback. Mirrors packages/core/api/schema.ts —
 * the boundary defense for installed-app schema drift required by root
 * CLAUDE.md "API Response Compatibility" and apps/mobile/CLAUDE.md
 * "Type drift defense".
 *
 * Why we mirror instead of import: keeps mobile fully decoupled and lets
 * us route the warning into mobile's own logger instead of the core
 * schemaLogger singleton. Behavior is identical: safeParse → on success
 * return parsed; on failure log + return fallback (never throw into UI).
 */
import { type ZodType } from "zod";

export interface ParseOptions {
  endpoint: string;
}

/** Dotted zod issue paths, e.g. `metadata.decision_park`; `(root)` when empty. */
export function schemaIssuePaths(issues: { path: PropertyKey[] }[]): string[] {
  return [...new Set(issues.map((i) => (i.path.length ? i.path.map(String).join(".") : "(root)")))];
}

/** A 2xx response that did not match its schema: the server answered, the data is unreadable. */
export class SchemaValidationError extends Error {
  readonly endpoint: string;
  readonly paths: string[];
  constructor(endpoint: string, paths: string[]) {
    super(`API response failed schema validation: ${endpoint} (${paths.join(", ")})`);
    this.name = "SchemaValidationError";
    this.endpoint = endpoint;
    this.paths = paths;
  }
}

function logSchemaFailure(
  issues: { path: PropertyKey[] }[],
  received: unknown,
  endpoint: string,
): string[] {
  const paths = schemaIssuePaths(issues);
  console.warn(`[api] schema validation failed: ${endpoint} (${paths.join(", ")})`, {
    issues,
    received,
  });
  return paths;
}

/** Like parseWithFallback, but throws when the caller cannot render a fallback. */
export function parseOrThrow<T>(data: unknown, schema: ZodType, opts: ParseOptions): T {
  const result = schema.safeParse(data);
  if (result.success) return result.data as T;
  throw new SchemaValidationError(
    opts.endpoint,
    logSchemaFailure(result.error.issues, data, opts.endpoint),
  );
}

export function parseWithFallback<T>(
  data: unknown,
  schema: ZodType,
  fallback: T,
  opts: ParseOptions,
): T {
  const result = schema.safeParse(data);
  if (result.success) return result.data as T;
  logSchemaFailure(result.error.issues, data, opts.endpoint);
  return fallback;
}

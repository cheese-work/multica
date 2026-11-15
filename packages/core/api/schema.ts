import type { ZodType } from "zod";
import { type Logger, noopLogger } from "../logger";

// Module-level logger for schema warnings. Defaults to no-op so test
// runs don't spam stderr; the platform layer wires a real logger via
// `setSchemaLogger` at app boot.
let schemaLogger: Logger = noopLogger;

export function setSchemaLogger(logger: Logger): void {
  schemaLogger = logger;
}

export interface ParseOptions {
  /** Endpoint identifier used in the warning log so we can grep for which
   *  contract drifted in production telemetry. */
  endpoint: string;
}

/**
 * Validate a JSON value parsed from an API response against a zod schema,
 * returning the parsed value on success or `fallback` on failure.
 *
 * On failure we log a warning with the endpoint and zod's structured error,
 * but never throw — the UI layer must keep rendering. This is the boundary
 * defense that turns "API contract drifted" from a white-screen incident
 * into a degraded-but-rendering page.
 *
 * The return type is anchored to `T` (inferred from `fallback`), not to the
 * schema's `z.infer` type. Schemas are intentionally **lenient** — string
 * enums kept as `z.string()` so an unknown enum value still parses, etc. —
 * so the parsed runtime value can be wider than the strict TS type at the
 * call site. The caller asserts compatibility by typing the fallback to the
 * expected `T`; downstream code is already responsible for handling unknown
 * enum values via `default`-bearing switches and optional chaining.
 *
 * See CLAUDE.md "API Response Compatibility" for when to reach for this.
 */
export function parseWithFallback<T>(
  data: unknown,
  schema: ZodType,
  fallback: T,
  opts: ParseOptions,
): T {
  const result = schema.safeParse(data);
  if (result.success) return result.data as T;
  logSchemaFailure(result.error.issues, data, opts);
  return fallback;
}

/** Dotted failing paths of a zod error, e.g. `metadata.decision_park`. */
export function schemaIssuePaths(issues: readonly { path: readonly PropertyKey[] }[]): string[] {
  return [...new Set(issues.map((i) => i.path.map(String).join(".") || "(root)"))];
}

/**
 * A 2xx body that failed its schema. Distinct from a network error and from a
 * 404 so the UI can say "this data is unreadable" instead of "check your
 * connection". Duck-typed by `name` in `isSchemaValidationError` so it holds
 * across module boundaries and test mocks.
 */
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

export function isSchemaValidationError(err: unknown): err is SchemaValidationError {
  return err instanceof Error && err.name === "SchemaValidationError";
}

/** Like `parseWithFallback`, but throws `SchemaValidationError` on failure. */
export function parseOrThrow<T>(data: unknown, schema: ZodType, opts: ParseOptions): T {
  const result = schema.safeParse(data);
  if (result.success) return result.data as T;
  logSchemaFailure(result.error.issues, data, opts);
  throw new SchemaValidationError(opts.endpoint, schemaIssuePaths(result.error.issues));
}

function logSchemaFailure(
  issues: readonly { path: readonly PropertyKey[] }[],
  received: unknown,
  opts: ParseOptions,
): void {
  const paths = schemaIssuePaths(issues);
  schemaLogger.warn(
    `API response failed schema validation: ${opts.endpoint} (${paths.join(", ")})`,
    { endpoint: opts.endpoint, paths, issues, received },
  );
}

import { SchemaValidationError } from "@/lib/parse-response";

export type IssueLoadErrorKind = "data" | "not_found" | "network";

/** Which message the issue screen shows for a failed load. No error at all means no issue. */
export function classifyIssueLoadError(error: unknown): IssueLoadErrorKind {
  if (error instanceof SchemaValidationError) return "data";
  if (!error) return "not_found";
  if ((error as { status?: unknown }).status === 404) return "not_found";
  return "network";
}

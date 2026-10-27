import { ApiError } from "@multica/core/api";

/**
 * The governance API names a refusal in its problem body (`problem`), which is
 * how a 409 from a stale version differs from a 409 for a version that no
 * longer exists. Anything without a readable name is `null`.
 */
export function problemOf(error: unknown): string | null {
  if (!(error instanceof ApiError)) return null;
  const body = error.body;
  if (body && typeof body === "object" && "problem" in body && typeof body.problem === "string") {
    return body.problem;
  }
  return null;
}

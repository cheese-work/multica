import { z } from "zod";
import { ApiError } from "../api/client";

const categoryChangeConflictSchema = z.object({
  code: z.literal("category_change_requires_confirmation"),
  issue_count: z.number().int().nonnegative(),
});

/**
 * Reads the server's "confirm this category change" conflict: how many issues
 * the change moves. Null for any other error, so an old or malformed server
 * falls through to the generic failure toast.
 */
export function issueStatusCategoryChangeConflictCount(error: unknown): number | null {
  if (!(error instanceof ApiError) || error.status !== 409) return null;
  const parsed = categoryChangeConflictSchema.safeParse(error.body);
  return parsed.success ? parsed.data.issue_count : null;
}

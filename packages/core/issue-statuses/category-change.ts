import { z } from "zod";
import { ApiError } from "../api/client";
import { normalizeIssueStatusCategory } from "../issues/config/status";
import type { IssueStatusCategory } from "../types";

const categorySchema = z
  .string()
  .transform((value) => normalizeIssueStatusCategory(value))
  .pipe(z.custom<IssueStatusCategory>((value) => value !== null));

const categoryChangeConflictSchema = z.object({
  code: z.literal("category_change_requires_confirmation"),
  issue_count: z.number().int().nonnegative(),
  from_category: categorySchema,
  to_category: categorySchema,
});

export interface IssueStatusCategoryChangeConflict {
  issueCount: number;
  from: IssueStatusCategory;
  to: IssueStatusCategory;
}

/**
 * Reads the server's "confirm this category change" conflict: how many issues
 * the change moves, and the categories it moves between. The server computes
 * both under the catalog lock, so they stay right when the editor opened on a
 * stale snapshot. Null for any other error, so an old or malformed server
 * falls through to the generic failure toast.
 */
export function parseIssueStatusCategoryChangeConflict(error: unknown): IssueStatusCategoryChangeConflict | null {
  if (!(error instanceof ApiError) || error.status !== 409) return null;
  const parsed = categoryChangeConflictSchema.safeParse(error.body);
  if (!parsed.success) return null;
  const { issue_count, from_category, to_category } = parsed.data;
  return { issueCount: issue_count, from: from_category, to: to_category };
}

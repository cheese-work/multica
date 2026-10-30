import { z } from "zod";

export const IssueRelationsResponseSchema = z.object({
  nodes: z.array(z.object({ id: z.string().uuid(), identifier: z.string(), title: z.string(), status: z.string() })).max(40),
  edges: z.array(z.object({ source: z.string().uuid(), target: z.string().uuid(), type: z.enum(["parent", "duplicate", "wakeup", "children_done", "mention", "shared_pr", "unknown"]).catch("unknown") })),
  omitted_count: z.number().int().nonnegative(),
  depth: z.union([z.literal(1), z.literal(2)]),
});

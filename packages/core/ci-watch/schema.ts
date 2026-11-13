import { z } from "zod";

// Enums stay plain strings: a mode or class the server adds later must render,
// not fail the whole panel.
export const CIWatchConfigSchema = z.object({
  repo: z.string(),
  mode: z.string(),
  branches: z.array(z.string()).default([]),
  events: z.array(z.string()).default([]),
  workflows_include: z.array(z.string()).default([]),
  workflows_exclude: z.array(z.string()).default([]),
  p1_mode: z.string(),
  p2_mode: z.string(),
  p3_mode: z.string(),
  p4_mode: z.string(),
  fix_owner_kind: z.string().nullable().default(null),
  fix_owner_id: z.string().nullable().default(null),
  notify_member_id: z.string().nullable().default(null),
  max_reruns_per_original_run: z.number(),
  max_new_issues_per_day: z.number(),
  max_owner_wakes_per_hour: z.number(),
  updated_at: z.string().nullable().default(null),
});
export type CIWatchConfig = z.infer<typeof CIWatchConfigSchema>;

// Breaker, cap and notify rows carry no workflow or run, so those columns
// default to blank.
const text = z.string().default("");
export const CIWatchEventSchema = z.object({
  at: z.string(),
  kind: z.string(),
  workflow: text,
  branch: text,
  run_id: z.number().default(0),
  detail: text,
  class: text,
  protocol: text,
  action: text,
  state: text,
  outcome: text,
  would_do: text,
});
export type CIWatchEvent = z.infer<typeof CIWatchEventSchema>;

/** `GET .../events` answers `{events: [...]}`; the schema unwraps it. */
export const CIWatchEventsSchema = z
  .object({ events: z.array(CIWatchEventSchema) })
  .transform((o) => o.events);
export const EMPTY_CI_WATCH_EVENTS: CIWatchEvent[] = [];

/** The server answers 403 with this code when the feature flag is off. */
export const CI_WATCHER_DISABLED_CODE = "ci_watcher_disabled";
export const CI_WATCH_MODES = ["off", "shadow", "act"] as const;
export const CI_WATCH_PROTOCOL_MODES = ["shadow", "act"] as const;
/** Events an admin can toggle; `merge_group` is shown as observed only. */
export const CI_WATCH_EVENTS = ["push", "schedule", "workflow_run", "workflow_dispatch"] as const;

import { z } from "zod";
import type { AgentRuntime } from "../types";

export const RuntimePauseSchema = z.object({
  // An unknown reason from a newer server still means "paused"; keep it paused
  // so Resume stays reachable, with the server default as the label.
  reason: z.enum(["maintenance", "incident", "suspended", "scheduled_update"]).catch("maintenance"),
  paused_by: z.string().nullable().catch(null),
  paused_at: z.string().catch(""),
  expires_at: z.string().catch(""),
}).loose();

export const DEFAULT_UPDATE_TIMEZONE = "Asia/Ho_Chi_Minh";

// Per-machine update schedule (CHE-5465). Field-level fallbacks keep a malformed
// schedule from dropping the runtime; the server owns the 10 to 30 window range.
export const RuntimeUpdateScheduleSchema = z.object({
  enabled: z.boolean().catch(false),
  cron: z.string().catch(""),
  timezone: z.string().catch(DEFAULT_UPDATE_TIMEZONE),
  window_minutes: z.number().int().catch(30),
  next_runs: z.array(z.string()).catch([]),
  updated_at: z.string().nullable().catch(null),
  updated_by: z.string().nullable().catch(null),
}).loose();

// An outcome a newer server adds reads as "unknown"; an absent one is null.
export const UpdateOutcomeSchema = z
  .enum([
    "updated",
    "up_to_date",
    "failed",
    "no_window",
    "cancelled",
    "skipped_manual_pause",
    "skipped_open_cycle",
    "unknown",
  ])
  .nullish()
  .transform((v) => v ?? null)
  .catch("unknown");

export const AgentRuntimeSchema = z.object({
  id: z.string(),
  workspace_id: z.string(),
  daemon_id: z.string().nullable().catch(null),
  name: z.string().catch(""),
  custom_name: z.string().nullable().catch(null),
  runtime_mode: z.enum(["local", "cloud"]).catch("local"),
  provider: z.string().catch(""),
  launch_header: z.string().catch(""),
  status: z.enum(["online", "offline"]).catch("offline"),
  device_info: z.string().catch(""),
  metadata: z.record(z.string(), z.unknown()).catch({}),
  owner_id: z.string().nullable().catch(null),
  visibility: z.enum(["private", "public"]).catch("private"),
  shared_with: z.array(z.string().uuid()).catch([]),
  profile_id: z.string().nullable().catch(null),
  pause: RuntimePauseSchema.nullable().catch(null),
  daemon_running_tasks: z.number().int().nonnegative().nullable().catch(null),
  update_schedule: RuntimeUpdateScheduleSchema.nullable().catch(null),
  last_update_outcome: UpdateOutcomeSchema,
  last_update_outcome_at: z.string().nullable().catch(null),
  max_concurrent_tasks: z.number().int().nullable().catch(null),
  last_seen_at: z.string().nullable().catch(null),
  created_at: z.string().catch(""),
  updated_at: z.string().catch(""),
  // Commit-based update verdict (CHE-2641). Absent or malformed means "no
  // update": an older server must never light up a badge.
  update_available: z.boolean().catch(false),
  server_commit: z.string().catch(""),
  server_version: z.string().catch(""),
  update_reason: z.string().catch(""),
}).loose();

export const EMPTY_RUNTIME: AgentRuntime = {
  id: "",
  workspace_id: "",
  daemon_id: null,
  name: "",
  runtime_mode: "local",
  provider: "",
  launch_header: "",
  status: "offline",
  device_info: "",
  metadata: {},
  owner_id: null,
  visibility: "private",
  shared_with: [],
  pause: null,
  daemon_running_tasks: null,
  update_schedule: null,
  last_update_outcome: null,
  last_update_outcome_at: null,
  max_concurrent_tasks: null,
  last_seen_at: null,
  created_at: "",
  updated_at: "",
  update_available: false,
  server_commit: "",
  server_version: "",
  update_reason: "",
};

// Machine history, audit log and planned times (CHE-5709). A list drops an item
// that has no usable id and keeps the rest; a field of an item falls back alone.
function listOf<T extends z.ZodType>(item: T) {
  return z
    .array(z.unknown())
    .catch([])
    .transform((items) => items.flatMap((i) => {
      const r = item.safeParse(i);
      return r.success ? [r.data as z.infer<T>] : [];
    }));
}

const nullableString = z.string().nullable().catch(null);

export const MachineCycleSchema = z.object({
  id: z.string(),
  state: z.string().catch("unknown"),
  outcome: UpdateOutcomeSchema,
  outcome_detail: nullableString,
  outcome_source: nullableString,
  from_commit: nullableString,
  target_commit: nullableString,
  notes: z.array(z.string()).catch([]),
  live_runs_at_end: z.number().int().nullable().catch(null),
  started_at: z.string().catch(""),
  deadline_at: z.string().catch(""),
  finished_at: nullableString,
}).loose();

export const MachineHistoryPageSchema = z.object({
  cycles: listOf(MachineCycleSchema),
  next_cursor: nullableString,
}).loose();

export const MachineAuditEventSchema = z.object({
  id: z.string(),
  at: z.string().catch(""),
  kind: z.string().catch(""),
  actor_type: z.string().catch(""),
  actor_id: nullableString,
  actor_name: nullableString,
  target_id: nullableString,
  target_name: nullableString,
  before: z.unknown().optional(),
  after: z.unknown().optional(),
}).loose();

export const MachineAuditPageSchema = z.object({
  events: listOf(MachineAuditEventSchema),
  next_cursor: nullableString,
}).loose();

export const MachinePlannedSchema = z.object({
  timezone: z.string().catch(DEFAULT_UPDATE_TIMEZONE),
  next_runs: z.array(z.unknown()).catch([]).transform((a) => a.filter((v): v is string => typeof v === "string")),
  server_deploys: listOf(z.object({
    commit: z.string(),
    version: z.string().catch(""),
    started_at: z.string().catch(""),
  }).loose()),
}).loose();

export type MachineCycle = z.infer<typeof MachineCycleSchema>;
export type MachineHistoryPage = z.infer<typeof MachineHistoryPageSchema>;
export type MachineAuditPage = z.infer<typeof MachineAuditPageSchema>;
export type MachinePlanned = z.infer<typeof MachinePlannedSchema>;

export const EMPTY_MACHINE_HISTORY: MachineHistoryPage = { cycles: [], next_cursor: null };
export const EMPTY_MACHINE_AUDIT: MachineAuditPage = { events: [], next_cursor: null };
export const EMPTY_MACHINE_PLANNED: MachinePlanned = { timezone: DEFAULT_UPDATE_TIMEZONE, next_runs: [], server_deploys: [] };

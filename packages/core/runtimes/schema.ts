import { z } from "zod";
import type { AgentRuntime } from "../types";

export const RuntimePauseSchema = z.object({
  // An unknown reason from a newer server still means "paused"; keep it paused
  // so Resume stays reachable, with the server default as the label.
  reason: z.enum(["maintenance", "incident", "suspended"]).catch("maintenance"),
  paused_by: z.string().nullable().catch(null),
  paused_at: z.string().catch(""),
  expires_at: z.string().catch(""),
}).loose();

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
  last_seen_at: null,
  created_at: "",
  updated_at: "",
  update_available: false,
  server_commit: "",
  server_version: "",
  update_reason: "",
};

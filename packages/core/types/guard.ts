/** Outcomes the guard writes to a run's decision log (`guard.Kind*`). */
export type GuardDecisionAction = "fixed" | "refused" | "plugin-error" | "shadow-hit" | "pass";

export interface GuardDayCounts {
  /** Calendar day in the viewer's time zone, `YYYY-MM-DD`. */
  day: string;
  fixed: number;
  refused: number;
  errors: number;
}

export interface GuardPluginStats {
  plugin: string;
  fixed: number;
  refused: number;
  errors: number;
  shadow_hits: number;
  passes: number;
  /** One row per day of the window, oldest first. */
  daily: GuardDayCounts[];
}

export interface GuardStats {
  days: number;
  tz: string;
  /** First day of the window, `YYYY-MM-DD`. */
  since: string;
  plugins: GuardPluginStats[];
}

export interface GuardDecision {
  id: string;
  task_id: string;
  /** RFC 3339. */
  time: string;
  plugin: string;
  /** Kept as sent: a newer server may add an outcome this client does not know. */
  action: string;
  tool: string;
  subcommand: string;
  /** The line the guard showed the agent. */
  line: string;
  issue: string;
  note: string;
  runtime: string;
}

export interface GuardDecisionFilters {
  plugin?: string;
  action?: string;
  since?: string;
  limit?: number;
}

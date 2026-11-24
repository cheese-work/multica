export type AuditActorType = "member" | "agent" | "system";

/** One row of the workspace audit trail (`GET /api/workspaces/{id}/audit-events`). */
export interface AuditEvent {
  id: string;
  /** RFC 3339, UTC. Render it in the viewing time zone. */
  at: string;
  actor_type: AuditActorType;
  actor_id: string | null;
  /** Null when the actor was deleted or is the system. */
  actor_name: string | null;
  task_id: string | null;
  /** `agent.update`, `runtime.delete`, ... */
  kind: string;
  target_type: string;
  target_id: string | null;
  /** Null when the target was deleted. */
  target_name: string | null;
  /** Allowed fields only. A redacted field holds the marker `"changed"`. */
  before: Record<string, unknown>;
  after: Record<string, unknown>;
  external_requested: string[];
}

export interface AuditEventsPage {
  events: AuditEvent[];
  /** Opaque keyset cursor; null on the last page. */
  next_cursor: string | null;
}

export interface AuditEventFilters {
  /** Prefix match: `agent.` matches `agent.update`. */
  kind?: string;
  actor_id?: string;
  target_id?: string;
  /** RFC 3339, inclusive. */
  since?: string;
  /** RFC 3339, exclusive. */
  until?: string;
  limit?: number;
  cursor?: string;
}

export type MemberRole = "owner" | "admin" | "member";

export interface WorkspaceRepo {
  url: string;
  description?: string;
}

export interface Workspace {
  id: string;
  name: string;
  slug: string;
  description: string | null;
  context: string | null;
  settings: Record<string, unknown>;
  repos: WorkspaceRepo[];
  issue_prefix: string;
  avatar_url: string | null;
  created_at: string;
  updated_at: string;
}

/**
 * One MCP server in the workspace's library.
 *
 * This is the WHOLE read shape: the stored configuration is write-only, so
 * urls, commands, headers, and env never leave the server for any role. UI
 * that needs to change an entry sends a replacement rather than editing what
 * it read back.
 *
 * `enabled` is only present on an AGENT's assignment list, where it is the
 * per-agent toggle; the workspace library listing has no binding to report.
 */
export interface WorkspaceMcpServer {
  id: string;
  workspace_id: string;
  name: string;
  transport: string;
  enabled?: boolean;
  /**
   * Live agents the entry is assigned to. Only the workspace library listing
   * carries it, and servers older than this field omit it.
   */
  agent_count?: number;
  created_at: string;
  updated_at: string;
}

export interface Member {
  id: string;
  workspace_id: string;
  user_id: string;
  role: MemberRole;
  created_at: string;
}

export interface User {
  id: string;
  name: string;
  email: string;
  avatar_url: string | null;
  onboarded_at: string | null;
  /**
   * JSONB payload from the server. Typed as `unknown` here so this module
   * stays independent of the questionnaire shape — the onboarding views
   * cast into `Partial<QuestionnaireAnswers>` when reading. Server always
   * returns an object (defaults to `{}`), never null.
   */
  onboarding_questionnaire: Record<string, unknown>;
  /** Preferred UI language. null means "follow client/system". */
  language: string | null;
  /**
   * Free-form self-description (role, stack, preferences). Injected into
   * the agent brief so coding agents have cheap, durable context about
   * who is requesting the work. Server always returns a string —
   * NOT NULL DEFAULT '' at the column level, empty when unset.
   */
  profile_description: string;
  /** Pinned IANA tz; null means "use browser-detected tz at render time". */
  timezone: string | null;
  created_at: string;
  updated_at: string;
}

export interface MemberWithUser {
  id: string;
  workspace_id: string;
  user_id: string;
  role: MemberRole;
  created_at: string;
  name: string;
  email: string;
  avatar_url: string | null;
}

export interface Invitation {
  id: string;
  workspace_id: string;
  inviter_id: string;
  invitee_email: string;
  invitee_user_id: string | null;
  role: MemberRole;
  status: "pending" | "accepted" | "declined" | "expired";
  created_at: string;
  updated_at: string;
  expires_at: string;
  inviter_name?: string;
  inviter_email?: string;
  workspace_name?: string;
}

export interface ShareLink {
  id: string;
  workspace_id: string;
  code: string;
  created_by: string;
  role: MemberRole;
  expires_at: string | null;
  max_uses: number | null;
  use_count: number;
  is_active: boolean;
  created_at: string;
  creator_name?: string;
  creator_email?: string;
}

export interface ShareLinkInfo {
  workspace_name: string;
  workspace_slug: string;
  creator_name?: string;
  role: MemberRole;
}

/**
 * CHE-766 export privacy policy: the CHE-755 export redaction mode and the
 * retention window for audit manifests. `redaction_mode` stays a plain
 * string, not a union — "strict" exists at the DB/CHECK level as a forward
 * compatibility placeholder but has no server-side enforcement yet
 * (server/internal/handler/workspace_export_privacy.go), so the UI must be
 * able to represent a value it does not offer as selectable rather than
 * coercing an unrecognized value into "small".
 */
export interface WorkspaceExportPrivacy {
  redaction_mode: string;
  manifest_retention_days: number;
}

/** How Jev squad routing acts: off skips Jev, shadow logs a pick without dispatching it, on dispatches it. */
export type JevRoutingMode = "off" | "shadow" | "on";

/**
 * CHE-1839: the slice of the workspace governance config the Jev / Governance
 * tab reads. Fields stay raw (a newer server may add modes), see
 * `resolveJevGovernance` for how the tab interprets them.
 */
/** One saved version of the governance settings: what they were before and what they were after. Settings stay raw: a newer server may add keys. */
export interface GovernanceAuditEntry {
  version: number;
  actor_id: string;
  previous: Record<string, unknown> | null;
  effective: Record<string, unknown>;
  changed_at: string;
}

export interface WorkspaceGovernanceConfig {
  version: number;
  settings: {
    jev_squad_routing_mode?: string | null;
    jev_squad_routing_enabled?: boolean | null;
    jev_min_confidence?: number | null;
    jev_api_url?: string | null;
  };
  /** Newest first; the server returns at most 50. */
  audit_history: GovernanceAuditEntry[];
}

/** What the tab shows: the effective routing mode and the confidence, with the version a write is based on, and the saved versions. `min_confidence` is null when the workspace never set one (the server then uses 0.90). */
export interface WorkspaceJevGovernance {
  version: number;
  routing_mode: JevRoutingMode;
  min_confidence: number | null;
  /** The custom Jev endpoint; empty means the default one. */
  api_url: string;
  history: GovernanceAuditEntry[];
}

/** A write is always based on the version the user saw. Omitted fields stay as they are. */
export interface WorkspaceJevGovernancePatch {
  expected_version: number;
  request_id: string;
  jev_squad_routing_mode?: "off" | "shadow";
  jev_min_confidence?: number;
  /** Empty clears the custom endpoint. */
  jev_api_url?: string;
}

/** Restores every setting to how it was after `rollback_to_version`, saved as a new version. */
export interface WorkspaceGovernanceRollback {
  expected_version: number;
  request_id: string;
  rollback_to_version: number;
}

/**
 * Whether a Jev provider key is stored, and whether this server can store one
 * (it holds an at-rest encryption key). The key itself is never readable.
 */
export interface JevCredentialPresence {
  present: boolean;
  configurable: boolean;
}

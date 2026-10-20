import type { GovernanceAuditEntry, JevRoutingMode } from "@multica/core/types";
import { routingModeOf } from "@multica/core/workspace/jev-governance";

// The server falls back to this when the workspace never set a confidence.
export const DEFAULT_MIN_CONFIDENCE = 0.9;

/** Two decimals, unless that would round the stored value: an untouched confidence is never rewritten. */
export function formatConfidence(value: number | null) {
  const v = value ?? DEFAULT_MIN_CONFIDENCE;
  const fixed = v.toFixed(2);
  return Number(fixed) === v ? fixed : String(v);
}

export type SettingKind =
  | "routing_mode"
  | "min_confidence"
  | "rule_mode"
  | "jev_governance_enabled"
  | "jev_fallback_agents_enabled"
  | "governance_corrections_enabled"
  | "limits"
  | "jev_api_url"
  | "other";

/** One setting as the history shows it: a value to print, or an opaque one that is only reported as changed. */
export type SettingValue = JevRoutingMode | number | boolean | string | null;

export interface SettingChange {
  kind: SettingKind;
  /** The raw settings key, used as the label of a setting this UI does not know. */
  key: string;
  before: SettingValue;
  after: SettingValue;
}

export interface SettingsDiff {
  changes: SettingChange[];
  /** Settings present on either side whose value is the same. */
  unchanged: number;
}

const BOOLEAN_KINDS = [
  "jev_governance_enabled",
  "jev_fallback_agents_enabled",
  "governance_corrections_enabled",
] as const;

function stable(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stable).join(",")}]`;
  if (value && typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>)
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([key, inner]) => `${JSON.stringify(key)}:${stable(inner)}`);
    return `{${entries.join(",")}}`;
  }
  return JSON.stringify(value) ?? "null";
}

/** The routing mode and its legacy flag are one setting to a reader. */
function logicalKey(key: string) {
  return key === "jev_squad_routing_mode" || key === "jev_squad_routing_enabled" ? "routing_mode" : key;
}

function kindOf(logical: string): SettingKind {
  switch (logical) {
    case "routing_mode":
      return "routing_mode";
    case "jev_min_confidence":
      return "min_confidence";
    case "rule_mode":
    case "jev_api_url":
    case "limits":
      return logical;
    default:
      return (BOOLEAN_KINDS as readonly string[]).includes(logical) ? (logical as SettingKind) : "other";
  }
}

function valueOf(settings: Record<string, unknown> | null, logical: string): SettingValue {
  const kind = kindOf(logical);
  if (kind === "routing_mode") return routingModeOf(settings);
  const raw = settings?.[logical];
  if (kind === "min_confidence") return typeof raw === "number" ? raw : null;
  if (kind === "rule_mode") return typeof raw === "string" && raw !== "" ? raw : "off";
  if ((BOOLEAN_KINDS as readonly string[]).includes(kind)) return raw === true;
  // Opaque settings are compared, never printed: a limits table or an address
  // is not something a history row should reproduce.
  return raw === undefined ? null : stable(raw);
}

/** What changed between two raw settings objects, in a stable display order. */
export function diffSettings(
  before: Record<string, unknown> | null,
  after: Record<string, unknown>,
): SettingsDiff {
  const logicals = new Set<string>();
  for (const settings of [before, after]) {
    for (const key of Object.keys(settings ?? {})) logicals.add(logicalKey(key));
  }
  const changes: SettingChange[] = [];
  let unchanged = 0;
  for (const logical of logicals) {
    const kind = kindOf(logical);
    const from = valueOf(before, logical);
    const to = valueOf(after, logical);
    if (from === to) {
      unchanged += 1;
      continue;
    }
    changes.push({ kind, key: logical, before: from, after: to });
  }
  const order: SettingKind[] = ["routing_mode", "min_confidence", "rule_mode"];
  const rank = (kind: SettingKind) => {
    const at = order.indexOf(kind);
    return at < 0 ? order.length : at;
  };
  changes.sort((a, b) => rank(a.kind) - rank(b.kind) || (a.key < b.key ? -1 : 1));
  return { changes, unchanged };
}

/** Restoring a version that turned routing on would bypass the locked `on` requirement. */
export function restoresRoutingOn(entry: GovernanceAuditEntry) {
  return routingModeOf(entry.effective) === "on";
}

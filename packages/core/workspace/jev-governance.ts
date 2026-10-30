import type {
  JevRoutingMode,
  WorkspaceGovernanceConfig,
  WorkspaceJevGovernance,
} from "../types";

/**
 * The effective Jev routing mode and confidence of a governance config. This
 * mirrors the server: an explicit known mode wins, an unknown one fails safe to
 * off, and with no mode the legacy enabled flag means on.
 */
export function resolveJevGovernance(config: WorkspaceGovernanceConfig): WorkspaceJevGovernance {
  const { jev_squad_routing_mode: mode, jev_squad_routing_enabled: legacyOn, jev_min_confidence: confidence, jev_api_url: apiUrl } =
    config.settings;
  const routing_mode: JevRoutingMode =
    mode === "off" || mode === "shadow" || mode === "on" ? mode : !mode && legacyOn ? "on" : "off";
  return {
    version: config.version,
    routing_mode,
    min_confidence: confidence ?? null,
    api_url: apiUrl ?? "",
    history: config.audit_history,
  };
}

/**
 * The routing mode a raw settings object resolves to, with the same rule as
 * `resolveJevGovernance`. History rows and rollback targets are raw settings.
 */
export function routingModeOf(settings: Record<string, unknown> | null): JevRoutingMode {
  if (!settings) return "off";
  const mode = settings.jev_squad_routing_mode;
  if (mode === "off" || mode === "shadow" || mode === "on") return mode;
  return !mode && settings.jev_squad_routing_enabled === true ? "on" : "off";
}

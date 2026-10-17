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
  const { jev_squad_routing_mode: mode, jev_squad_routing_enabled: legacyOn, jev_min_confidence: confidence } =
    config.settings;
  const routing_mode: JevRoutingMode =
    mode === "off" || mode === "shadow" || mode === "on" ? mode : !mode && legacyOn ? "on" : "off";
  return { version: config.version, routing_mode, min_confidence: confidence ?? null };
}

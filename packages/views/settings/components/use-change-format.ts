import { useT } from "../../i18n";
import { formatConfidence, type SettingChange } from "./jev-history";

/** How one change reads: a label and, for a known setting, its before and after. */
export function useChangeFormat() {
  const { t } = useT("settings");
  const value = (change: SettingChange, side: "before" | "after") => {
    const raw = side === "before" ? change.before : change.after;
    switch (change.kind) {
      case "routing_mode":
        return t(($) => $.jev_governance.mode[raw === "on" || raw === "shadow" ? raw : "off"]);
      case "min_confidence":
        return typeof raw === "number" ? formatConfidence(raw) : t(($) => $.jev_governance.history.default_value);
      case "jev_governance_enabled":
      case "jev_fallback_agents_enabled":
      case "governance_corrections_enabled":
        return raw === true ? t(($) => $.jev_governance.mode.on) : t(($) => $.jev_governance.mode.off);
      case "rule_mode":
        return String(raw ?? "");
      default:
        return null;
    }
  };
  const label = (change: SettingChange) => {
    switch (change.kind) {
      case "routing_mode":
        return t(($) => $.jev_governance.history.labels.routing_mode);
      case "min_confidence":
        return t(($) => $.jev_governance.confidence_label);
      case "rule_mode":
        return t(($) => $.jev_governance.history.labels.rule_mode);
      case "jev_governance_enabled":
        return t(($) => $.jev_governance.history.labels.jev_governance_enabled);
      case "jev_fallback_agents_enabled":
        return t(($) => $.jev_governance.history.labels.jev_fallback_agents_enabled);
      case "governance_corrections_enabled":
        return t(($) => $.jev_governance.history.labels.governance_corrections_enabled);
      case "limits":
        return t(($) => $.jev_governance.history.labels.limits);
      case "jev_api_url":
        return t(($) => $.jev_governance.history.labels.jev_api_url);
      default:
        return change.key;
    }
  };
  /** "Min confidence: 0.70 → 0.80", or "Limits changed" for a setting that is not printed. */
  const chip = (change: SettingChange) => {
    const before = value(change, "before");
    const after = value(change, "after");
    return before === null || after === null
      ? t(($) => $.jev_governance.history.opaque_changed, { label: label(change) })
      : t(($) => $.jev_governance.history.change, { label: label(change), before, after });
  };
  return { value, label, chip };
}

package issueworkflow

import (
	"encoding/json"
	"strings"
)

// SettingsKeyQuickCreateDiscovery is the workspace settings key (CHE-9075,
// CHE-6107 C1) that lets quick-create file an unclear request in Discovery:
// "off" (default, also any other value) | "on".
const SettingsKeyQuickCreateDiscovery = "quick_create_discovery"

// DiscoveryStatusKey is the status of a workflow's Discovery step.
const DiscoveryStatusKey = "investigating"

// QuickCreateDiscovery reports whether the workspace turned the setting on.
// Only an explicit "on" does; an unset, unreadable or unknown value is off.
func QuickCreateDiscovery(settings []byte) bool {
	var parsed struct {
		Discovery string `json:"quick_create_discovery"`
	}
	_ = json.Unmarshal(settings, &parsed)
	return strings.ToLower(strings.TrimSpace(parsed.Discovery)) == "on"
}

// DiscoveryFallback is the status a quick-create issue takes when its run asked
// for Discovery. The run cannot see which workflow the server will pick (a
// title type or an agent-chosen project can route the issue elsewhere), so the
// server decides on the workflow it picked: a workflow with the `investigating`
// step keeps the status; any other gets its start status. Every other status
// passes through unchanged.
func DiscoveryFallback(def *Definition, status string) string {
	if status != DiscoveryStatusKey || (def != nil && def.Has(DiscoveryStatusKey)) {
		return status
	}
	if def == nil {
		return StartStatus
	}
	return def.Start()
}

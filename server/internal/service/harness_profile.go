package service

import "encoding/json"

// Harness profile (CHE-3706): how the daemon builds the CLI's config home.
// Shown in the UI as "Runtime profile". Stored as `harness_profile` in
// workspace settings (default) and agent runtime_config (override); the key is
// distinct from the existing daemon "runtime profile" concept.
//
//	personal: the CLI runs with the human's own harness config (today).
//	managed:  the daemon generates a minimal allowlisted config home per task.
//
// "managed + sandbox" is reserved for OS isolation (CHE-1067) and not accepted.
const (
	HarnessProfilePersonal = "personal"
	HarnessProfileManaged  = "managed"
)

func ValidHarnessProfile(v string) bool {
	return v == HarnessProfilePersonal || v == HarnessProfileManaged
}

// ResolveHarnessProfile: agent override > workspace default > personal.
// Null, empty and unknown stored values inherit; the value is validated on
// write, so this only guards legacy data.
func ResolveHarnessProfile(workspaceSettings, agentConfig []byte) string {
	profile := HarnessProfilePersonal
	for _, raw := range [][]byte{workspaceSettings, agentConfig} {
		var layer struct {
			Profile string `json:"harness_profile"`
		}
		if json.Unmarshal(raw, &layer) == nil && ValidHarnessProfile(layer.Profile) {
			profile = layer.Profile
		}
	}
	return profile
}

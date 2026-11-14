package decisionaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

// Digest recipes of the config-change core. The workflow recipe is the CHE-2456
// workflowStepsHash and lives with the workflow write path.

// CodeDenylisted is the refusal code for a field or key outside the allow tables.
const CodeDenylisted = "denylisted"

// TextDigest is the digest of a text field: sha256 of the raw UTF-8 bytes of the
// stored value. A NULL column is passed as "" and hashes the same, as in the
// CHE-1300 owner CAS contract.
func TextDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// SettingsDigest is the digest of the named workspace settings keys: sha256 of
// the canonical JSON of {"keys": {<key>: <value or null>}, "workspace_id": <id>}.
// A missing key (nil or empty value) reads as null. One digest covers a whole
// patch, so a patch is one compare-and-swap.
func SettingsDigest(workspaceID string, keys map[string]json.RawMessage) (string, error) {
	named := make(map[string]json.RawMessage, len(keys))
	for key, value := range keys {
		if len(value) == 0 {
			value = json.RawMessage("null")
		}
		named[key] = value
	}
	raw, err := json.Marshal(map[string]any{"keys": named, "workspace_id": workspaceID})
	if err != nil {
		return "", invalid("settings value is not JSON: %v", err)
	}
	canonical, err := Canonical(raw)
	if err != nil {
		return "", invalid("settings are not canonicalizable: %v", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// configFields are the governed fields a config change may write (scope A). A
// field not listed is refused, which keeps project.description, members and
// roles, passkeys, owner transfer, secrets and the action registry out.
var configFields = []string{
	"agent.instructions", "squad.instructions", "workspace.context",
	"workflow.steps", "workflow.instructions", "workflow.budget", SettingsField,
	// Dedicated identities (CHE-4462 E0a): none is a settings key, so the generic
	// patch still refuses routing_agent and delegated_governed_writes.
	"workspace.routing_agent", "workspace.delegated_writes", "agent.skills",
}

// SettingsField is the field a workspace settings patch writes.
const SettingsField = "workspace.settings"

// settingsKeys is the allow table of plain operational workspace settings keys
// (CHE-3474). A key not listed is refused: the delegation setting, the decision
// gates, reviewer identity, merge switches, redaction, server-stamped keys and
// any key not yet written stay out.
var settingsKeys = []string{
	"compact_prompt", "auto_compact", "idle_started_guard", "wakeups_paused",
	"github_auto_link_prs_enabled", "github_pr_sidebar_enabled", "co_authored_by_enabled",
	"github_wake_on_pr_merge", "github_wake_on_ci_failure", "github_wake_on_ci_success", "github_wake_on_pr_attention",
	"pr_event_capture", "system_wakeup_child_done", "system_wakeup_child_done_instruction",
	"agent_comment_wake_policy_enabled", "mermaid_orientation",
	"provider_breaker_max_hold_seconds", "guard_policy",
}

// SettingsKeys lists the allow table keys.
func SettingsKeys() []string { return slices.Clone(settingsKeys) }

// CheckConfigChange refuses, with CodeDenylisted, a field outside the allow
// list, a settings patch that names no key or any key outside the allow table,
// and keys on any other field. It runs at request and again at approval.
func CheckConfigChange(field string, keys []string) error {
	if !slices.Contains(configFields, field) {
		return &Error{Code: CodeDenylisted, Message: "this field cannot be changed through a decision action"}
	}
	if field != SettingsField {
		if len(keys) > 0 {
			return &Error{Code: CodeDenylisted, Message: "only workspace settings take keys"}
		}
		return nil
	}
	if len(keys) == 0 {
		return &Error{Code: CodeDenylisted, Message: "a settings change must name its keys"}
	}
	for _, key := range keys {
		if !slices.Contains(settingsKeys, key) {
			return &Error{Code: CodeDenylisted, Message: "a settings key is not on the allow table"}
		}
	}
	return nil
}

// WorkflowDigest is the CHE-2456 workflow recipe: sha256 of the canonical JSON
// (keys sorted, numbers kept as written, no HTML escaping) of
// {"steps": <steps>, "workflow_id": <id>}. The server hashes the step payloads it
// stores; the CLI hashes the steps array as the workflow GET returns it.
func WorkflowDigest(workflowID string, steps json.RawMessage) (string, error) {
	raw, err := json.Marshal(map[string]any{"steps": steps, "workflow_id": workflowID})
	if err != nil {
		return "", invalid("steps are not JSON: %v", err)
	}
	var generic any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&generic); err != nil {
		return "", invalid("steps are not JSON: %v", err)
	}
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical) // a map encodes with its keys sorted
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(generic); err != nil {
		return "", invalid("steps are not canonicalizable: %v", err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:]), nil
}

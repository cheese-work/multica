package decisionaction

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestTextDigestHashesTheRawUTF8Bytes(t *testing.T) {
	for value, want := range map[string]string{
		"":      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // NULL reads as empty
		"héllo": "3c48591d8d098a4538f5e013dfcf406e948eac4d3277b10bf614e295d6068179",
	} {
		if got := TextDigest(value); got != want {
			t.Errorf("TextDigest(%q) = %s, want %s", value, got, want)
		}
	}
}

func TestSettingsDigestCoversOnlyTheNamedKeysAsCanonicalJSON(t *testing.T) {
	digest := func(workspace string, keys map[string]json.RawMessage) string {
		t.Helper()
		got, err := SettingsDigest(workspace, keys)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// sha256 of {"keys":{"compact_prompt":"x","mermaid_orientation":null},"workspace_id":"ws"}
	const pinned = "4f6f519ddf3e72827740472ffa706360bca0d8b1b33deffd4a308ee491d8d2b0"
	keys := map[string]json.RawMessage{"mermaid_orientation": nil, "compact_prompt": json.RawMessage(` "x" `)}
	if got := digest("ws", keys); got != pinned {
		t.Fatalf("digest = %s, want %s", got, pinned)
	}
	if got := digest("ws", map[string]json.RawMessage{"mermaid_orientation": json.RawMessage(`null`), "compact_prompt": json.RawMessage(`"x"`)}); got != pinned {
		t.Errorf("a missing key and an explicit null differ: %s", got)
	}
	if digest("other", keys) == pinned {
		t.Error("the digest does not bind the workspace")
	}
	keys["compact_prompt"] = json.RawMessage(`"y"`)
	if digest("ws", keys) == pinned {
		t.Error("the digest ignores a value change")
	}
	if _, err := SettingsDigest("ws", map[string]json.RawMessage{"compact_prompt": json.RawMessage(`{`)}); err == nil {
		t.Error("invalid JSON value hashed")
	}
}

func TestCheckConfigChangeDeniesByDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		keys  []string
		deny  bool
	}{
		{"agent instructions", "agent.instructions", nil, false},
		{"squad instructions", "squad.instructions", nil, false},
		{"workspace context", "workspace.context", nil, false},
		{"workflow steps", "workflow.steps", nil, false},
		{"workflow step instructions", "workflow.instructions", nil, false},
		{"allowed settings keys", "workspace.settings", []string{"compact_prompt", "wakeups_paused", "mermaid_orientation"}, false},
		{"provider hold deadline", "workspace.settings", []string{"provider_breaker_max_hold_seconds"}, false},
		{"guard policy", "workspace.settings", []string{"guard_policy"}, false},
		{"title type to workflow map", "workspace.settings", []string{"workflow_by_type"}, false},
		{"capability routing", "workspace.settings", []string{"capability_routing"}, false},
		{"review independence", "workspace.settings", []string{"review_independence"}, false},
		{"ask guard mode", "workspace.settings", []string{"ask_guard_mode"}, false},
		{"quick create discovery", "workspace.settings", []string{"quick_create_discovery"}, false},
		{"decision ask park", "workspace.settings", []string{"decision_ask_park"}, false},
		{"design card guard", "workspace.settings", []string{"design_card_guard"}, false},
		{"auto close mode", "workspace.settings", []string{"auto_close"}, false},
		{"blocked run skip mode", "workspace.settings", []string{"blocked_run_skip"}, false},
		{"run guard mode", "workspace.settings", []string{"run_guard"}, false},
		{"run budget mode", "workspace.settings", []string{"run_budget"}, false},
		{"wakeup live join mode", "workspace.settings", []string{"wakeup_live_join"}, false},
		{"both run-cutting modes", "workspace.settings", []string{"auto_close", "blocked_run_skip"}, false},
		{"project description is out of scope", "project.description", nil, true},
		{"members and roles", "member.role", nil, true},
		{"unknown field", "", nil, true},
		{"settings with no key", "workspace.settings", nil, true},
		{"the delegation setting", "workspace.settings", []string{"delegated_governed_writes"}, true},
		{"reviewer identity", "workspace.settings", []string{"github_review_members"}, true},
		{"live-only decision key", "workspace.settings", []string{"agent_decisions_jev"}, true},
		{"server-stamped key", "workspace.settings", []string{"wakeups_paused_by"}, true},
		{"unknown key", "workspace.settings", []string{"never_heard_of_it"}, true},
		{"one denied key among allowed", "workspace.settings", []string{"compact_prompt", "always_redact_env"}, true},
		{"keys on a non-settings field", "agent.instructions", []string{"compact_prompt"}, true},
		{"routing agent identity", "workspace.routing_agent", nil, false},
		{"delegated writes identity", "workspace.delegated_writes", nil, false},
		{"agent skills identity", "agent.skills", nil, false},
		{"keys on the routing agent field", "workspace.routing_agent", []string{"routing_agent"}, true},
		{"keys on the delegated writes field", "workspace.delegated_writes", []string{"delegated_governed_writes"}, true},
		{"keys on the agent skills field", "agent.skills", []string{"compact_prompt"}, true},
		{"settings patch of routing_agent", "workspace.settings", []string{"routing_agent"}, true},
		{"settings patch of delegated_governed_writes", "workspace.settings", []string{"delegated_governed_writes", "compact_prompt"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckConfigChange(tc.field, tc.keys)
			var coded *Error
			switch {
			case tc.deny && (!errors.As(err, &coded) || coded.Code != CodeDenylisted):
				t.Fatalf("CheckConfigChange(%q, %v) = %v, want %s", tc.field, tc.keys, err, CodeDenylisted)
			case !tc.deny && err != nil:
				t.Fatalf("CheckConfigChange(%q, %v) = %v, want nil", tc.field, tc.keys, err)
			}
		})
	}
}

// CHE-9340: run_guard may carry the stop-loss mode next to its own mode, so the
// switch-on of the stop-loss goes through the governed settings path.
func TestValidateModeSettingRunGuardStopLoss(t *testing.T) {
	for name, tc := range map[string]struct {
		key, value string
		ok         bool
	}{
		"run_guard with stop_loss block": {"run_guard", `{"mode":"warn","stop_loss":{"mode":"block"}}`, true},
		"run_guard with stop_loss off":   {"run_guard", `{"mode":"block","stop_loss":{"mode":"off"}}`, true},
		"stop_loss alone":                {"run_guard", `{"stop_loss":{"mode":"block"}}`, false},
		"stop_loss bogus mode":           {"run_guard", `{"mode":"warn","stop_loss":{"mode":"bogus"}}`, false},
		"stop_loss not an object":        {"run_guard", `{"mode":"warn","stop_loss":"block"}`, false},
		"stop_loss extra field":          {"run_guard", `{"mode":"warn","stop_loss":{"mode":"block","x":1}}`, false},
		"unknown sibling":                {"run_guard", `{"mode":"warn","other":{"mode":"block"}}`, false},
		"other key rejects stop_loss":    {"run_budget", `{"mode":"warn","stop_loss":{"mode":"block"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateModeSetting(tc.key, json.RawMessage(tc.value))
			if tc.ok && err != nil {
				t.Fatalf("refused %s: %v", tc.value, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("accepted %s", tc.value)
			}
		})
	}
}

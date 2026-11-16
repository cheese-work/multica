package handler

import (
	"encoding/json"
	"testing"
)

func TestGuardPolicySettingValidation(t *testing.T) {
	good := `{"version":1,"plugins":[{"name":"x","match":{"tool":"gh"},"action":"deny","message":"no"}],
	  "layers":[{"scope":"org","org":"acme","locked":true,"verify":false}]}`
	if err := validateSettingsPatchValue("guard_policy", json.RawMessage(good)); err != nil {
		t.Fatalf("valid policy refused: %v", err)
	}
	for name, raw := range map[string]string{
		"not an object":  `"x"`,
		"wrong version":  `{"version":2}`,
		"script field":   `{"version":1,"plugins":[],"script":"rm -rf /"}`,
		"unknown action": `{"version":1,"plugins":[{"name":"x","match":{"tool":"gh"},"action":"run"}]}`,
		"bad layer":      `{"version":1,"layers":[{"scope":"team"}]}`,
	} {
		if err := validateSettingsPatchValue("guard_policy", json.RawMessage(raw)); err == nil {
			t.Errorf("%s: invalid policy accepted", name)
		}
	}
	if err := validateGuardPolicySetting(map[string]any{"guard_policy": map[string]any{"version": 2}}); err == nil {
		t.Error("generic save accepted an invalid policy")
	}
	if err := validateGuardPolicySetting(map[string]any{"other": 1}); err != nil {
		t.Errorf("settings without the key refused: %v", err)
	}
}

// CHE-5946: workflow_by_type goes through the settings patch with its own validator.
func TestWorkflowByTypeSettingValidation(t *testing.T) {
	good := `{"critic":"7328545d-f16f-4bdb-9deb-27f5d70b1210"}`
	if err := validateSettingsPatchValue("workflow_by_type", json.RawMessage(good)); err != nil {
		t.Fatalf("valid map refused: %v", err)
	}
	for name, raw := range map[string]string{
		"not an object":    `"critic"`,
		"value not a uuid": `{"critic":"gate"}`,
		"empty map":        `{}`,
	} {
		if err := validateSettingsPatchValue("workflow_by_type", json.RawMessage(raw)); err == nil {
			t.Errorf("%s: invalid map accepted", name)
		}
	}
}

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

const intakeHoldProject = "7328545d-f16f-4bdb-9deb-27f5d70b1210"

func intakeHoldEntry(extra string) string {
	return `[{"project_id":"` + intakeHoldProject + `","mode":"block","starts_at":"2026-11-18T00:00:00Z","ends_at":"2026-11-25T00:00:00Z","source":"CHE-6911"` + extra + `}]`
}

var badIntakeHold = map[string]string{
	"not an array":            `{"project_id":"x"}`,
	"unknown field":           intakeHoldEntry(`,"script":"rm"`),
	"mode outside the set":    `[{"project_id":"` + intakeHoldProject + `","mode":"loud","starts_at":"2026-11-18T00:00:00Z","ends_at":"2026-11-25T00:00:00Z","source":"s"}]`,
	"ends before starts":      `[{"project_id":"` + intakeHoldProject + `","mode":"block","starts_at":"2026-11-25T00:00:00Z","ends_at":"2026-11-18T00:00:00Z","source":"s"}]`,
	"project not a uuid":      `[{"project_id":"p","mode":"block","starts_at":"2026-11-18T00:00:00Z","ends_at":"2026-11-25T00:00:00Z","source":"s"}]`,
	"class outside the five":  intakeHoldEntry(`,"exempt_escalation_classes":["fun"]`),
	"two entries one project": `[` + intakeHoldEntry(``)[1:len(intakeHoldEntry(``))-1] + `,` + intakeHoldEntry(``)[1:len(intakeHoldEntry(``))-1] + `]`,
}

func TestIntakeHoldSettingValidation(test *testing.T) {
	if err := validateSettingsPatchValue("intake_hold", json.RawMessage(intakeHoldEntry(`,"exempt_types":["critic"],"exempt_escalation_classes":["spend"]`))); err != nil {
		test.Fatalf("valid entry refused: %v", err)
	}
	if err := validateSettingsPatchValue("intake_hold", json.RawMessage(`[]`)); err != nil {
		test.Fatalf("empty list refused: %v", err)
	}
	for name, raw := range badIntakeHold {
		if err := validateSettingsPatchValue("intake_hold", json.RawMessage(raw)); err == nil {
			test.Errorf("patch: %s accepted", name)
		}
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			test.Fatal(err)
		}
		if err := validateIntakeHoldSetting(map[string]any{"intake_hold": value}); err == nil {
			test.Errorf("ordinary save: %s accepted", name)
		}
	}
	if err := validateIntakeHoldSetting(map[string]any{"other": 1}); err != nil {
		test.Errorf("settings without the key refused: %v", err)
	}
	if err := validateIntakeHoldSetting(map[string]any{"intake_hold": nil}); err != nil {
		test.Errorf("null refused: %v", err)
	}
}

func TestIntakeHoldIsAnAllowTableKey(test *testing.T) {
	if err := decisionaction.CheckConfigChange(decisionaction.SettingsField, []string{"intake_hold"}); err != nil {
		test.Fatalf("intake_hold is not patchable: %v", err)
	}
}

func TestIntakeHoldSettingPatchStoresAndRemoves(test *testing.T) {
	var entries any
	if err := json.Unmarshal([]byte(intakeHoldEntry(``)), &entries); err != nil {
		test.Fatal(err)
	}
	patch := map[string]any{"intake_hold": entries}
	request, _ := requestSettings(test, http.StatusCreated, patch, nil)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	if got := string(storedSettings(test)["intake_hold"]); got == "" || got == "null" {
		test.Fatalf("intake_hold not stored: %q", got)
	}

	removal, _ := fileSettings(test, http.StatusCreated, map[string]any{
		"workspace_id": testWorkspaceID, "settings": map[string]any{"intake_hold": nil},
		"base_sha256": liveSettingsDigest(test, "intake_hold"), "after_sha256": patchDigest(test, map[string]any{"intake_hold": nil}),
	})
	removal.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	if _, present := storedSettings(test)["intake_hold"]; present {
		test.Fatal("a null in the patch must remove intake_hold")
	}
}

func TestIntakeHoldSettingPatchRefusesInvalid(test *testing.T) {
	for name, raw := range badIntakeHold {
		test.Run(name, func(test *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				test.Fatal(err)
			}
			_, body := requestSettings(test, http.StatusBadRequest, map[string]any{"intake_hold": value}, nil)
			mustContain(test, body, "invalid_params")
		})
	}
}

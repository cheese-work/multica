package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/reviewindependence"
)

// review_independence (CHE-9319): written only through the settings patch.

func reviewIndependenceValue() map[string]any {
	return map[string]any{
		"attribute": "lab", "rule": reviewindependence.Rule,
		"agent_values":      map[string]any{"claude/*": "anthropic", "codex/*": "openai"},
		"commit_identities": map[string]any{"anthropic.com": "anthropic"},
	}
}

func TestReviewIndependenceSettingValidation(t *testing.T) {
	good, _ := json.Marshal(reviewIndependenceValue())
	if err := validateSettingsPatchValue(reviewindependence.SettingsKey, good); err != nil {
		t.Fatalf("valid setting refused: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"no attribute": func(m map[string]any) { delete(m, "attribute") },
		"unknown rule": func(m map[string]any) { m["rule"] = "other" },
		"bad agent key": func(m map[string]any) {
			m["agent_values"] = map[string]any{"claude": "x"}
		},
		"suffix identity": func(m map[string]any) {
			m["commit_identities"] = map[string]any{"*.anthropic.com": "x"}
		},
	} {
		value := reviewIndependenceValue()
		mutate(value)
		raw, _ := json.Marshal(value)
		if err := validateSettingsPatchValue(reviewindependence.SettingsKey, raw); err == nil {
			t.Errorf("%s: invalid setting accepted", name)
		}
	}
}

func TestSettingsPatchReviewIndependenceIsAccepted(t *testing.T) {
	requestSettings(t, http.StatusCreated, map[string]any{reviewindependence.SettingsKey: reviewIndependenceValue()}, nil)
	_, body := requestSettings(t, http.StatusBadRequest, map[string]any{reviewindependence.SettingsKey: map[string]any{"attribute": "lab"}}, nil)
	mustContain(t, body, "invalid_params")
}

func TestReviewIndependenceGenericSettingsSaveKeepsIt(t *testing.T) {
	wsID := pauseWorkspaceFixture(t, "ri-generic")
	value := reviewIndependenceValue()
	stored, _ := json.Marshal(map[string]any{reviewindependence.SettingsKey: value})
	dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, wsID, string(stored))

	for name, settings := range map[string]map[string]any{
		"replace": {reviewindependence.SettingsKey: map[string]any{"attribute": "x"}},
		"null":    {reviewindependence.SettingsKey: nil},
		"omit":    {"github_pr_sidebar_enabled": false},
	} {
		got := saveWorkspaceSettings(t, wsID, settings)
		have, _ := json.Marshal(got[reviewindependence.SettingsKey])
		want, _ := json.Marshal(value)
		if string(have) != string(want) {
			t.Fatalf("%s: generic save left %s, want %s", name, have, want)
		}
	}
	other := pauseWorkspaceFixture(t, "ri-generic-unset")
	if got := saveWorkspaceSettings(t, other, map[string]any{reviewindependence.SettingsKey: value}); got[reviewindependence.SettingsKey] != nil {
		t.Fatalf("generic save set the setting: %v", got)
	}
}

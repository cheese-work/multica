package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/agentpreset"
	"github.com/multica-ai/multica/server/internal/capabilityrouting"
)

// capability_routing (CHE-7666): type -> author and reviewers, written only
// through the settings patch (a passkey card), never by a generic save.

func capabilityTable(author string, reviewers ...string) map[string]any {
	ids := make([]any, len(reviewers))
	for i, id := range reviewers {
		ids[i] = id
	}
	return map[string]any{"feat": map[string]any{"author": author, "reviewers": ids}}
}

func TestCapabilityRoutingSettingValidation(t *testing.T) {
	a, b := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	good, _ := json.Marshal(capabilityTable(a, b))
	if err := validateSettingsPatchValue(capabilityrouting.SettingsKey, good); err != nil {
		t.Fatalf("valid table refused: %v", err)
	}
	for name, table := range map[string]any{
		"not an object":   "feat",
		"empty table":     map[string]any{},
		"empty reviewers": capabilityTable(a),
		"author reviews":  capabilityTable(a, a),
		"bad uuid":        capabilityTable("sonnet", b),
	} {
		raw, _ := json.Marshal(table)
		if err := validateSettingsPatchValue(capabilityrouting.SettingsKey, raw); err == nil {
			t.Errorf("%s: invalid table accepted", name)
		}
	}
}

func TestSettingsPatchCapabilityRoutingNeedsWorkspaceAgents(t *testing.T) {
	author := createHandlerTestAgent(t, "cr author", []byte("[]"))
	reviewer := createHandlerTestAgent(t, "cr reviewer", []byte("[]"))
	ghost := "00000000-0000-4000-8000-000000000009"

	_, body := requestSettings(t, http.StatusBadRequest, map[string]any{capabilityrouting.SettingsKey: capabilityTable(author, ghost)}, nil)
	mustContain(t, body, "invalid_params")
	mustContain(t, body, ghost)
	_, body = requestSettings(t, http.StatusBadRequest, map[string]any{capabilityrouting.SettingsKey: capabilityTable(ghost, reviewer)}, nil)
	mustContain(t, body, ghost)
	// A valid table is accepted at request time.
	requestSettings(t, http.StatusCreated, map[string]any{capabilityrouting.SettingsKey: capabilityTable(author, reviewer)}, nil)
}

func TestCapabilityRoutingGenericSettingsSaveKeepsIt(t *testing.T) {
	wsID := pauseWorkspaceFixture(t, "cr-generic")
	a, b := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	table := capabilityTable(a, b)
	stored, _ := json.Marshal(map[string]any{capabilityrouting.SettingsKey: table})
	dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, wsID, string(stored))

	// A generic save cannot replace, change or drop the table.
	for name, settings := range map[string]map[string]any{
		"replace": {capabilityrouting.SettingsKey: capabilityTable(b, a)},
		"null":    {capabilityrouting.SettingsKey: nil},
		"omit":    {"github_pr_sidebar_enabled": false},
	} {
		got := saveWorkspaceSettings(t, wsID, settings)
		have, _ := json.Marshal(got[capabilityrouting.SettingsKey])
		want, _ := json.Marshal(table)
		if string(have) != string(want) {
			t.Fatalf("%s: generic save left %s, want %s", name, have, want)
		}
	}
	// A generic save cannot set it on a workspace that has none.
	other := pauseWorkspaceFixture(t, "cr-generic-unset")
	if got := saveWorkspaceSettings(t, other, map[string]any{capabilityrouting.SettingsKey: table}); got[capabilityrouting.SettingsKey] != nil {
		t.Fatalf("generic save set the table: %v", got)
	}
}

func TestAgentLabReadsTheRuntimeProviderAndModel(t *testing.T) {
	t.Setenv("MULTICA_VOTE_GATE_LABS", `{"codex/gpt-5.5":"openai","claude/claude-sonnet-5-5":"anthropic"}`)
	if got := presetLabs().Of("codex", "gpt-5.5"); got != "openai" {
		t.Fatalf("lab = %q, want openai", got)
	}
	var _ agentpreset.Labs = presetLabs()
}

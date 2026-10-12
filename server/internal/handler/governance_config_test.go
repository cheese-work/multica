package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestGovernanceConfigAdminIdempotencyAndEpoch(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceConfigFixture(t)
	t.Cleanup(func() { resetGovernanceConfigFixture(t) })
	requestID := uuid.NewString()
	body := []byte(`{"expected_version":0,"request_id":"` + requestID + `","jev_governance_enabled":true}`)
	first := invokeGovernanceConfig(t, testUserID, http.MethodPatch, body, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first config update = %d: %s", first.Code, first.Body.String())
	}
	firstConfig := decodeGovernanceConfig(t, first)
	if firstConfig["version"] != float64(1) || firstConfig["control_epoch"] != float64(1) {
		t.Fatalf("first config version/epoch = %v/%v, want 1/1", firstConfig["version"], firstConfig["control_epoch"])
	}

	replay := invokeGovernanceConfig(t, testUserID, http.MethodPatch, body, "")
	if replay.Code != http.StatusOK {
		t.Fatalf("idempotent replay = %d: %s", replay.Code, replay.Body.String())
	}
	replayConfig := decodeGovernanceConfig(t, replay)
	if replayConfig["version"] != float64(1) || replayConfig["control_epoch"] != float64(1) {
		t.Fatalf("replay changed version/epoch to %v/%v", replayConfig["version"], replayConfig["control_epoch"])
	}

	conflictingReplay := invokeGovernanceConfig(t, testUserID, http.MethodPatch,
		[]byte(`{"expected_version":0,"request_id":"`+requestID+`","jev_governance_enabled":false}`), "")
	if conflictingReplay.Code != http.StatusConflict {
		t.Fatalf("idempotency-key conflict = %d: %s", conflictingReplay.Code, conflictingReplay.Body.String())
	}

	disable := invokeGovernanceConfig(t, testUserID, http.MethodPatch,
		[]byte(`{"expected_version":1,"request_id":"`+uuid.NewString()+`","jev_governance_enabled":false}`), "")
	if disable.Code != http.StatusOK {
		t.Fatalf("master disable = %d: %s", disable.Code, disable.Body.String())
	}
	disabledConfig := decodeGovernanceConfig(t, disable)
	if disabledConfig["version"] != float64(2) || disabledConfig["control_epoch"] != float64(2) {
		t.Fatalf("disable version/epoch = %v/%v, want 2/2", disabledConfig["version"], disabledConfig["control_epoch"])
	}
}

func TestGovernanceConfigRejectsAgentsAndNonAdminsWithSafeErrors(t *testing.T) {
	if testHandler == nil || dbfx == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceConfigFixture(t)
	t.Cleanup(func() { resetGovernanceConfigFixture(t) })
	memberID := dbfx.User(t, "Governance config member", uuid.NewString()+"@multica.test")
	dbfx.Member(t, testWorkspaceID, memberID, "member")
	body := []byte(`{"expected_version":0,"request_id":"` + uuid.NewString() + `","jev_fallback_agents_enabled":true}`)
	memberResponse := invokeGovernanceConfig(t, memberID, http.MethodPatch, body, "")
	if memberResponse.Code != http.StatusForbidden {
		t.Fatalf("member update = %d: %s", memberResponse.Code, memberResponse.Body.String())
	}
	assertSafeGovernanceProblem(t, memberResponse)

	agentResponse := invokeGovernanceConfig(t, testUserID, http.MethodPatch, body, "task_token")
	if agentResponse.Code != http.StatusForbidden {
		t.Fatalf("agent update = %d: %s", agentResponse.Code, agentResponse.Body.String())
	}
	assertSafeGovernanceProblem(t, agentResponse)
}

func TestGovernanceConfigReportsLimitClassificationHistoryAndRollback(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceConfigFixture(t)
	t.Cleanup(func() { resetGovernanceConfigFixture(t) })
	firstVersion := invokeGovernanceConfig(t, testUserID, http.MethodPatch,
		[]byte(`{"expected_version":0,"request_id":"`+uuid.NewString()+`","limits":{"max_refreshes":2}}`), "")
	if firstVersion.Code != http.StatusOK {
		t.Fatalf("set first limit = %d: %s", firstVersion.Code, firstVersion.Body.String())
	}
	secondVersion := invokeGovernanceConfig(t, testUserID, http.MethodPatch,
		[]byte(`{"expected_version":1,"request_id":"`+uuid.NewString()+`","limits":{"max_refreshes":3}}`), "")
	if secondVersion.Code != http.StatusOK {
		t.Fatalf("set second limit = %d: %s", secondVersion.Code, secondVersion.Body.String())
	}
	rollback := invokeGovernanceConfig(t, testUserID, http.MethodPatch,
		[]byte(`{"expected_version":2,"request_id":"`+uuid.NewString()+`","rollback_to_version":1}`), "")
	if rollback.Code != http.StatusOK {
		t.Fatalf("rollback config = %d: %s", rollback.Code, rollback.Body.String())
	}
	result := decodeGovernanceConfig(t, rollback)
	settings := result["settings"].(map[string]any)
	limits := settings["limits"].(map[string]any)
	if limits["max_refreshes"] != float64(2) {
		t.Fatalf("rollback max_refreshes = %v, want 2", limits["max_refreshes"])
	}
	descriptions := result["operating_limits"].([]any)
	var found bool
	for _, raw := range descriptions {
		limit := raw.(map[string]any)
		if limit["key"] != "max_refreshes" {
			continue
		}
		found = true
		if limit["classification"] != "workspace_setting" || limit["owner"] == "" || limit["rationale"] == "" || limit["allowed_range"] == "" || limit["effective_source"] != "workspace" {
			t.Fatalf("incomplete limit description: %+v", limit)
		}
		if len(limit["audit_history"].([]any)) < 3 || len(limit["rollback_to_versions"].([]any)) < 3 {
			t.Fatalf("limit history/rollback missing: %+v", limit)
		}
	}
	if !found {
		t.Fatal("max_refreshes missing from operating limit catalog")
	}
}

func TestGovernanceConfigSafeErrorsDoNotEchoInput(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceConfigFixture(t)
	t.Cleanup(func() { resetGovernanceConfigFixture(t) })
	response := invokeGovernanceConfig(t, testUserID, http.MethodPatch,
		[]byte(`{"expected_version":0,"request_id":"`+uuid.NewString()+`","api_key":"sentinel-private-value"}`), "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid config request = %d: %s", response.Code, response.Body.String())
	}
	assertSafeGovernanceProblem(t, response)
	if strings.Contains(response.Body.String(), "sentinel-private-value") || strings.Contains(response.Body.String(), "api_key") {
		t.Fatalf("safe error echoed private input: %s", response.Body.String())
	}
}

func resetGovernanceConfigFixture(t *testing.T) {
	t.Helper()
	if testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	requireGovernanceConfigTables(t)
	if _, err := testPool.Exec(context.Background(), `DELETE FROM governance_workspace_config_audit WHERE workspace_id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `DELETE FROM governance_workspace_config WHERE workspace_id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
}

func useCompleteGovernanceConfigFixture(t *testing.T) {
	t.Helper()
	resetGovernanceConfigFixture(t)
	_, err := testPool.Exec(context.Background(), `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2::jsonb)
	`, testWorkspaceID, `{"jev_governance_enabled":true,"jev_fallback_agents_enabled":true,"governance_corrections_enabled":true,"rule_mode":"correction","limits":{"max_refreshes":4,"max_evaluations":8,"admission_window_seconds":3600,"workspace_spend_cap_micro_usd":1000000,"pricing_policy":{"version":"test-v1","input_micro_usd_per_million_tokens":100000,"output_micro_usd_per_million_tokens":0}}}`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetGovernanceConfigFixture(t) })
}

func requireGovernanceConfigTables(t *testing.T) {
	t.Helper()
	if testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	var ready bool
	if err := testPool.QueryRow(context.Background(), `
		SELECT to_regclass('governance_workspace_config') IS NOT NULL
		   AND to_regclass('governance_workspace_config_audit') IS NOT NULL
	`).Scan(&ready); err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Skip("governance workspace config migrations are not applied to the test database")
	}
}

func invokeGovernanceConfig(t *testing.T, userID, method string, body []byte, actorSource string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/workspaces/"+testWorkspaceID+"/governance/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	if actorSource != "" {
		req.Header.Set("X-Actor-Source", actorSource)
	}
	req = withURLParam(req, "id", testWorkspaceID)
	recorder := httptest.NewRecorder()
	switch method {
	case http.MethodGet:
		testHandler.GetGovernanceConfig(recorder, req)
	case http.MethodPatch:
		testHandler.PatchGovernanceConfig(recorder, req)
	default:
		t.Fatalf("unsupported test method %s", method)
	}
	return recorder
}

func decodeGovernanceConfig(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		t.Fatalf("decode governance config response: %v: %s", err, response.Body.String())
	}
	return config
}

func assertSafeGovernanceProblem(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var problem map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode safe error: %v: %s", err, response.Body.String())
	}
	for _, field := range []string{"problem", "cause", "permitted_fix", "retryable", "correlation_id", "documentation_link"} {
		value, ok := problem[field]
		if !ok || value == "" {
			t.Errorf("safe error field %q missing: %s", field, response.Body.String())
		}
	}
}

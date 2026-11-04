package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentDecisionRelyCommand(t *testing.T) {
	const issue = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/issues/CHE-2643" {
			json.NewEncoder(w).Encode(map[string]string{"id": issue})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/issues/"+issue+"/agent-decisions/rely" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["gate"] != "screenshot_evidence" || body["purpose"] != "gate check" {
			t.Errorf("wrong binding: %v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"decided": false, "state": "not decided"})
	}))
	defer server.Close()
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	cmd, _, err := rootCmd.Find([]string{"decision", "rely"})
	if err != nil || cmd.Name() != "rely" {
		t.Fatal("decision rely is unavailable")
	}
	for key, value := range map[string]string{"issue": "CHE-2643", "gate": "screenshot_evidence", "candidate-sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "purpose": "gate check"} {
		if err := cmd.Flags().Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	out, err := captureStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("rely: %q, %v", out, err)
	}
}

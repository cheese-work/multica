package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentDecisionBriefAndSettingCommands(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPut {
			var body map[string]bool
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["enabled"] != true {
				t.Errorf("setting body: %v", body)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"enabled": true})
	}))
	defer server.Close()
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	brief, _, err := rootCmd.Find([]string{"decision", "brief"})
	if err != nil || brief.Name() != "brief" {
		t.Fatal("decision brief is unavailable")
	}
	if out, err := captureStdout(t, func() error { return brief.RunE(brief, nil) }); err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("brief: %q, %v", out, err)
	}
	setting, _, err := rootCmd.Find([]string{"decision", "agent-setting"})
	if err != nil || setting.Name() != "agent-setting" {
		t.Fatal("decision agent-setting is unavailable")
	}
	if err := setting.Flags().Set("enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return setting.RunE(setting, nil) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /api/agent-decisions/brief", "PUT /api/agent-decisions/setting"}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("requests=%v want %v", seen, want)
	}
}

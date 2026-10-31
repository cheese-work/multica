package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAgentDecisionClosedRoutes(t *testing.T) {
	f := workspaceFixture(t)
	issue := f.Issue(t, "Disabled agent settlement")
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/api/issues/" + issue + "/agent-decisions"
		if method == http.MethodGet {
			path += "?gate=screenshot_evidence&candidate_sha256=" + strings.Repeat("a", 64)
		} else {
			path += "/rely"
		}
		req, _ := http.NewRequest(method, testServer.URL+path, strings.NewReader(`{"gate":"screenshot_evidence","candidate_sha256":"`+strings.Repeat("a", 64)+`","purpose":"test"}`))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		req.Header.Set("Content-Type", "application/json")
		response, err := testServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Decided       bool `json:"decided"`
			Authorization bool `json:"authorization"`
		}
		err = json.NewDecoder(response.Body).Decode(&out)
		response.Body.Close()
		if method == http.MethodGet && (response.StatusCode != 200 || err != nil || out.Decided || out.Authorization) {
			t.Errorf("lookup: status=%d, result=%+v, error=%v; want display-only closed response", response.StatusCode, out, err)
		}
		if method == http.MethodPost && response.StatusCode != http.StatusForbidden {
			t.Errorf("human reliance status=%d, want 403", response.StatusCode)
		}
	}
}

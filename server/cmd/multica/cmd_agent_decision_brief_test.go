package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestAgentDecisionBriefMarkdownSection(t *testing.T) {
	feed := map[string]any{
		"enabled": true,
		"decisions": []map[string]any{
			{"instance_id": "i1", "identifier": "CHE-9", "gate": "screenshot_evidence", "state": "effective", "review_target_at": "2026-10-31T10:00:00Z",
				"link": "https://app/x/issues/CHE-9#comment-c1", "veto_line": "Veto: decision:i1 sha member:u1"},
			{"instance_id": "i2", "identifier": "CHE-8", "gate": "design_lock_label", "state": "revoked", "revoke_reason": "wrong", "review_target_at": "2026-10-30T10:00:00Z",
				"link": "https://app/x/issues/CHE-8#comment-c2", "veto_line": "Veto: decision:i2 sha member:u1"},
		},
		"notice_failures": []map[string]any{{"instance_id": "i1", "attempts": 2, "last_error": "boom"}},
		"stopped":         map[string]any{"reason": "excluded gate", "at": "2026-10-31T01:00:00Z"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(feed) }))
	defer server.Close()
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	brief, _, _ := rootCmd.Find([]string{"decision", "brief"})
	if err := brief.Flags().Set("format", "markdown"); err != nil {
		t.Fatal(err)
	}
	defer brief.Flags().Set("format", "json")
	out, err := captureStdout(t, func() error { return brief.RunE(brief, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Agents decided — veto?", "CHE-9", "https://app/x/issues/CHE-9#comment-c1", "Veto: decision:i1 sha member:u1",
		"2026-10-31 17:00 ICT", "revoked", "wrong", "Notice delivery failed", "boom", "excluded gate"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Needs you") {
		t.Errorf("section must stand outside Needs you:\n%s", out)
	}
}

func TestAgentDecisionBriefMarkdownIsEmptyWhenOffAndNothingToShow(t *testing.T) {
	record := map[string]any{"identifier": "CHE-9", "gate": "screenshot_evidence", "state": "effective", "review_target_at": "2026-10-31T10:00:00Z",
		"link": "https://app/x/issues/CHE-9#comment-c1", "veto_line": "Veto: decision:i1 sha member:u1"}
	cases := []struct {
		name      string
		feed      map[string]any
		wantEmpty bool
	}{
		{"off and empty", map[string]any{"enabled": false, "decisions": []any{}, "stopped": nil}, true},
		{"off with a record still vetoable", map[string]any{"enabled": false, "decisions": []any{record}}, false},
		{"off after a stop", map[string]any{"enabled": false, "decisions": []any{}, "stopped": map[string]any{"reason": "excluded gate", "at": "2026-10-31T01:00:00Z"}}, false},
		{"off with a failed notice", map[string]any{"enabled": false, "decisions": []any{}, "notice_failures": []any{map[string]any{"instance_id": "i1", "attempts": 1, "last_error": "boom"}}}, false},
		{"on and empty", map[string]any{"enabled": true, "decisions": []any{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			if err := writeAgentDecisionMarkdown(&buf, tc.feed); err != nil {
				t.Fatal(err)
			}
			if got := buf.Len() == 0; got != tc.wantEmpty {
				t.Fatalf("empty=%v want %v; output:\n%s", got, tc.wantEmpty, buf.String())
			}
		})
	}
}

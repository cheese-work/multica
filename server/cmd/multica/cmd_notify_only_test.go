package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueCommentNotifyOnlyFlag(t *testing.T) {
	if issueCommentAddCmd.Flags().Lookup("notify-only") == nil {
		t.Fatal("comment add lacks --notify-only")
	}
}

func TestIssueCommentNotifyOnlyBody(t *testing.T) {
	const issueID = "11111111-1111-4111-8111-111111111111"
	for _, set := range []bool{true, false} {
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": issueID, "identifier": "TST-1"})
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": issueID})
		}))
		t.Setenv("MULTICA_SERVER_URL", server.URL)
		t.Setenv("MULTICA_TOKEN", "mat_test-token")
		t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
		cmd := newIssueCommentAddTestCmd()
		cmd.Flags().Bool("notify-only", set, "")
		_ = cmd.Flags().Set("content", "Receipt posted")
		if _, err := captureStdout(t, func() error { return runIssueCommentAdd(cmd, []string{issueID}) }); err != nil {
			t.Fatal(err)
		}
		server.Close()
		if _, has := body["notify_only"]; has != set {
			t.Fatalf("notify-only=%v, body %+v", set, body)
		}
	}
}

func TestIssueCommentNotifyOnlyRefusesTypedKinds(t *testing.T) {
	for _, kind := range []string{"decision-intent", "decision-record", "decision-veto"} {
		cmd := newIssueCommentAddTestCmd()
		cmd.Flags().Bool("notify-only", true, "")
		cmd.Flags().Bool(kind, true, "")
		if kind != "decision-record" {
			cmd.Flags().Bool("decision-record", false, "")
		}
		_ = cmd.Flags().Set("content", "x")
		if err := runIssueCommentAdd(cmd, []string{"11111111-1111-4111-8111-111111111111"}); err == nil || !strings.Contains(err.Error(), "--notify-only cannot combine") {
			t.Errorf("--notify-only with --%s: err = %v, want a local refusal", kind, err)
		}
	}
}

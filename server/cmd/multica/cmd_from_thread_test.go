package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueCommentFromThreadFlag(t *testing.T) {
	if issueCommentAddCmd.Flags().Lookup("from-thread") == nil {
		t.Fatal("comment add lacks --from-thread")
	}
}

func TestIssueCommentFromThreadBody(t *testing.T) {
	const (
		issueID = "11111111-1111-4111-8111-111111111111"
		source  = "66666666-6666-4666-8666-666666666666"
	)
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
		cmd.Flags().String("from-thread", "", "")
		if set {
			_ = cmd.Flags().Set("from-thread", source)
		}
		_ = cmd.Flags().Set("content", "One question")
		if _, err := captureStdout(t, func() error { return runIssueCommentAdd(cmd, []string{issueID}) }); err != nil {
			t.Fatal(err)
		}
		server.Close()
		got, has := body["from_thread"]
		if has != set || (set && got != source) {
			t.Fatalf("from-thread set=%v, body %+v", set, body)
		}
	}
}

func TestIssueCommentFromThreadRefusesParent(t *testing.T) {
	cmd := newIssueCommentAddTestCmd()
	cmd.Flags().String("from-thread", "", "")
	_ = cmd.Flags().Set("from-thread", "66666666-6666-4666-8666-666666666666")
	_ = cmd.Flags().Set("parent", "77777777-7777-4777-8777-777777777777")
	_ = cmd.Flags().Set("content", "x")
	err := runIssueCommentAdd(cmd, []string{"11111111-1111-4111-8111-111111111111"})
	if err == nil || !strings.Contains(err.Error(), "--from-thread opens a new root and cannot combine with --parent") {
		t.Errorf("err = %v, want a local refusal", err)
	}
}

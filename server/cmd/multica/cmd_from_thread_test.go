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

// CHE-6432: the create body tells the server the author will resolve the thread,
// so the long-thread route posts in place instead of moving the comment.
func TestIssueCommentResolveThreadBody(t *testing.T) {
	const (
		issueID  = "11111111-1111-4111-8111-111111111111"
		parentID = "77777777-7777-4777-8777-777777777777"
	)
	for _, set := range []bool{true, false} {
		var body map[string]any
		resolved := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]any{"id": issueID, "identifier": "TST-1"})
			case strings.HasSuffix(r.URL.Path, "/resolve"):
				resolved = true
				_ = json.NewEncoder(w).Encode(map[string]any{})
			default:
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "88888888-8888-4888-8888-888888888888"})
			}
		}))
		t.Setenv("MULTICA_SERVER_URL", server.URL)
		t.Setenv("MULTICA_TOKEN", "mat_test-token")
		t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
		cmd := newIssueCommentAddTestCmd()
		cmd.Flags().String("from-thread", "", "")
		_ = cmd.Flags().Set("parent", parentID)
		_ = cmd.Flags().Set("content", "Done")
		if set {
			_ = cmd.Flags().Set("resolve-thread", "true")
		}
		if _, err := captureStdout(t, func() error { return runIssueCommentAdd(cmd, []string{issueID}) }); err != nil {
			t.Fatal(err)
		}
		server.Close()
		got, has := body["resolve_thread"]
		if has != set || (set && got != true) {
			t.Fatalf("resolve-thread set=%v, body %+v", set, body)
		}
		if resolved != set {
			t.Fatalf("resolve-thread set=%v but the resolve call happened=%v", set, resolved)
		}
	}
}

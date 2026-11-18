package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// newBlockedOnTestServer serves MUL-1 and MUL-2 and records the PUT body for
// MUL-2. With recorded=false it answers like a server that predates
// --blocked-on: no metadata.blocked_on in the response.
func newBlockedOnTestServer(t *testing.T, recorded bool, body *map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/MUL-1":
			json.NewEncoder(w).Encode(map[string]any{"id": "issue-1", "identifier": "MUL-1", "status": "todo"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/MUL-2":
			json.NewEncoder(w).Encode(map[string]any{"id": "issue-2", "identifier": "MUL-2", "status": "todo"})
		case r.Method == http.MethodPut && r.URL.Path == "/api/issues/issue-2":
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			resp := map[string]any{"id": "issue-2", "identifier": "MUL-2", "status": "blocked", "metadata": map[string]any{}}
			if ref, _ := (*body)["blocked_on"].(string); recorded && ref != "" {
				resp["metadata"] = map[string]any{"blocked_on": ref}
			}
			json.NewEncoder(w).Encode(resp)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TASK_CONFIG_ROOT", t.TempDir())
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
}

func TestIssueStatusCommandHasBlockedOnFlag(t *testing.T) {
	if issueStatusCmd.Flags().Lookup("blocked-on") == nil {
		t.Fatal("issue status has no --blocked-on flag")
	}
}

func TestRunIssueStatusBlockedOnSendsRef(t *testing.T) {
	for in, want := range map[string]string{
		"pr:checks":              "pr:checks",
		"children@2":             "children@2",
		"gh-run:cheese-work/x:1": "gh-run:cheese-work/x:1",
		"decision:abc":           "decision:abc",
		"issue:MUL-1":            "issue:issue-1",
		"issue:MUL-1@in_review":  "issue:issue-1@in_review",
		"  issue:MUL-1@done  ":   "issue:issue-1@done",
	} {
		var body map[string]any
		newBlockedOnTestServer(t, true, &body)
		cmd := newIssueStatusTestCmd()
		if err := cmd.Flags().Set("blocked-on", in); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if err := runIssueStatus(cmd, []string{"MUL-2", "blocked"}); err != nil {
			t.Fatalf("%q: runIssueStatus: %v", in, err)
		}
		if !reflect.DeepEqual(body, map[string]any{"status": "blocked", "blocked_on": want}) {
			t.Fatalf("%q: body = %#v, want blocked_on %q", in, body, want)
		}
	}
}

func TestRunIssueStatusBlockedOnRefusals(t *testing.T) {
	cases := map[string][2]string{
		"unknown form":    {"member:abc", "blocked"},
		"empty":           {" ", "blocked"},
		"bad pr event":    {"pr:open", "blocked"},
		"bad stage":       {"children@0", "blocked"},
		"bad issue state": {"issue:MUL-1@wrong", "blocked"},
		"other status":    {"pr:checks", "in_review"},
	}
	for name, c := range cases {
		var body map[string]any
		newBlockedOnTestServer(t, true, &body)
		cmd := newIssueStatusTestCmd()
		if err := cmd.Flags().Set("blocked-on", c[0]); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		err := runIssueStatus(cmd, []string{"MUL-2", c[1]})
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if c[1] == "blocked" && !strings.Contains(err.Error(), "issue:") {
			t.Fatalf("%s: error %q does not name the forms", name, err)
		}
		if body != nil {
			t.Fatalf("%s: a refused move must not reach the server, got %#v", name, body)
		}
	}
}

func TestRunIssueStatusBlockedOnFailsWhenServerDoesNotRecordIt(t *testing.T) {
	var body map[string]any
	newBlockedOnTestServer(t, false, &body)
	cmd := newIssueStatusTestCmd()
	_ = cmd.Flags().Set("blocked-on", "pr:checks")
	err := runIssueStatus(cmd, []string{"MUL-2", "blocked"})
	if err == nil || !strings.Contains(err.Error(), "blocked-on") {
		t.Fatalf("err = %v, want a --blocked-on not recorded error", err)
	}
}

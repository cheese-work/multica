package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CHE-9075: --discovery-reason reaches the create request as discovery_reason,
// next to the status and the quick-create origin the run already sends.
func TestRunIssueCreateSendsDiscoveryReason(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name string
		args []string
		want any
	}{
		{"reason is sent", []string{"--status", "investigating", "--discovery-reason", "no done-when"}, "no done-when"},
		{"no reason sends no key", []string{"--status", "investigating"}, nil},
		{"blank reason is dropped", []string{"--discovery-reason", "  "}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&body)
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{"id": "created-issue", "title": "t"})
			}))
			defer srv.Close()
			t.Setenv("MULTICA_SERVER_URL", srv.URL)
			t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
			t.Setenv("MULTICA_TOKEN", "mat_test-token")
			t.Setenv("MULTICA_QUICK_CREATE_TASK_ID", "cccc1111-2222-3333-4444-555566667777")
			cmd := newIssueCreateTestCmd()
			cmd.Flags().String("discovery-reason", "", "")
			if err := cmd.ParseFlags(append([]string{"--title", "t"}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			if err := runIssueCreate(cmd, nil); err != nil {
				t.Fatal(err)
			}
			if got := body["discovery_reason"]; got != tc.want {
				t.Errorf("discovery_reason = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIssueCreateRegistersDiscoveryReasonFlag(t *testing.T) {
	if issueCreateCmd.Flags().Lookup("discovery-reason") == nil {
		t.Fatal("issue create must register --discovery-reason")
	}
}

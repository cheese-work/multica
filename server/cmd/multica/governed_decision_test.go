package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// CHE-2458: --decision-id sends the CHE-2448 decision-bound body and
// --print-hash prints the hashes without writing.

const testDecisionID = "01a186e6-deed-73a8-b081-7444d71a7000"

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func addDecisionFlags(cmd *cobra.Command) *cobra.Command {
	if cmd.Flags().Lookup("vote-issue") == nil {
		cmd.Flags().String("vote-issue", "", "")
	}
	cmd.Flags().String("decision-id", "", "")
	cmd.Flags().Bool("print-hash", false, "")
	return cmd
}

func wantDecisionBody(t *testing.T, body map[string]any, field, content string) {
	t.Helper()
	want := map[string]any{
		field:           content,
		"decision_id":   testDecisionID,
		"before_sha256": testDigestHex,
		"after_sha256":  sha256Hex(content),
	}
	if len(body) != len(want) {
		t.Fatalf("body = %v, want exactly %v", body, want)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, body[k], v)
		}
	}
}

func TestDecisionFlagsRejectedWithoutHTTPCall(t *testing.T) {
	cases := []struct {
		name, digest, decision, vote string
		printHash                    bool
		want                         string
	}{
		{"print-hash needs expected-before-digest", "", "", "", true, "--print-hash requires --expected-before-digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			var calls int32
			srv := voteTestServer(t, "agent", &body, &calls)
			defer srv.Close()
			setSquadUpdateServerEnv(t, srv.URL)

			cmd := addDecisionFlags(newAgentCASTestCmd())
			_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "agent text\n"))
			if tc.digest != "" {
				_ = cmd.Flags().Set("expected-before-digest", tc.digest)
			}
			if tc.decision != "" {
				_ = cmd.Flags().Set("decision-id", tc.decision)
			}
			if tc.vote != "" {
				_ = cmd.Flags().Set("vote-issue", tc.vote)
			}
			if tc.printHash {
				_ = cmd.Flags().Set("print-hash", "true")
			}

			err := runAgentUpdate(cmd, []string{"agent-123"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if atomic.LoadInt32(&calls) != 0 {
				t.Errorf("HTTP calls = %d, want 0", calls)
			}
		})
	}
}

func TestAgentUpdatePrintHashWritesNothing(t *testing.T) {
	var body map[string]any
	var calls int32
	srv := voteTestServer(t, "agent", &body, &calls)
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := addDecisionFlags(newAgentCASTestCmd())
	_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "agent text\n"))
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
	_ = cmd.Flags().Set("print-hash", "true")

	out, err := captureStdout(t, func() error { return runAgentUpdate(cmd, []string{"agent-123"}) })
	if err != nil {
		t.Fatalf("runAgentUpdate: %v", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("HTTP calls = %d, want 0", calls)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", out, err)
	}
	if got["before_sha256"] != testDigestHex || got["after_sha256"] != sha256Hex("agent text\n") {
		t.Errorf("hashes = %v", got)
	}
}

// Workflow half. The expected hash is spelled out by hand from the recipe in
// server/internal/handler/issue_workflow_bound_write.go: keys sorted, no
// whitespace, omitempty fields absent, wrapped with the workflow id. The
// "after" side is the steps as the server stores them (trimmed, arranged).
const (
	hashFixtureDoneCancelled = `{"handler":{"type":"none"},"instructions":"","status_key":"done"},{"handler":{"type":"none"},"instructions":"","status_key":"cancelled"}`
	hashFixtureCanonical     = `{"steps":[{"handler":{"id":"a1","type":"agent"},"instructions":"S1","next_status_key":"done","status_key":"todo"},{"handler":{"type":"none"},"instructions":"","status_key":"done"},{"handler":{"type":"none"},"instructions":"","status_key":"cancelled"}],"workflow_id":"` + testWorkflowID + `"}`
	hashFixtureAfter         = `{"steps":[{"handler":{"id":"a1","type":"agent"},"instructions":"S1 new","next_status_key":"done","status_key":"todo"},{"handler":{"type":"none"},"instructions":"","status_key":"done"},{"handler":{"type":"none"},"instructions":"","status_key":"cancelled"}],"workflow_id":"` + testWorkflowID + `"}`
)

func hashFixtureWorkflow() map[string]any {
	return map[string]any{
		"id":      testWorkflowID,
		"version": "v1",
		"steps": []any{
			map[string]any{"status_key": "todo", "handler": map[string]any{"type": "agent", "id": "a1"}, "instructions": "S1", "next_status_key": "done"},
			map[string]any{"status_key": "done", "handler": map[string]any{"type": "none"}, "instructions": ""},
			map[string]any{"status_key": "cancelled", "handler": map[string]any{"type": "none"}, "instructions": ""},
		},
	}
}

func workflowDecisionServer(t *testing.T, patched *map[string]any, patches *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(hashFixtureWorkflow())
			return
		}
		atomic.AddInt32(patches, 1)
		_ = json.NewDecoder(r.Body).Decode(patched)
		_ = json.NewEncoder(w).Encode(hashFixtureWorkflow())
	}))
}

func newWorkflowDecisionCmd() *cobra.Command {
	cmd := newWorkflowTestCmd("status", "file", "steps-file", "decision-id")
	cmd.Flags().Int("max-runs-per-issue", 0, "")
	cmd.Flags().Bool("include-child-prs", true, "")
	cmd.Flags().Bool("print-hash", false, "")
	return cmd
}

func TestWorkflowPrintHashWritesNothing(t *testing.T) {
	var patched map[string]any
	var patches int32
	srv := workflowDecisionServer(t, &patched, &patches)
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowDecisionCmd()
	_ = cmd.Flags().Set("status", "todo")
	_ = cmd.Flags().Set("file", writeTemp(t, "S1 new"))
	_ = cmd.Flags().Set("print-hash", "true")
	out, err := captureStdout(t, func() error { return runWorkflowSetInstructions(cmd, []string{testWorkflowID}) })
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&patches) != 0 {
		t.Errorf("PATCH calls = %d, want 0", patches)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", out, err)
	}
	if got["before_sha256"] != sha256Hex(hashFixtureCanonical) || got["after_sha256"] != sha256Hex(hashFixtureAfter) {
		t.Errorf("hashes = %v", got)
	}
}

func TestWorkflowDecisionFlagErrorsSendNothing(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"start status", map[string]string{"decision-id": testDecisionID, "start-status": "todo"}, "--start-status needs the owner/admin path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var patched map[string]any
			var patches int32
			srv := workflowDecisionServer(t, &patched, &patches)
			defer srv.Close()
			setCLITestServerEnv(t, srv.URL)

			cmd := newWorkflowDecisionCmd()
			cmd.Flags().String("start-status", "", "")
			for k, v := range tc.set {
				if k == "steps-file" {
					v = writeTemp(t, v)
				}
				if err := cmd.Flags().Set(k, v); err != nil {
					t.Fatal(err)
				}
			}
			err := runWorkflowUpdate(cmd, []string{testWorkflowID})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if atomic.LoadInt32(&patches) != 0 {
				t.Errorf("PATCH calls = %d, want 0", patches)
			}
		})
	}
}

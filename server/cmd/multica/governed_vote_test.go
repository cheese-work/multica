package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// CHE-1936: --vote-issue sends the CHE-1862 vote-receipt body
// {<field>, vote_issue, before_sha256, after_sha256} instead of the owner CAS
// body, for agent instructions, squad instructions and workspace context.

const testVoteIssue = "01a15d81-674f-7154-89a5-ce4047260ef3"

func voteTestServer(t *testing.T, entityKey string, body *map[string]any, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if err := json.NewDecoder(r.Body).Decode(body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			entityKey:       map[string]any{"id": "id-1"},
			"before_digest": testDigestHex,
			"after_digest":  "after456",
		})
	}))
}

func wantVoteBody(t *testing.T, body map[string]any, field, content string) {
	t.Helper()
	want := map[string]any{
		field:           content,
		"vote_issue":    testVoteIssue,
		"before_sha256": testDigestHex,
		"after_sha256":  fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
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

func newWorkspaceVoteTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "update"}
	for _, name := range []string{"server-url", "profile", "name", "description", "context", "context-file", "issue-prefix", "compact-prompt-file", "expected-before-digest", "vote-issue"} {
		cmd.Flags().String(name, "", "")
	}
	cmd.Flags().Bool("description-stdin", false, "")
	cmd.Flags().Bool("context-stdin", false, "")
	cmd.Flags().String("output", "json", "")
	return cmd
}

func TestAgentUpdateVoteIssueSendsVoteReceiptBody(t *testing.T) {
	var body map[string]any
	var calls int32
	srv := voteTestServer(t, "agent", &body, &calls)
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newAgentCASTestCmd()
	cmd.Flags().String("vote-issue", "", "")
	_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "agent text\n"))
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
	_ = cmd.Flags().Set("vote-issue", testVoteIssue)

	if _, err := captureStdout(t, func() error { return runAgentUpdate(cmd, []string{"agent-123"}) }); err != nil {
		t.Fatalf("runAgentUpdate: %v", err)
	}
	wantVoteBody(t, body, "instructions", "agent text\n")
}

func TestSquadUpdateVoteIssueSendsVoteReceiptBody(t *testing.T) {
	var body map[string]any
	var calls int32
	srv := voteTestServer(t, "squad", &body, &calls)
	defer srv.Close()
	setSquadUpdateServerEnv(t, srv.URL)

	cmd := newSquadUpdateTestCmd()
	cmd.Flags().String("vote-issue", "", "")
	_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "squad text"))
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
	_ = cmd.Flags().Set("vote-issue", testVoteIssue)

	if _, err := captureStdout(t, func() error { return runSquadUpdate(cmd, []string{"squad-123"}) }); err != nil {
		t.Fatalf("runSquadUpdate: %v", err)
	}
	wantVoteBody(t, body, "instructions", "squad text")
}

func TestWorkspaceUpdateVoteIssueSendsVoteReceiptBody(t *testing.T) {
	var body map[string]any
	var calls int32
	srv := voteTestServer(t, "workspace", &body, &calls)
	defer srv.Close()
	setWorkspaceUpdateServerEnv(t, srv.URL)

	cmd := newWorkspaceVoteTestCmd()
	_ = cmd.Flags().Set("context-file", writeCASFile(t, "context text\n"))
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
	_ = cmd.Flags().Set("vote-issue", testVoteIssue)

	if _, err := captureStdout(t, func() error { return runWorkspaceUpdate(cmd, []string{testWorkspaceUUID}) }); err != nil {
		t.Fatalf("runWorkspaceUpdate: %v", err)
	}
	wantVoteBody(t, body, "context", "context text\n")
}

func TestVoteIssueRejectedWithoutHTTPCall(t *testing.T) {
	cases := []struct {
		name, digest, issue, want string
	}{
		{"needs expected-before-digest", "", testVoteIssue, "--vote-issue requires --expected-before-digest"},
		{"issue must be a UUID", testDigestHex, "CHE-1936", "--vote-issue must be an issue UUID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			var calls int32
			srv := voteTestServer(t, "agent", &body, &calls)
			defer srv.Close()
			setSquadUpdateServerEnv(t, srv.URL)

			cmd := newAgentCASTestCmd()
			cmd.Flags().String("vote-issue", "", "")
			_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "agent text\n"))
			if tc.digest != "" {
				_ = cmd.Flags().Set("expected-before-digest", tc.digest)
			}
			_ = cmd.Flags().Set("vote-issue", tc.issue)

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

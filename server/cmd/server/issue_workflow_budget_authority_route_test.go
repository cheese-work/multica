package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
)

// CHE-2829 (CHE-2798 S1b) through the real Auth -> router -> handler chain: only
// an interactive session changes a workflow's run cap. A client-forged
// X-Actor-Source must lose to the server's own stamp.
func TestIssueWorkflowRunCapNeedsInteractiveSessionOnRoute(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()

	var agentID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load integration-test agent: %v", err)
	}
	matToken := mintAgentTaskToken(t, agentID, ensureAgentTask(t, agentID), testUserID)

	pat, err := auth.GeneratePATToken()
	if err != nil {
		t.Fatalf("generate pat: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO personal_access_token (user_id, name, token_hash, token_prefix)
		VALUES ($1, 'che-2829-budget', $2, $3)
	`, testUserID, auth.HashToken(pat), pat[:12]); err != nil {
		t.Fatalf("insert pat: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM personal_access_token WHERE user_id = $1 AND name = 'che-2829-budget'`, testUserID)
	})

	var workflowID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue_workflow (workspace_id, name, initial_status_key, owner_id)
		VALUES ($1, 'che-2829-budget-route', 'backlog', $2) RETURNING id::text
	`, testWorkspaceID, testUserID).Scan(&workflowID); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue_workflow WHERE id = $1`, workflowID) })

	patch := func(bearer, forgedSource string, body map[string]any) int {
		t.Helper()
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
		req, err := http.NewRequest(http.MethodPatch, testServer.URL+"/api/issue-workflows/"+workflowID, &buf)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		if forgedSource != "" {
			req.Header.Set("X-Actor-Source", forgedSource)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("perform request: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	storedCap := func() (cap *int32) {
		t.Helper()
		if err := testPool.QueryRow(ctx, `SELECT max_runs_per_issue FROM issue_workflow WHERE id = $1`, workflowID).Scan(&cap); err != nil {
			t.Fatalf("read cap: %v", err)
		}
		return cap
	}

	for _, tc := range []struct{ name, bearer, forged string }{
		{"cli_jwt", testToken, ""},
		{"cli_jwt forging interactive_jwt", testToken, "interactive_jwt"},
		{"pat", pat, ""},
		{"pat forging interactive_jwt", pat, "interactive_jwt"},
		{"task token", matToken, ""},
		{"task token forging interactive_jwt", matToken, "interactive_jwt"},
	} {
		t.Run(tc.name+" cannot change the cap", func(t *testing.T) {
			if got := patch(tc.bearer, tc.forged, map[string]any{"max_runs_per_issue": 9}); got != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", got)
			}
			if got := storedCap(); got != nil {
				t.Fatalf("refused write stored cap %d", *got)
			}
		})
	}

	t.Run("interactive_jwt sets the cap", func(t *testing.T) {
		if got := patch(interactiveToken(t), "", map[string]any{"max_runs_per_issue": 7}); got != http.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
		if got := storedCap(); got == nil || *got != 7 {
			t.Fatalf("stored cap = %v, want 7", got)
		}
	})

	for _, tc := range []struct{ name, bearer string }{{"cli_jwt", testToken}, {"pat", pat}, {"task token", matToken}} {
		t.Run(tc.name+" cannot clear or move the cap", func(t *testing.T) {
			for _, n := range []int{0, 8} {
				if got := patch(tc.bearer, "", map[string]any{"max_runs_per_issue": n}); got != http.StatusForbidden {
					t.Fatalf("cap %d: status = %d, want 403", n, got)
				}
			}
			if got := storedCap(); got == nil || *got != 7 {
				t.Fatalf("stored cap = %v, want 7", got)
			}
		})
	}

	t.Run("unchanged cap keeps today's authorization", func(t *testing.T) {
		if got := patch(pat, "", map[string]any{"max_runs_per_issue": 7, "description": "same cap"}); got != http.StatusOK {
			t.Fatalf("pat same cap: status = %d, want 200", got)
		}
		if got := patch(testToken, "", map[string]any{"description": "no budget field"}); got != http.StatusOK {
			t.Fatalf("cli_jwt no cap: status = %d, want 200", got)
		}
		if got := patch(matToken, "", map[string]any{"max_runs_per_issue": 7}); got != http.StatusForbidden {
			t.Fatalf("task token same cap: status = %d, want 403 (unbound machine write, as today)", got)
		}
	})
}

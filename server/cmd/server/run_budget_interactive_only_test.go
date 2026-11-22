package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestRunBudgetDirectRoutesGone sends every credential kind through the real
// router and Auth middleware (CHE-2798 S1c): the direct raise and top-up routes
// answer 410 to all of them, an interactive session included, and write nothing.
func TestRunBudgetDirectRoutesGone(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	fx := workspaceFixture(t)

	// Each request gets its own issue on a two-run workflow: a raise to 5 only
	// succeeds once, and a successful write must be seen to land.
	budgetedIssue := func(t *testing.T) string {
		t.Helper()
		workflowID := fx.Insert(t, "issue_workflow", testutil.Cols{
			"workspace_id": testWorkspaceID, "name": fmt.Sprintf("che-2824 budget %d", time.Now().UnixNano()),
			"initial_status_key": "todo", "steps": `[{"status_key":"todo"}]`, "max_runs_per_issue": 2,
		})
		issueID := fx.Issue(t, "Budget auth matrix", testutil.Cols{"workflow_id": workflowID})
		fx.Cleanup(t, `DELETE FROM activity_log WHERE issue_id = $1`, issueID)
		return issueID
	}

	var agentID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load agent: %v", err)
	}
	taskToken := mintAgentTaskToken(t, agentID, ensureAgentTask(t, agentID), testUserID)

	pat, err := auth.GeneratePATToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO personal_access_token (user_id, name, token_hash, token_prefix) VALUES ($1, 'che-2824-pat', $2, $3)`, testUserID, auth.HashToken(pat), pat[:12]); err != nil {
		t.Fatalf("insert pat: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM personal_access_token WHERE user_id = $1 AND name = 'che-2824-pat'`, testUserID)
	})

	fleet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"valid": true, "owner_id": "` + testUserID + `"}`))
	}))
	t.Cleanup(fleet.Close)
	t.Setenv("MULTICA_CLOUD_URL", fleet.URL)
	cloudRouter := httptest.NewServer(NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil))
	t.Cleanup(cloudRouter.Close)

	// Mints the cookies exactly as login does, so the CSRF value is a real one.
	rec := httptest.NewRecorder()
	interactive := interactiveToken(t)
	if err := auth.SetAuthCookies(rec, interactive); err != nil {
		t.Fatal(err)
	}
	var csrf string
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CSRFCookieName {
			csrf = c.Value
		}
	}

	type credential struct {
		name        string
		server      string
		csrfRefused bool
		apply       func(*http.Request)
	}
	bearer := func(token string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	}
	cookie := func(csrfHeader string) func(*http.Request) {
		return func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: interactive})
			if csrfHeader != "" {
				r.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrfHeader})
				r.Header.Set(auth.CSRFHeaderName, csrfHeader)
			}
		}
	}
	cliJWT, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": testUserID, "exp": 4102444800}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	credentials := []credential{
		{"pat", testServer.URL, false, bearer(pat)},
		{"cloud pat", cloudRouter.URL, false, bearer("mcn_che2824")},
		{"cli_jwt", testServer.URL, false, bearer(cliJWT)},
		{"task token", testServer.URL, false, bearer(taskToken)},
		{"forged header on cli_jwt", testServer.URL, false, func(r *http.Request) {
			bearer(cliJWT)(r)
			r.Header.Set("X-Actor-Source", "interactive_jwt")
		}},
		{"forged header on pat", testServer.URL, false, func(r *http.Request) {
			bearer(pat)(r)
			r.Header.Set("X-Actor-Source", "interactive_jwt")
		}},
		{"interactive cookie, no csrf", testServer.URL, true, cookie("")},
		{"interactive cookie, wrong csrf", testServer.URL, true, cookie("00.00")},
		{"interactive cookie, valid csrf", testServer.URL, false, cookie(csrf)},
		{"desktop interactive bearer", testServer.URL, false, bearer(interactive)},
	}
	for _, route := range []string{"/run-budget", "/run-budget/top-up"} {
		for _, c := range credentials {
			t.Run(route+"/"+c.name, func(t *testing.T) {
				issueID := budgetedIssue(t)
				req, err := http.NewRequest(http.MethodPost, c.server+"/api/issues/"+issueID+route, bytes.NewReader([]byte(`{"runs": 5}`)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Workspace-ID", testWorkspaceID)
				req.Header.Set("Content-Type", "application/json")
				c.apply(req)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				action, wrote := "run_budget_raised", 0
				if route == "/run-budget/top-up" {
					action = "run_budget_top_up"
				}
				if err := testPool.QueryRow(ctx, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = $2`, issueID, action).Scan(&wrote); err != nil {
					t.Fatal(err)
				}
				// S1c: the direct routes are gone for every credential, the one that
				// could raise before included. The action path is the only way.
				want := http.StatusGone
				if c.csrfRefused {
					// The global CSRF check stops a cookie request before any route.
					want = http.StatusForbidden
				}
				if resp.StatusCode != want || wrote != 0 {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("status = %d (%s), %s rows %d; want %d and 0", resp.StatusCode, b, action, wrote, want)
				}
				var capNow int
				if err := testPool.QueryRow(ctx, `SELECT COALESCE(max_runs_override, 0) FROM issue WHERE id = $1`, issueID).Scan(&capNow); err != nil || capNow != 0 {
					t.Fatalf("max_runs_override = %d (err %v), want 0", capNow, err)
				}
			})
		}
	}
}

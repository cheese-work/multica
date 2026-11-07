package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// seedProviderBreaker inserts a breaker row in state and removes it and its audit rows afterwards.
func seedProviderBreaker(t *testing.T, provider, state string) {
	t.Helper()
	ctx := context.Background()
	cleanup := func() {
		testPool.Exec(ctx, `DELETE FROM provider_breaker_audit WHERE workspace_id = $1 AND provider = $2`, testWorkspaceID, provider)
		testPool.Exec(ctx, `DELETE FROM provider_breaker WHERE workspace_id = $1 AND provider = $2`, testWorkspaceID, provider)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO provider_breaker (workspace_id, provider, state, opened_at, next_probe_at)
		VALUES ($1, $2, $3, now(), now() + interval '5 minutes')`, testWorkspaceID, provider, state); err != nil {
		t.Fatalf("seed provider breaker: %v", err)
	}
}

func resetBreakerRequest(userID, provider string) *http.Request {
	req := newRequestAs(userID, "POST", "/api/workspaces/"+testWorkspaceID+"/provider-breakers/"+provider+"/reset", nil)
	return withURLParams(req, "id", testWorkspaceID, "provider", provider)
}

func breakerState(t *testing.T, provider string) string {
	t.Helper()
	var state string
	dbfx.QueryRow(t, `SELECT state FROM provider_breaker WHERE workspace_id = $1 AND provider = $2`, testWorkspaceID, provider).Scan(&state)
	return state
}

func breakerAuditCount(t *testing.T, provider, event string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM provider_breaker_audit WHERE workspace_id = $1 AND provider = $2 AND event = $3`, testWorkspaceID, provider, event)
}

// CHE-4381: reset is owner/admin only and leaves an audit row with who, when and the previous state.
func TestResetProviderBreaker(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const provider = "reset-test"
	seedProviderBreaker(t, provider, "open")

	memberID, _ := addWorkflowTestMember(t, "breaker-member")
	testutil.Call(t, testHandler.ResetProviderBreaker, resetBreakerRequest(memberID, provider)).Want(http.StatusForbidden)
	if got := breakerState(t, provider); got != "open" {
		t.Fatalf("member reset changed state to %q", got)
	}
	if n := breakerAuditCount(t, provider, "reset"); n != 0 {
		t.Fatalf("refused reset wrote %d audit rows", n)
	}

	adminID := dbfx.User(t, "Breaker Admin", "breaker-admin-"+workflowTestSuffix()+"@example.com")
	dbfx.Member(t, testWorkspaceID, adminID, "admin")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE user_id = $1`, adminID)
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, adminID)
	})
	testutil.Call(t, testHandler.ResetProviderBreaker, resetBreakerRequest(adminID, provider)).Want(http.StatusOK)
	if got := breakerState(t, provider); got != "closed" {
		t.Fatalf("admin reset left state %q", got)
	}

	var event, previous, next, actor string
	dbfx.QueryRow(t, `SELECT event, previous_state, new_state, actor_id::text FROM provider_breaker_audit WHERE workspace_id = $1 AND provider = $2`,
		testWorkspaceID, provider).Scan(&event, &previous, &next, &actor)
	if event != "reset" || previous != "open" || next != "closed" || actor != adminID {
		t.Fatalf("audit row = %s %s->%s by %s, want reset open->closed by %s", event, previous, next, actor, adminID)
	}
}

func TestListProviderBreakers(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const provider = "list-test"
	seedProviderBreaker(t, provider, "open")
	req := withURLParams(newRequest("GET", "/api/workspaces/"+testWorkspaceID+"/provider-breakers", nil), "id", testWorkspaceID)
	var out struct {
		Breakers []providerBreakerResponse `json:"breakers"`
	}
	testutil.Call(t, testHandler.ListProviderBreakers, req).Want(http.StatusOK).JSON(&out)
	for _, b := range out.Breakers {
		if b.Provider == provider {
			if b.State != "open" || b.OpenedAt == nil || b.NextProbeAt == nil || b.TripEvidence == nil {
				t.Fatalf("breaker = %+v, want open with opened_at, next_probe_at and evidence list", b)
			}
			return
		}
	}
	t.Fatalf("provider %q missing from %+v", provider, out.Breakers)
}

func TestListProviderBreakersClosedHidesStaleEvidence(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const provider = "stale-test"
	seedProviderBreaker(t, provider, "closed")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE provider_breaker SET trip_evidence = ARRAY[gen_random_uuid(), gen_random_uuid()] WHERE workspace_id = $1 AND provider = $2`,
		testWorkspaceID, provider); err != nil {
		t.Fatalf("seed trip evidence: %v", err)
	}
	req := withURLParams(newRequest("GET", "/api/workspaces/"+testWorkspaceID+"/provider-breakers", nil), "id", testWorkspaceID)
	var out struct {
		Breakers []providerBreakerResponse `json:"breakers"`
	}
	testutil.Call(t, testHandler.ListProviderBreakers, req).Want(http.StatusOK).JSON(&out)
	for _, b := range out.Breakers {
		if b.Provider == provider {
			if b.FailureCount != 0 || len(b.TripEvidence) != 0 {
				t.Fatalf("closed breaker shows failure_count=%d evidence=%v, want none", b.FailureCount, b.TripEvidence)
			}
			return
		}
	}
	t.Fatalf("provider %q missing from %+v", provider, out.Breakers)
}

// Security gate (PR 1162): a machine credential bound to an owner/admin must not reset a breaker.
func TestResetProviderBreakerRejectsMachineCredentials(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const provider = "machine-test"
	for _, source := range []string{"task_token", "cloud_pat"} {
		t.Run(source, func(t *testing.T) {
			seedProviderBreaker(t, provider, "open")
			req := resetBreakerRequest(testUserID, provider)
			req.Header.Set("X-Actor-Source", source)
			testutil.Call(t, testHandler.ResetProviderBreaker, req).Want(http.StatusForbidden)
			if got := breakerState(t, provider); got != "open" {
				t.Fatalf("%s reset changed state to %q", source, got)
			}
			if n := breakerAuditCount(t, provider, "reset"); n != 0 {
				t.Fatalf("refused %s reset wrote %d audit rows", source, n)
			}
		})
	}
}

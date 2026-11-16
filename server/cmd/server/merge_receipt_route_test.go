package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
)

// CHE-5594: PUT /api/github/merge-receipts through the real Auth -> router
// chain. A task token and a plain member are refused before the handler runs;
// a person who is owner or admin reaches it (an empty body is a 400, not a 403).
func TestMergeReceiptRouteAcceptsOnlyAPersonWhoIsOwnerOrAdmin(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	var agentID, memberID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load integration-test agent: %v", err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Receipt Plain Member', $1) RETURNING id::text`, uuid.NewString()+"@multica.ai").Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, memberID) })
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, memberID); err != nil {
		t.Fatal(err)
	}
	memberToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": memberID, "amr": "interactive", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodPut, "/api/github/merge-receipts"},
		{http.MethodPut, "/api/github/merge-vetoes"},
		{http.MethodDelete, "/api/github/merge-vetoes"},
	} {
		route := route
		t.Run(route.method+" "+route.path, func(t *testing.T) { mergeRouteGates(t, route.method, route.path, agentID, memberToken) })
	}
}

func mergeRouteGates(t *testing.T, method, path, agentID, memberToken string) {
	t.Helper()
	put := func(bearer string) int {
		t.Helper()
		req, err := http.NewRequest(method, testServer.URL+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	for name, bearer := range map[string]string{
		"task token":   mintAgentTaskToken(t, agentID, ensureAgentTask(t, agentID), testUserID),
		"plain member": memberToken,
	} {
		if got := put(bearer); got != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", name, got)
		}
	}
	if got := put(interactiveToken(t)); got != http.StatusBadRequest {
		t.Errorf("owner or admin: status = %d, want 400 from the handler (past both gates)", got)
	}
}

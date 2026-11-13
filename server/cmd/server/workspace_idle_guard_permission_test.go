package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// CHE-3296: PATCH /api/workspaces/{id} admits admins, but the
// idle_started_guard field is the owner's. Real router, one user per role.
func TestWorkspaceIdleGuardPatchOwnerOnly(t *testing.T) {
	ctx := context.Background()
	var original []byte
	if err := testPool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = $1 WHERE id = $2`, original, testWorkspaceID)
	})

	tokenFor := func(role string) string {
		t.Helper()
		var userID string
		email := uuid.NewString() + "@multica.ai"
		if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`, "IdleGuard "+role, email).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID) })
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`, testWorkspaceID, userID, role); err != nil {
			t.Fatal(err)
		}
		token, err := generateTestJWT(userID, email, "IdleGuard "+role)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	patch := func(token string) (int, string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"idle_started_guard": map[string]any{"mode": "shadow", "after_hours": 5}})
		req, err := http.NewRequest(http.MethodPatch, testServer.URL+"/api/workspaces/"+testWorkspaceID, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	saved := func() string {
		t.Helper()
		var raw []byte
		if err := testPool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	before := saved()
	for _, role := range []string{"member", "admin"} {
		if status, body := patch(tokenFor(role)); status != http.StatusForbidden {
			t.Fatalf("%s PATCH idle_started_guard = %d %s, want 403", role, status, body)
		}
		if after := saved(); after != before {
			t.Fatalf("%s PATCH changed settings: %s -> %s", role, before, after)
		}
	}
	if status, body := patch(testToken); status != http.StatusOK {
		t.Fatalf("owner PATCH idle_started_guard = %d %s, want 200", status, body)
	}
	if got := saved(); !strings.Contains(got, `"mode": "shadow"`) || !strings.Contains(got, `"after_hours": 5`) {
		t.Fatalf("owner PATCH did not save idle_started_guard: %s", got)
	}
}

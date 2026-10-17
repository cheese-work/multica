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

// Retrospective test (CHE-1806, added after the implementation): the
// auto_compact field rides on PATCH /api/workspaces/{id}, which the router
// gates to owner and admin. This goes through the real router and auth
// middleware with one user per role.
func TestWorkspaceAutoCompactPatchRoleGate(t *testing.T) {
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
		if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`, "AutoCompact "+role, email).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID) })
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`, testWorkspaceID, userID, role); err != nil {
			t.Fatal(err)
		}
		token, err := generateTestJWT(userID, email, "AutoCompact "+role)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	patch := func(token string, body map[string]any) (int, string) {
		t.Helper()
		payload, _ := json.Marshal(body)
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

	// A member is refused before the handler runs, so the settings are unchanged.
	before := saved()
	memberToken := tokenFor("member")
	if status, body := patch(memberToken, map[string]any{"auto_compact": map[string]any{"threshold_pct": 55}}); status != http.StatusForbidden {
		t.Fatalf("member PATCH auto_compact = %d %s, want 403", status, body)
	}
	if after := saved(); after != before {
		t.Fatalf("member PATCH changed settings: %s -> %s", before, after)
	}

	for _, role := range []string{"admin", "owner"} {
		token := tokenFor(role)
		if role == "owner" {
			token = testToken
		}
		status, body := patch(token, map[string]any{"auto_compact": map[string]any{"enabled": false, "threshold_pct": 65}})
		if status != http.StatusOK {
			t.Fatalf("%s PATCH auto_compact = %d %s, want 200", role, status, body)
		}
		if got := saved(); !strings.Contains(got, `"threshold_pct": 65`) || !strings.Contains(got, `"enabled": false`) {
			t.Fatalf("%s PATCH did not save auto_compact: %s", role, got)
		}
		// Validation still runs behind the role gate.
		if status, body := patch(token, map[string]any{"auto_compact": map[string]any{"threshold_pct": 99}}); status != http.StatusBadRequest || !strings.Contains(body, "threshold_pct") {
			t.Fatalf("%s PATCH threshold_pct 99 = %d %s, want 400 naming the field", role, status, body)
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// CHE-8883 (A1b): issue and revoke of a per-gateway credential are owner-only, through the real
// router and auth middleware: no token is 401, a member or admin is 403 and writes nothing, and
// the owner issues (201) and revokes (204).
func TestBrowserGatewayCredentialRoutesAreOwnerOnly(t *testing.T) {
	ctx := context.Background()
	tokenFor := func(role string) string {
		t.Helper()
		var userID string
		email := uuid.NewString() + "@multica.ai"
		if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`, "GatewayCred "+role, email).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID) })
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`, testWorkspaceID, userID, role); err != nil {
			t.Fatal(err)
		}
		token, err := generateTestJWT(userID, email, "GatewayCred "+role)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	call := func(method, path, token string) int {
		t.Helper()
		req, err := http.NewRequest(method, testServer.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM browser_gateway_credential WHERE workspace_id = $1`, testWorkspaceID)
	})
	issue := "/api/workspaces/" + testWorkspaceID + "/browser-gateways/c00/credentials"
	revoke := issue + "/" + uuid.NewString()
	routes := []struct{ method, path string }{{http.MethodPost, issue}, {http.MethodDelete, revoke}}

	for _, r := range routes {
		if got := call(r.method, r.path, ""); got != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", r.method, r.path, got)
		}
		for _, role := range []string{"member", "admin"} {
			if got := call(r.method, r.path, tokenFor(role)); got != http.StatusForbidden {
				t.Errorf("%s %s as %s = %d, want 403", r.method, r.path, role, got)
			}
		}
	}
	var n int
	testPool.QueryRow(ctx, `SELECT count(*) FROM browser_gateway_credential WHERE workspace_id = $1`, testWorkspaceID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d credentials written by refused callers", n)
	}

	// The owner issues, then revokes what was issued.
	req, _ := http.NewRequest(http.MethodPost, testServer.URL+issue, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ID, Credential string
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(body, &out) != nil || out.ID == "" || out.Credential == "" {
		t.Fatalf("owner issue = %d %s, want 201 with an id and a credential", resp.StatusCode, body)
	}
	if got := call(http.MethodDelete, issue+"/"+out.ID, testToken); got != http.StatusNoContent {
		t.Errorf("owner revoke = %d, want 204", got)
	}
}

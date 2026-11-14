package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/internal/cibroker"
)

type oneKey struct{ k *rsa.PublicKey }

func (o oneKey) Key(context.Context, string) (*rsa.PublicKey, error) { return o.k, nil }

// CHE-5594: a workflow identity writes receipts with no person. Only a token
// for the pinned workflow on main with the receipt audience is accepted, and it
// can write only for the repository its policy entry names.
func TestPutMergeReceiptCIAcceptsOnlyThePinnedWorkflow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, snapshot_head_sha, snapshot_base_ref)
		VALUES ($1, 1, 'fixture', 'oidc', 559403, 'oidc PR', 'open', 'https://example.test/pr', now(), now(), 'h1', 'h1', 'main')`, testWorkspaceID); err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM merge_receipt_fact WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1 AND repo_owner = 'fixture' AND repo_name = 'oidc'`, testWorkspaceID)
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := cibroker.LoadPolicy([]byte(`entries:
  - repository: fixture/oidc
    repository_id: "11"
    repository_owner_id: "22"
    workflow: .github/workflows/merge-receipt.yml
    events: [schedule, workflow_dispatch]
    ref: refs/heads/main
    runner_environment: github-hosted
    environments: [""]
    permissions: {contents: read, pull_requests: read}
`))
	if err != nil {
		t.Fatal(err)
	}
	prevB, prevW := testHandler.MergeReceiptBroker, testHandler.MergeReceiptWorkspaceID
	t.Cleanup(func() { testHandler.MergeReceiptBroker, testHandler.MergeReceiptWorkspaceID = prevB, prevW })
	jti := 0
	token := func(aud string, mutate map[string]any) string {
		jti++
		c := jwt.MapClaims{
			"iss": cibroker.Issuer, "aud": aud, "jti": time.Now().String() + string(rune('a'+jti)),
			"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(),
			"repository": "fixture/oidc", "repository_id": "11", "repository_owner_id": "22",
			"ref": cibroker.MainRef, "event_name": "schedule", "runner_environment": "github-hosted",
			"workflow_ref": "fixture/oidc/.github/workflows/merge-receipt.yml@refs/heads/main",
			"run_id":       "1", "run_attempt": "1",
		}
		for k, v := range mutate {
			c[k] = v
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
		tok.Header["kid"] = "k1"
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	body := map[string]any{"pr_number": 559403, "head_sha": "h1", "base_ref": "main", "tier": 1, "security_gate": "success", "paths": []string{"a.go"}, "evidence": "ci-1"}
	rows := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM merge_receipt_fact WHERE workspace_id=$1 AND repo_name='oidc'`, testWorkspaceID)
	}
	call := func(bearer string, b map[string]any) int {
		req := newRequest("PUT", "/api/github/merge-receipts/ci", b)
		req.Header.Del("X-User-ID")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		testHandler.PutMergeReceiptCI(w, req)
		return w.Code
	}
	if got := call(token(MergeReceiptAudience, nil), body); got != http.StatusNotFound || rows() != 0 {
		t.Fatalf("route not configured: status %d rows %d, want 404 and 0", got, rows())
	}
	testHandler.MergeReceiptBroker = &cibroker.Broker{
		Verifier: &cibroker.Verifier{Keys: oneKey{&key.PublicKey}, Now: time.Now, Audience: MergeReceiptAudience},
		Policy:   policy,
	}
	testHandler.MergeReceiptBroker.Verifier = cibroker.NewVerifier(oneKey{&key.PublicKey})
	testHandler.MergeReceiptBroker.Verifier.Audience = MergeReceiptAudience
	testHandler.MergeReceiptWorkspaceID = testWorkspaceID
	cases := []struct {
		name   string
		bearer string
		body   map[string]any
		want   int
	}{
		{"no token", "", body, http.StatusUnauthorized},
		{"CI broker audience", token(cibroker.Audience, nil), body, http.StatusUnauthorized},
		{"other workflow", token(MergeReceiptAudience, map[string]any{"workflow_ref": "fixture/oidc/.github/workflows/evil.yml@refs/heads/main"}), body, http.StatusForbidden},
		{"pull_request event", token(MergeReceiptAudience, map[string]any{"event_name": "pull_request"}), body, http.StatusForbidden},
		{"other branch", token(MergeReceiptAudience, map[string]any{"ref": "refs/heads/feature"}), body, http.StatusForbidden},
		{"other repository in the body", token(MergeReceiptAudience, nil), map[string]any{"repo_owner": "someone", "repo_name": "else", "pr_number": 559403, "head_sha": "h1", "base_ref": "main", "evidence": "x"}, http.StatusForbidden},
	}
	for _, c := range cases {
		if got := call(c.bearer, c.body); got != c.want || rows() != 0 {
			t.Fatalf("%s: status %d rows %d, want %d and 0", c.name, got, rows(), c.want)
		}
	}
	good := token(MergeReceiptAudience, nil)
	if got := call(good, body); got != http.StatusNoContent || rows() != 1 {
		t.Fatalf("pinned workflow: status %d rows %d, want 204 and 1", got, rows())
	}
	if got := call(good, body); got != http.StatusUnauthorized {
		t.Fatalf("replayed token: status %d, want 401", got)
	}
}

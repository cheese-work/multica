package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-8883 (A1b): issue and revoke of a per-gateway credential. The owner-only and human-only
// gates are router middleware; TestBrowserGatewayCredentialRoutesAreOwnerOnly in cmd/server
// covers them. These tests cover what the handlers do once admitted.

type credentialOut struct {
	ID         string `json:"id"`
	Gateway    string `json:"gateway"`
	Credential string `json:"credential"`
	CreatedAt  string `json:"created_at"`
}

func credentialFixture(t *testing.T) *Handler {
	t.Helper()
	h := testHandler
	h.BrowserLeases = &browserlease.Service{Pool: testPool, Queries: db.New(testPool)}
	t.Cleanup(func() {
		h.BrowserLeases = nil
		testPool.Exec(context.Background(), "DELETE FROM browser_gateway_credential WHERE workspace_id = $1", testWorkspaceID)
	})
	return h
}

func credReq(method, ws, gateway, credentialID string) *http.Request {
	req := newRequest(method, "/api/browser-gateway-credentials", nil)
	kv := []string{"id", ws, "gateway", gateway}
	if credentialID != "" {
		kv = append(kv, "credentialId", credentialID)
	}
	return withURLParams(req, kv...)
}

func issueCredential(t *testing.T, h *Handler, ws, gateway string) credentialOut {
	t.Helper()
	var out credentialOut
	testutil.Call(t, h.IssueBrowserGatewayCredential, credReq(http.MethodPost, ws, gateway, "")).Want(http.StatusCreated).JSON(&out)
	return out
}

func TestIssueBrowserGatewayCredentialReturnsThePlaintextOnceAndStoresOnlyItsHash(t *testing.T) {
	h := credentialFixture(t)
	rec := testutil.Call(t, h.IssueBrowserGatewayCredential, credReq(http.MethodPost, testWorkspaceID, "c00", "")).Want(http.StatusCreated)
	var out credentialOut
	rec.JSON(&out)
	if !strings.HasPrefix(out.Credential, browserlease.CredentialPrefix) || len(out.Credential) <= len(browserlease.CredentialPrefix)+40 {
		t.Fatalf("credential = %q, want %s + 43 base64url chars", out.Credential, browserlease.CredentialPrefix)
	}
	if out.Gateway != "c00" || out.ID == "" || out.CreatedAt == "" {
		t.Fatalf("body = %+v", out)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var hash string
	if err := testPool.QueryRow(context.Background(), `SELECT credential_hash FROM browser_gateway_credential WHERE id = $1 AND workspace_id = $2`, out.ID, testWorkspaceID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != browserlease.HashCredential(out.Credential) || strings.Contains(hash, out.Credential) {
		t.Fatalf("stored %q, want only the SHA-256 of the credential", hash)
	}
	var plaintextRows int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM browser_gateway_credential WHERE credential_hash = $1`, out.Credential).Scan(&plaintextRows)
	if plaintextRows != 0 {
		t.Fatal("the plaintext credential is stored")
	}
}

func TestIssuedBrowserGatewayCredentialAuthenticatesAsItsOwnGatewayAndWorkspace(t *testing.T) {
	h := credentialFixture(t)
	out := issueCredential(t, h, testWorkspaceID, "x99")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+out.Credential)
	p, ok := browserlease.CredentialAuth{Queries: db.New(testPool)}.Authenticate(req)
	if !ok || p.Gateway != "x99" || uuidToString(p.WorkspaceID) != testWorkspaceID || uuidToString(p.CredentialID) != out.ID {
		t.Fatalf("principal = %+v ok=%v, want x99 / %s / %s", p, ok, testWorkspaceID, out.ID)
	}
}

func TestIssueBrowserGatewayCredentialUnknownGatewayIs404AndWritesNothing(t *testing.T) {
	h := credentialFixture(t)
	for _, gw := range []string{"c01", "C00", "x99 ", "../c00"} {
		testutil.Call(t, h.IssueBrowserGatewayCredential, credReq(http.MethodPost, testWorkspaceID, gw, "")).Want(http.StatusNotFound)
	}
	var n int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM browser_gateway_credential WHERE workspace_id = $1`, testWorkspaceID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d credentials written for unknown gateways", n)
	}
}

func TestIssueBrowserGatewayCredentialIsUnavailableWithoutTheLeaseService(t *testing.T) {
	h := testHandler
	h.BrowserLeases = nil
	testutil.Call(t, h.IssueBrowserGatewayCredential, credReq(http.MethodPost, testWorkspaceID, "c00", "")).Want(http.StatusServiceUnavailable)
	testutil.Call(t, h.RevokeBrowserGatewayCredential, credReq(http.MethodDelete, testWorkspaceID, "c00", uuid.NewString())).Want(http.StatusServiceUnavailable)
}

func TestRevokeBrowserGatewayCredentialStopsItAuthenticatingAndIsIdempotent(t *testing.T) {
	h := credentialFixture(t)
	out := issueCredential(t, h, testWorkspaceID, "c00")
	authenticates := func() bool {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+out.Credential)
		_, ok := browserlease.CredentialAuth{Queries: db.New(testPool)}.Authenticate(req)
		return ok
	}
	if !authenticates() {
		t.Fatal("a freshly issued credential does not authenticate")
	}
	testutil.Call(t, h.RevokeBrowserGatewayCredential, credReq(http.MethodDelete, testWorkspaceID, "c00", out.ID)).Want(http.StatusNoContent)
	if authenticates() {
		t.Fatal("a revoked credential still authenticates")
	}
	var first string
	testPool.QueryRow(context.Background(), `SELECT revoked_at::text FROM browser_gateway_credential WHERE id = $1`, out.ID).Scan(&first)
	testutil.Call(t, h.RevokeBrowserGatewayCredential, credReq(http.MethodDelete, testWorkspaceID, "c00", out.ID)).Want(http.StatusNoContent)
	var second string
	testPool.QueryRow(context.Background(), `SELECT revoked_at::text FROM browser_gateway_credential WHERE id = $1`, out.ID).Scan(&second)
	if first == "" || first != second {
		t.Fatalf("revoked_at changed on the second revoke: %q -> %q", first, second)
	}
}

// A credential of another workspace or of another gateway is the same 404 as a missing one, and
// is left untouched.
func TestRevokeBrowserGatewayCredentialOfAnotherWorkspaceOrGatewayIs404(t *testing.T) {
	h := credentialFixture(t)
	out := issueCredential(t, h, testWorkspaceID, "c00")
	cases := []struct{ name, ws, gateway, id string }{
		{"other workspace", uuid.NewString(), "c00", out.ID},
		{"other gateway", testWorkspaceID, "x99", out.ID},
		{"unknown gateway", testWorkspaceID, "nope", out.ID},
		{"never issued", testWorkspaceID, "c00", uuid.NewString()},
	}
	for _, c := range cases {
		testutil.Call(t, h.RevokeBrowserGatewayCredential, credReq(http.MethodDelete, c.ws, c.gateway, c.id)).Want(http.StatusNotFound)
	}
	testutil.Call(t, h.RevokeBrowserGatewayCredential, credReq(http.MethodDelete, testWorkspaceID, "c00", "not-a-uuid")).Want(http.StatusBadRequest)
	var revoked *string
	testPool.QueryRow(context.Background(), `SELECT revoked_at::text FROM browser_gateway_credential WHERE id = $1`, out.ID).Scan(&revoked)
	if revoked != nil {
		t.Fatalf("credential revoked by a foreign request: %v", *revoked)
	}
}

// Issue and revoke each write one audit row in the mutation's transaction. The row names the
// gateway and the credential; neither the plaintext nor its hash is in it, and an idempotent
// second revoke writes nothing new.
func TestBrowserGatewayCredentialIssueAndRevokeWriteAuditRowsWithoutTheSecret(t *testing.T) {
	h := credentialFixture(t)
	out := issueCredential(t, h, testWorkspaceID, "x99")

	rows := auditRowsFor(t, browserlease.AuditCredentialIssue, out.ID)
	if len(rows) != 1 || rows[0].TargetType != "browser_gateway_credential" || rows[0].After["gateway"] != "x99" {
		t.Fatalf("issue audit rows = %+v, want one for gateway x99", rows)
	}
	testutil.Call(t, h.RevokeBrowserGatewayCredential, credReq(http.MethodDelete, testWorkspaceID, "x99", out.ID)).Want(http.StatusNoContent)
	testutil.Call(t, h.RevokeBrowserGatewayCredential, credReq(http.MethodDelete, testWorkspaceID, "x99", out.ID)).Want(http.StatusNoContent)
	revoke := auditRowsFor(t, browserlease.AuditCredentialRevoke, out.ID)
	if len(revoke) != 1 || revoke[0].Before["revoked"] != false || revoke[0].After["revoked"] != true {
		t.Fatalf("revoke audit rows = %+v, want exactly one, revoked false -> true", revoke)
	}

	var all string
	if err := testPool.QueryRow(context.Background(), `SELECT coalesce(string_agg(row_to_json(e)::text, ' '), '') FROM workspace_audit_event e WHERE target_id = $1`, out.ID).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(all, out.Credential) || strings.Contains(all, browserlease.HashCredential(out.Credential)) {
		t.Fatal("an audit row holds the credential plaintext or its hash")
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace_audit_event WHERE target_id = $1`, out.ID)
	})
}

// If the audit row cannot be written, the revoke is rolled back and the credential still works.
func TestRevokeBrowserGatewayCredentialRollsBackWhenTheAuditWriteFails(t *testing.T) {
	h := credentialFixture(t)
	out := issueCredential(t, h, testWorkspaceID, "c00")
	failAuditInsertFor(t, out.ID)
	testutil.Call(t, h.RevokeBrowserGatewayCredential, credReq(http.MethodDelete, testWorkspaceID, "c00", out.ID)).Want(http.StatusInternalServerError)
	var revoked *string
	testPool.QueryRow(context.Background(), `SELECT revoked_at::text FROM browser_gateway_credential WHERE id = $1`, out.ID).Scan(&revoked)
	if revoked != nil {
		t.Fatalf("credential revoked although the audit write failed: %s", *revoked)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace_audit_event WHERE target_id = $1`, out.ID)
	})
}

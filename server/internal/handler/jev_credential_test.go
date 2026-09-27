package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/governance/credential"
)

const jevCredentialTestSecret = "fake-jev-credential-for-test-only"

type jevCredentialLockSignalingTxStarter struct {
	inner   txStarter
	reached chan<- struct{}
}

func (s jevCredentialLockSignalingTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &jevCredentialLockSignalingTx{Tx: tx, reached: s.reached}, nil
}

type jevCredentialLockSignalingTx struct {
	pgx.Tx
	reached chan<- struct{}
}

func (tx *jevCredentialLockSignalingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FOR KEY SHARE") {
		select {
		case tx.reached <- struct{}{}:
		default:
		}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *jevCredentialLockSignalingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestJevCredentialWriteOnlyLifecycle(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	requireJevCredentialTable(t)
	resetJevCredential(t)
	t.Cleanup(func() { resetJevCredential(t) })

	ring, err := credential.NewKeyring(credential.Key{ID: "current", Material: bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	h := *testHandler
	h.JevCredentials = ring

	if response := invokeJevCredential(t, &h, testUserID, http.MethodGet, nil, ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"present":false`) {
		t.Fatalf("initial GET = %d: %s", response.Code, response.Body.String())
	}
	put := invokeJevCredential(t, &h, testUserID, http.MethodPut, []byte(`{"api_key":"`+jevCredentialTestSecret+`"}`), "")
	if put.Code != http.StatusOK || strings.Contains(put.Body.String(), jevCredentialTestSecret) {
		t.Fatalf("PUT exposed credential or failed = %d: %s", put.Code, put.Body.String())
	}

	var stored []byte
	if err := testPool.QueryRow(context.Background(), `SELECT envelope FROM governance_jev_credential WHERE workspace_id = $1`, testWorkspaceID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), jevCredentialTestSecret) {
		t.Fatalf("stored envelope contains plaintext: %s", stored)
	}
	var envelope credential.Envelope
	if err := json.Unmarshal(stored, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Open(envelope, credential.Binding{WorkspaceID: testWorkspaceID, CredentialID: "api-key", Purpose: jevCredentialPurpose}); err != nil {
		t.Fatalf("stored envelope failed authenticated open: %v", err)
	}

	get := invokeJevCredential(t, &h, testUserID, http.MethodGet, nil, "")
	if get.Code != http.StatusOK || get.Body.String() != "{\"present\":true}\n" || strings.Contains(get.Body.String(), jevCredentialTestSecret) {
		t.Fatalf("write-only GET = %d: %s", get.Code, get.Body.String())
	}
	if deleted := invokeJevCredential(t, &h, testUserID, http.MethodDelete, nil, ""); deleted.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", deleted.Code, deleted.Body.String())
	}
	if response := invokeJevCredential(t, &h, testUserID, http.MethodGet, nil, ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"present":false`) {
		t.Fatalf("GET after delete = %d: %s", response.Code, response.Body.String())
	}
}

func TestJevCredentialRejectsNonAdminsMachinesAndMissingKey(t *testing.T) {
	if testHandler == nil || dbfx == nil {
		t.Skip("handler test database is unavailable")
	}
	h := *testHandler // nil keyring is the deployment's default-off state.
	memberID := dbfx.User(t, "Jev credential member", uuid.NewString()+"@multica.test")
	dbfx.Member(t, testWorkspaceID, memberID, "member")
	body := []byte(`{"api_key":"` + jevCredentialTestSecret + `"}`)
	for name, tc := range map[string]struct {
		userID, actorSource string
		wantCode            int
	}{
		"member":      {memberID, "", http.StatusForbidden},
		"machine":     {testUserID, "task_token", http.StatusForbidden},
		"default-off": {testUserID, "", http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			response := invokeJevCredential(t, &h, tc.userID, http.MethodPut, body, tc.actorSource)
			if response.Code != tc.wantCode || strings.Contains(response.Body.String(), jevCredentialTestSecret) {
				t.Fatalf("PUT = %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestJevCredentialDefaultOffReturnsNonretryableSafeProblem(t *testing.T) {
	if testHandler == nil || testPool == nil || dbfx == nil {
		t.Skip("handler test database is unavailable")
	}
	requireJevCredentialTable(t)
	workspaceID := dbfx.Workspace(t, "Jev credential default-off", "jev-credential-default-off-"+uuid.NewString())
	dbfx.Member(t, workspaceID, testUserID, "owner")
	h := *testHandler
	response := invokeJevCredentialForWorkspace(t, &h, workspaceID, testUserID, http.MethodPut,
		[]byte(`{"api_key":"`+jevCredentialTestSecret+`"}`), "")
	if response.Code != http.StatusForbidden {
		t.Fatalf("default-off PUT = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/problem+json" {
		t.Errorf("default-off Content-Type = %q, want application/problem+json", response.Header().Get("Content-Type"))
	}
	var problem map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode default-off problem: %v: %s", err, response.Body.String())
	}
	for _, field := range []string{"problem", "cause", "permitted_fix", "correlation_id", "documentation_link"} {
		value, ok := problem[field].(string)
		if !ok || strings.TrimSpace(value) == "" {
			t.Errorf("default-off problem field %q missing: %s", field, response.Body.String())
		}
	}
	if problem["problem"] != "jev_credential_configuration_required" {
		t.Errorf("default-off problem = %v, want jev_credential_configuration_required", problem["problem"])
	}
	if retryable, ok := problem["retryable"].(bool); !ok || retryable {
		t.Errorf("default-off retryable = %v, want explicit false", problem["retryable"])
	}
	if correlationID, _ := problem["correlation_id"].(string); correlationID == "" || correlationID != response.Header().Get("X-Request-ID") {
		t.Errorf("default-off correlation id %q does not match response header %q", correlationID, response.Header().Get("X-Request-ID"))
	}
	if strings.Contains(response.Body.String(), jevCredentialTestSecret) || strings.Contains(response.Body.String(), "api_key") {
		t.Errorf("default-off problem echoed credential input: %s", response.Body.String())
	}
	var credentialPresent bool
	if err := testPool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM governance_jev_credential WHERE workspace_id = $1)`, workspaceID).Scan(&credentialPresent); err != nil {
		t.Fatal(err)
	}
	if credentialPresent {
		t.Fatal("default-off PUT persisted a credential")
	}
}

func TestResolveJevCredentialRotatesOnlyAfterAuthenticatedOpen(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	requireJevCredentialTable(t)
	resetJevCredential(t)
	t.Cleanup(func() { resetJevCredential(t) })
	old, err := credential.NewKeyring(credential.Key{ID: "old", Material: bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := old.Seal([]byte(jevCredentialTestSecret), credential.Binding{WorkspaceID: testWorkspaceID, CredentialID: "api-key", Purpose: jevCredentialPurpose})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO governance_jev_credential (workspace_id, envelope, updated_by) VALUES ($1, $2, $3)`, testWorkspaceID, raw, testUserID); err != nil {
		t.Fatal(err)
	}
	ring, err := credential.NewKeyring(
		credential.Key{ID: "current", Material: bytes.Repeat([]byte{1}, 32)},
		credential.Key{ID: "old", Material: bytes.Repeat([]byte{2}, 32)},
	)
	if err != nil {
		t.Fatal(err)
	}
	h := *testHandler
	h.JevCredentials = ring
	plaintext, err := h.ResolveJevCredential(t.Context(), parseUUID(testWorkspaceID))
	if err != nil || string(plaintext) != jevCredentialTestSecret {
		t.Fatalf("ResolveJevCredential = (%q, %v)", plaintext, err)
	}
	var rotated []byte
	if err := testPool.QueryRow(t.Context(), `SELECT envelope FROM governance_jev_credential WHERE workspace_id = $1`, testWorkspaceID).Scan(&rotated); err != nil {
		t.Fatal(err)
	}
	var rotatedEnvelope credential.Envelope
	if err := json.Unmarshal(rotated, &rotatedEnvelope); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rotated), jevCredentialTestSecret) || rotatedEnvelope.KeyID != "current" {
		t.Fatalf("rotation did not rewrite sealed envelope: %s", rotated)
	}

	wrongBinding, err := old.Seal([]byte(jevCredentialTestSecret), credential.Binding{WorkspaceID: "other-workspace", CredentialID: "api-key", Purpose: jevCredentialPurpose})
	if err != nil {
		t.Fatal(err)
	}
	wrongBindingJSON, err := json.Marshal(wrongBinding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE governance_jev_credential SET envelope = $2 WHERE workspace_id = $1`, testWorkspaceID, wrongBindingJSON); err != nil {
		t.Fatal(err)
	}
	var beforeRollback []byte
	if err := testPool.QueryRow(t.Context(), `SELECT envelope FROM governance_jev_credential WHERE workspace_id = $1`, testWorkspaceID).Scan(&beforeRollback); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ResolveJevCredential(t.Context(), parseUUID(testWorkspaceID)); !errors.Is(err, ErrJevCredentialUnavailable) {
		t.Fatalf("bad AAD resolve error = %v, want unavailable", err)
	}
	var afterRollback []byte
	if err := testPool.QueryRow(t.Context(), `SELECT envelope FROM governance_jev_credential WHERE workspace_id = $1`, testWorkspaceID).Scan(&afterRollback); err != nil {
		t.Fatal(err)
	}
	var beforeEnvelope, afterEnvelope credential.Envelope
	if err := json.Unmarshal(beforeRollback, &beforeEnvelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(afterRollback, &afterEnvelope); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterEnvelope, beforeEnvelope) {
		t.Fatalf("failed rotation mutated stored envelope: before=%s after=%s", beforeRollback, afterRollback)
	}
}

func TestWorkspaceDeletionRemovesJevCredential(t *testing.T) {
	if testHandler == nil || testPool == nil || dbfx == nil {
		t.Skip("handler test database is unavailable")
	}
	requireJevCredentialTable(t)
	workspaceID := dbfx.Workspace(t, "Jev credential deletion", "jev-credential-deletion-"+uuid.NewString())
	dbfx.Member(t, workspaceID, testUserID, "owner")
	h := jevCredentialHandler(t)

	put := invokeJevCredentialForWorkspace(t, &h, workspaceID, testUserID, http.MethodPut,
		[]byte(`{"api_key":"`+jevCredentialTestSecret+`"}`), "")
	if put.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", put.Code, put.Body.String())
	}

	deleted := httptest.NewRecorder()
	deleteRequest := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID, nil), "id", workspaceID)
	h.DeleteWorkspace(deleted, deleteRequest)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("workspace DELETE = %d: %s", deleted.Code, deleted.Body.String())
	}
	var workspacePresent, credentialPresent bool
	if err := testPool.QueryRow(t.Context(), `
SELECT EXISTS(SELECT 1 FROM workspace WHERE id = $1),
       EXISTS(SELECT 1 FROM governance_jev_credential WHERE workspace_id = $1)
`, workspaceID).Scan(&workspacePresent, &credentialPresent); err != nil {
		t.Fatal(err)
	}
	if workspacePresent || credentialPresent {
		t.Fatalf("workspace deletion left data behind: workspace_present=%t credential_present=%t", workspacePresent, credentialPresent)
	}
}

func TestJevCredentialWriteFencePreventsPostTeardownInsert(t *testing.T) {
	if testHandler == nil || testPool == nil || dbfx == nil {
		t.Skip("handler test database is unavailable")
	}
	requireJevCredentialTable(t)
	workspaceID := dbfx.Workspace(t, "Jev credential write fence", "jev-credential-fence-"+uuid.NewString())
	dbfx.Member(t, workspaceID, testUserID, "owner")

	holder, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = holder.Rollback(context.Background())
		}
	}()
	if _, err := holder.Exec(t.Context(), `SELECT id FROM workspace WHERE id = $1 FOR UPDATE`, workspaceID); err != nil {
		t.Fatal(err)
	}

	reached := make(chan struct{}, 1)
	h := jevCredentialHandler(t)
	h.TxStarter = jevCredentialLockSignalingTxStarter{inner: h.TxStarter, reached: reached}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/workspaces/"+workspaceID+"/jev/credential", bytes.NewReader([]byte(`{"api_key":"`+jevCredentialTestSecret+`"}`)))
	request.Header.Set("X-User-ID", testUserID)
	request.Header.Set("X-Workspace-ID", workspaceID)
	request = withURLParam(request, "id", workspaceID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.PutJevCredential(response, request)
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("credential writer did not reach the workspace write fence")
	}
	if !waitForBlockedBackend(t, done) {
		t.Fatal("credential PUT returned while workspace teardown lock was held")
	}
	if _, err := holder.Exec(t.Context(), `DELETE FROM workspace WHERE id = $1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	committed = true
	<-done
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("queued PUT = %d: %s", response.Code, response.Body.String())
	}
	var credentialPresent bool
	if err := testPool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM governance_jev_credential WHERE workspace_id = $1)`, workspaceID).Scan(&credentialPresent); err != nil {
		t.Fatal(err)
	}
	if credentialPresent {
		t.Fatal("credential writer inserted after workspace teardown committed")
	}
}

func jevCredentialHandler(t *testing.T) Handler {
	t.Helper()
	ring, err := credential.NewKeyring(credential.Key{ID: "current", Material: bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	h := *testHandler
	h.JevCredentials = ring
	return h
}

func requireJevCredentialTable(t *testing.T) {
	t.Helper()
	var ready bool
	if err := testPool.QueryRow(t.Context(), `SELECT to_regclass('governance_jev_credential') IS NOT NULL`).Scan(&ready); err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Skip("jev credential migration is not applied to the test database")
	}
}

func resetJevCredential(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM governance_jev_credential WHERE workspace_id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
}

func invokeJevCredential(t *testing.T, h *Handler, userID, method string, body []byte, actorSource string) *httptest.ResponseRecorder {
	t.Helper()
	return invokeJevCredentialForWorkspace(t, h, testWorkspaceID, userID, method, body, actorSource)
}

func invokeJevCredentialForWorkspace(t *testing.T, h *Handler, workspaceID, userID, method string, body []byte, actorSource string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/workspaces/"+workspaceID+"/jev/credential", bytes.NewReader(body))
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Workspace-ID", workspaceID)
	if actorSource != "" {
		req.Header.Set("X-Actor-Source", actorSource)
	}
	req = withURLParam(req, "id", workspaceID)
	response := httptest.NewRecorder()
	switch method {
	case http.MethodGet:
		h.GetJevCredential(response, req)
	case http.MethodPut:
		h.PutJevCredential(response, req)
	case http.MethodDelete:
		h.DeleteJevCredential(response, req)
	default:
		t.Fatalf("unsupported method %s", method)
	}
	return response
}

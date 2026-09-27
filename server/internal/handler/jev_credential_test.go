package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/governance/credential"
)

const jevCredentialTestSecret = "fake-jev-credential-for-test-only"

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
	if strings.Contains(string(rotated), jevCredentialTestSecret) || !strings.Contains(string(rotated), `"key_id":"current"`) {
		t.Fatalf("rotation did not rewrite sealed envelope: %s", rotated)
	}

	wrongBinding, err := old.Seal([]byte(jevCredentialTestSecret), credential.Binding{WorkspaceID: "other-workspace", CredentialID: "api-key", Purpose: jevCredentialPurpose})
	if err != nil {
		t.Fatal(err)
	}
	beforeRollback, err := json.Marshal(wrongBinding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE governance_jev_credential SET envelope = $2 WHERE workspace_id = $1`, testWorkspaceID, beforeRollback); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ResolveJevCredential(t.Context(), parseUUID(testWorkspaceID)); !errors.Is(err, ErrJevCredentialUnavailable) {
		t.Fatalf("bad AAD resolve error = %v, want unavailable", err)
	}
	var afterRollback []byte
	if err := testPool.QueryRow(t.Context(), `SELECT envelope FROM governance_jev_credential WHERE workspace_id = $1`, testWorkspaceID).Scan(&afterRollback); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRollback, beforeRollback) {
		t.Fatalf("failed rotation mutated stored envelope: before=%s after=%s", beforeRollback, afterRollback)
	}
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
	req := httptest.NewRequest(method, "/api/workspaces/"+testWorkspaceID+"/jev/credential", bytes.NewReader(body))
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	if actorSource != "" {
		req.Header.Set("X-Actor-Source", actorSource)
	}
	req = withURLParam(req, "id", testWorkspaceID)
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

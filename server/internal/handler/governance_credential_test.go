package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/credential"
)

func testCredentialKeyring(t *testing.T) *credential.Keyring {
	t.Helper()
	kr, err := credential.NewKeyring([]credential.VersionedKey{
		{ID: "test-k1", Key: bytes.Repeat([]byte{0x11}, credential.KeySize)},
	})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}

func resetGovernanceCredentialFixture(t *testing.T) {
	t.Helper()
	if testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	requireGovernanceCredentialTables(t)
	if _, err := testPool.Exec(context.Background(), `DELETE FROM governance_workspace_credential_audit WHERE workspace_id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `DELETE FROM governance_workspace_credential WHERE workspace_id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
}

func requireGovernanceCredentialTables(t *testing.T) {
	t.Helper()
	if testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	var ready bool
	if err := testPool.QueryRow(context.Background(), `
		SELECT to_regclass('governance_workspace_credential') IS NOT NULL
		   AND to_regclass('governance_workspace_credential_audit') IS NOT NULL
	`).Scan(&ready); err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Skip("governance workspace credential migrations are not applied to the test database")
	}
}

func invokeGovernanceCredential(t *testing.T, userID, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/workspaces/"+testWorkspaceID+"/governance/credential", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req = withURLParam(req, "id", testWorkspaceID)
	recorder := httptest.NewRecorder()
	switch method {
	case http.MethodGet:
		testHandler.GetGovernanceCredentialStatus(recorder, req)
	case http.MethodPut:
		testHandler.PutGovernanceCredential(recorder, req)
	case http.MethodDelete:
		testHandler.DeleteGovernanceCredential(recorder, req)
	default:
		t.Fatalf("unsupported test method %s", method)
	}
	return recorder
}

func decodeCredentialPresent(t *testing.T, response *httptest.ResponseRecorder) bool {
	t.Helper()
	var body map[string]bool
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode credential response: %v: %s", err, response.Body.String())
	}
	return body["credential_present"]
}

func TestGovernanceCredential_WriteReplaceDeleteLifecycle(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceCredentialFixture(t)
	t.Cleanup(func() { resetGovernanceCredentialFixture(t) })
	testHandler.CredentialKeyring = testCredentialKeyring(t)
	t.Cleanup(func() { testHandler.CredentialKeyring = nil })

	status := invokeGovernanceCredential(t, testUserID, http.MethodGet, "")
	if status.Code != http.StatusOK || decodeCredentialPresent(t, status) {
		t.Fatalf("expected credential_present=false before any write, got %d %s", status.Code, status.Body.String())
	}

	write := invokeGovernanceCredential(t, testUserID, http.MethodPut, `{"value":"sk-fake-provider-key-1"}`)
	if write.Code != http.StatusOK || !decodeCredentialPresent(t, write) {
		t.Fatalf("write failed: %d %s", write.Code, write.Body.String())
	}

	// Acceptance criterion 1: no readback value or fragments anywhere in
	// the response. Assert the plaintext never appears in ANY response
	// body, not just this one — a narrower check risks passing on an
	// oversight in a different response field.
	if bytes.Contains(write.Body.Bytes(), []byte("sk-fake-provider-key-1")) {
		t.Fatalf("write response leaked the plaintext credential: %s", write.Body.String())
	}

	statusAfterWrite := invokeGovernanceCredential(t, testUserID, http.MethodGet, "")
	if !decodeCredentialPresent(t, statusAfterWrite) {
		t.Fatalf("expected credential_present=true after write")
	}

	// Replace: a second write must succeed (not conflict) and rotate the
	// stored envelope rather than erroring as a duplicate.
	replace := invokeGovernanceCredential(t, testUserID, http.MethodPut, `{"value":"sk-fake-provider-key-2"}`)
	if replace.Code != http.StatusOK || !decodeCredentialPresent(t, replace) {
		t.Fatalf("replace failed: %d %s", replace.Code, replace.Body.String())
	}
	var rowCount int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM governance_workspace_credential WHERE workspace_id = $1 AND purpose = $2
	`, testWorkspaceID, JevCredentialPurpose).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Fatalf("expected exactly one credential row after replace, got %d", rowCount)
	}

	// Deletion is a hard delete: verify via loadGovernanceCredentialPlaintext
	// (the only internal reader) that decrypting the replaced value returns
	// the NEW plaintext, not the original — proves replace actually
	// re-sealed rather than layering.
	workspaceUUID, err := util.ParseUUID(testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := loadGovernanceCredentialPlaintext(context.Background(), testPool, testHandler.CredentialKeyring, workspaceUUID, JevCredentialPurpose)
	if err != nil {
		t.Fatalf("loadGovernanceCredentialPlaintext: %v", err)
	}
	if string(plaintext) != "sk-fake-provider-key-2" {
		t.Fatalf("plaintext = %q, want the replaced value", plaintext)
	}

	del := invokeGovernanceCredential(t, testUserID, http.MethodDelete, "")
	if del.Code != http.StatusOK || decodeCredentialPresent(t, del) {
		t.Fatalf("delete failed: %d %s", del.Code, del.Body.String())
	}

	if _, err := loadGovernanceCredentialPlaintext(context.Background(), testPool, testHandler.CredentialKeyring, workspaceUUID, JevCredentialPurpose); err == nil {
		t.Fatalf("expected no credential row to remain after delete")
	}

	// Deleting again is a clean 404, not a 5xx.
	secondDelete := invokeGovernanceCredential(t, testUserID, http.MethodDelete, "")
	if secondDelete.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404: %s", secondDelete.Code, secondDelete.Body.String())
	}
}

func TestGovernanceCredential_AgentDeniedWrite(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceCredentialFixture(t)
	t.Cleanup(func() { resetGovernanceCredentialFixture(t) })
	testHandler.CredentialKeyring = testCredentialKeyring(t)
	t.Cleanup(func() { testHandler.CredentialKeyring = nil })

	req := httptest.NewRequest(http.MethodPut, "/api/workspaces/"+testWorkspaceID+"/governance/credential", bytes.NewReader([]byte(`{"value":"sk-fake"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req.Header.Set("X-Actor-Source", "task_token")
	req = withURLParam(req, "id", testWorkspaceID)
	recorder := httptest.NewRecorder()
	testHandler.PutGovernanceCredential(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("agent write = %d, want 403: %s", recorder.Code, recorder.Body.String())
	}
	assertSafeGovernanceProblem(t, recorder)
}

func TestGovernanceCredential_MissingKeyringReturnsSafeUnavailable(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceCredentialFixture(t)
	t.Cleanup(func() { resetGovernanceCredentialFixture(t) })
	// Deliberately leave testHandler.CredentialKeyring nil — the
	// "missing-key acceptance evidence" criterion: no keyring configured
	// must be safe (no panic, no plaintext fallback), not merely non-crashing.
	testHandler.CredentialKeyring = nil

	write := invokeGovernanceCredential(t, testUserID, http.MethodPut, `{"value":"sk-fake"}`)
	if write.Code != http.StatusServiceUnavailable {
		t.Fatalf("write with no keyring = %d, want 503: %s", write.Code, write.Body.String())
	}
	assertSafeGovernanceProblem(t, write)

	// GET status must still work without a keyring: presence is a metadata
	// read, not a decrypt.
	status := invokeGovernanceCredential(t, testUserID, http.MethodGet, "")
	if status.Code != http.StatusOK || decodeCredentialPresent(t, status) {
		t.Fatalf("status with no keyring = %d %s, want 200 credential_present=false", status.Code, status.Body.String())
	}
}

func TestGovernanceCredential_RotationOldKeyStillOpensNewKeyRewrites(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceCredentialFixture(t)
	t.Cleanup(func() { resetGovernanceCredentialFixture(t) })

	oldKeyring, err := credential.NewKeyring([]credential.VersionedKey{
		{ID: "rot-k1", Key: bytes.Repeat([]byte{0x21}, credential.KeySize)},
	})
	if err != nil {
		t.Fatal(err)
	}
	testHandler.CredentialKeyring = oldKeyring
	t.Cleanup(func() { testHandler.CredentialKeyring = nil })

	write := invokeGovernanceCredential(t, testUserID, http.MethodPut, `{"value":"sk-fake-rotate"}`)
	if write.Code != http.StatusOK {
		t.Fatalf("initial write failed: %d %s", write.Code, write.Body.String())
	}

	// Rotate: new keyring's primary is rot-k2, but it still knows rot-k1
	// for decrypting the row written above.
	rotatedKeyring, err := credential.NewKeyring([]credential.VersionedKey{
		{ID: "rot-k2", Key: bytes.Repeat([]byte{0x22}, credential.KeySize)},
		{ID: "rot-k1", Key: bytes.Repeat([]byte{0x21}, credential.KeySize)},
	})
	if err != nil {
		t.Fatal(err)
	}
	testHandler.CredentialKeyring = rotatedKeyring

	workspaceUUID, err := util.ParseUUID(testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := loadGovernanceCredentialPlaintext(context.Background(), testPool, rotatedKeyring, workspaceUUID, JevCredentialPurpose)
	if err != nil {
		t.Fatalf("expected rotated keyring to still open the row sealed under the retired key: %v", err)
	}
	if string(plaintext) != "sk-fake-rotate" {
		t.Fatalf("plaintext = %q", plaintext)
	}

	// New-key rewrite: write the same value back through the handler while
	// the rotated keyring is active; the stored envelope must now be keyed
	// by the new primary.
	rewrite := invokeGovernanceCredential(t, testUserID, http.MethodPut, `{"value":"sk-fake-rotate"}`)
	if rewrite.Code != http.StatusOK {
		t.Fatalf("rewrite failed: %d %s", rewrite.Code, rewrite.Body.String())
	}
	var storedKeyID string
	if err := testPool.QueryRow(context.Background(), `
		SELECT key_id FROM governance_workspace_credential WHERE workspace_id = $1 AND purpose = $2
	`, testWorkspaceID, JevCredentialPurpose).Scan(&storedKeyID); err != nil {
		t.Fatal(err)
	}
	if storedKeyID != "rot-k2" {
		t.Fatalf("stored key_id = %q after rewrite, want rot-k2", storedKeyID)
	}

	// rotation_version incremented across write -> rewrite, and the audit
	// trail recorded a rotate action, not a bare write.
	var rotationVersion int64
	if err := testPool.QueryRow(context.Background(), `
		SELECT rotation_version FROM governance_workspace_credential WHERE workspace_id = $1 AND purpose = $2
	`, testWorkspaceID, JevCredentialPurpose).Scan(&rotationVersion); err != nil {
		t.Fatal(err)
	}
	if rotationVersion != 2 {
		t.Fatalf("rotation_version = %d, want 2", rotationVersion)
	}
	var auditCount int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM governance_workspace_credential_audit
		WHERE workspace_id = $1 AND purpose = $2 AND action = 'rotate'
	`, testWorkspaceID, JevCredentialPurpose).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("expected one rotate audit row, got %d", auditCount)
	}
}

func TestGovernanceCredential_AuditNeverStoresCiphertextOrPlaintext(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceCredentialFixture(t)
	t.Cleanup(func() { resetGovernanceCredentialFixture(t) })
	testHandler.CredentialKeyring = testCredentialKeyring(t)
	t.Cleanup(func() { testHandler.CredentialKeyring = nil })

	write := invokeGovernanceCredential(t, testUserID, http.MethodPut, `{"value":"sk-fake-audit-check"}`)
	if write.Code != http.StatusOK {
		t.Fatalf("write failed: %d %s", write.Code, write.Body.String())
	}

	rows, err := testPool.Query(context.Background(), `
		SELECT action, rotation_version, key_id, actor_id FROM governance_workspace_credential_audit
		WHERE workspace_id = $1 AND purpose = $2
	`, testWorkspaceID, JevCredentialPurpose)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		found++
		var action, keyID, actorID string
		var rotationVersion int64
		if err := rows.Scan(&action, &rotationVersion, &keyID, &actorID); err != nil {
			t.Fatal(err)
		}
		if action != "write" {
			t.Fatalf("action = %q, want write", action)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("expected one audit row, got %d", found)
	}
}

func TestGovernanceCredential_InvalidRequestRejected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceCredentialFixture(t)
	t.Cleanup(func() { resetGovernanceCredentialFixture(t) })
	testHandler.CredentialKeyring = testCredentialKeyring(t)
	t.Cleanup(func() { testHandler.CredentialKeyring = nil })

	cases := []string{
		`{}`,
		`{"value":""}`,
		`{"value":"ok","extra":"field"}`,
		`not-json`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			resp := invokeGovernanceCredential(t, testUserID, http.MethodPut, body)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("body %q = %d, want 400: %s", body, resp.Code, resp.Body.String())
			}
		})
	}
}

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/pkg/credential"
)

// JevCredentialPurpose identifies the workspace-scoped TypeSafe/Jev API key
// row (CHE-714). Purposes are free-form strings scoped per workspace, so a
// future credential (a second provider key, say) gets its own purpose
// string rather than reusing this one.
const JevCredentialPurpose = "jev_api_key"

const governanceCredentialBodyMaxBytes = 16 << 10

type governanceCredentialWrite struct {
	Value string `json:"value"`
}

// GetGovernanceCredentialStatus reports only whether a credential is stored
// for this purpose — never the value or any fragment of it. This mirrors
// governanceConfigResponse.CredentialPresent, which today always reports
// false because nothing could write it yet.
func (h *Handler) GetGovernanceCredentialStatus(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := h.governanceWorkspaceAccess(w, r, false)
	if !ok {
		return
	}
	present, err := credentialPresent(r.Context(), h.DB, workspaceID, JevCredentialPurpose)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"credential_present": present})
}

// PutGovernanceCredential writes or replaces the workspace's Jev credential.
// The plaintext is sealed under the server's current primary key and the
// request body is discarded once sealed; nothing here logs, echoes, or
// otherwise retains it.
func (h *Handler) PutGovernanceCredential(w http.ResponseWriter, r *http.Request) {
	workspaceID, actorID, ok := h.governanceWorkspaceAccess(w, r, true)
	if !ok {
		return
	}
	if h.CredentialKeyring == nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "credential_unavailable")
		return
	}
	var body governanceCredentialWrite
	r.Body = http.MaxBytesReader(w, r.Body, governanceCredentialBodyMaxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Value == "" {
		writeGovernanceProblem(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.writeGovernanceCredential(r.Context(), workspaceID, actorID, JevCredentialPurpose, []byte(body.Value)); err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"credential_present": true})
}

// DeleteGovernanceCredential permanently removes the stored credential row
// and its ciphertext. This is a hard delete, not a soft/redacted one: the
// acceptance criteria require deletion to leave no readable value or
// fragment behind, and the audit trail records that a delete happened
// without retaining anything that could reconstruct the secret.
func (h *Handler) DeleteGovernanceCredential(w http.ResponseWriter, r *http.Request) {
	workspaceID, actorID, ok := h.governanceWorkspaceAccess(w, r, true)
	if !ok {
		return
	}
	deleted, err := h.deleteGovernanceCredential(r.Context(), workspaceID, actorID, JevCredentialPurpose)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	if !deleted {
		writeGovernanceProblem(w, r, http.StatusNotFound, "credential_not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"credential_present": false})
}

func credentialPresent(ctx context.Context, database dbExecutor, workspaceID pgtype.UUID, purpose string) (bool, error) {
	var exists bool
	err := database.QueryRow(ctx, `
		SELECT true FROM governance_workspace_credential
		WHERE workspace_id = $1 AND purpose = $2
	`, workspaceID, purpose).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exists, nil
}

// writeGovernanceCredential seals plaintext under the keyring's current
// primary key and upserts it. The record id (used as AAD, not as the
// primary key) is generated fresh on every write, including a replace over
// an existing row, so a rewritten credential is a new AEAD context rather
// than a value implicitly re-bound to a stale id.
func (h *Handler) writeGovernanceCredential(ctx context.Context, workspaceID, actorID pgtype.UUID, purpose string, plaintext []byte) error {
	recordID := uuid.New()
	aad := credential.AAD{
		WorkspaceID: uuidToString(workspaceID),
		RecordID:    recordID.String(),
		Purpose:     purpose,
	}
	env, err := h.CredentialKeyring.Seal(plaintext, aad)
	if err != nil {
		return err
	}

	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var rotationVersion int64
	err = tx.QueryRow(ctx, `
		INSERT INTO governance_workspace_credential (
			workspace_id, purpose, record_id, key_id, algorithm, nonce, ciphertext,
			rotation_version, created_by, updated_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, $8)
		ON CONFLICT (workspace_id, purpose) DO UPDATE SET
			record_id = EXCLUDED.record_id,
			key_id = EXCLUDED.key_id,
			algorithm = EXCLUDED.algorithm,
			nonce = EXCLUDED.nonce,
			ciphertext = EXCLUDED.ciphertext,
			rotation_version = governance_workspace_credential.rotation_version + 1,
			updated_by = EXCLUDED.updated_by,
			updated_at = now()
		RETURNING rotation_version
	`, workspaceID, purpose, recordID, env.KeyID, env.Algorithm, env.Nonce, env.Ciphertext, actorID).Scan(&rotationVersion)
	if err != nil {
		return err
	}

	action := "write"
	if rotationVersion > 1 {
		action = "rotate"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_workspace_credential_audit (
			workspace_id, purpose, record_id, action, rotation_version, key_id, actor_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, workspaceID, purpose, recordID, action, rotationVersion, env.KeyID, actorID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// deleteGovernanceCredential removes the credential row outright. It
// returns (false, nil) — not an error — when no row existed, so the caller
// can return a clean 404 rather than a 5xx for a delete that had nothing to
// do.
func (h *Handler) deleteGovernanceCredential(ctx context.Context, workspaceID, actorID pgtype.UUID, purpose string) (bool, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var recordID uuid.UUID
	var rotationVersion int64
	var keyID string
	err = tx.QueryRow(ctx, `
		DELETE FROM governance_workspace_credential
		WHERE workspace_id = $1 AND purpose = $2
		RETURNING record_id, rotation_version, key_id
	`, workspaceID, purpose).Scan(&recordID, &rotationVersion, &keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO governance_workspace_credential_audit (
			workspace_id, purpose, record_id, action, rotation_version, key_id, actor_id
		) VALUES ($1, $2, $3, 'delete', $4, $5, $6)
	`, workspaceID, purpose, recordID, rotationVersion, keyID, actorID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// loadGovernanceCredentialPlaintext decrypts the stored credential for
// internal server use (e.g. wiring a real Jev client). It is deliberately
// unexported and unreachable from any HTTP handler: the acceptance
// criteria require that no agent or API response can ever read back the
// value, so the only caller allowed to exist is server-side wiring that
// never serializes the result.
func loadGovernanceCredentialPlaintext(ctx context.Context, database dbExecutor, keyring *credential.Keyring, workspaceID pgtype.UUID, purpose string) ([]byte, error) {
	if keyring == nil {
		return nil, errors.New("governance credential: keyring not configured")
	}
	var recordID uuid.UUID
	var env credential.Envelope
	err := database.QueryRow(ctx, `
		SELECT record_id, key_id, algorithm, nonce, ciphertext
		FROM governance_workspace_credential
		WHERE workspace_id = $1 AND purpose = $2
	`, workspaceID, purpose).Scan(&recordID, &env.KeyID, &env.Algorithm, &env.Nonce, &env.Ciphertext)
	if err != nil {
		return nil, err
	}
	aad := credential.AAD{
		WorkspaceID: uuidToString(workspaceID),
		RecordID:    recordID.String(),
		Purpose:     purpose,
	}
	return keyring.Open(env, aad)
}

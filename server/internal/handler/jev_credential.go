package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/governance/credential"
)

const jevCredentialPurpose = "jev-provider-credential"

var ErrJevCredentialUnavailable = errors.New("jev credential unavailable")

type jevCredentialResponse struct {
	Present bool `json:"present"`
}

type putJevCredentialRequest struct {
	APIKey string `json:"api_key"`
}

// GetJevCredential reports only whether a credential exists. It deliberately
// has no read endpoint and never returns metadata that could help recover it.
func (h *Handler) GetJevCredential(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := h.governanceWorkspaceAccess(w, r, false)
	if !ok {
		return
	}
	var present bool
	err := h.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM governance_jev_credential WHERE workspace_id = $1)`, workspaceID).Scan(&present)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, jevCredentialResponse{Present: present})
}

// PutJevCredential writes or replaces an encrypted credential. The plaintext
// is decoded once, sealed before database I/O, and never appears in a response.
func (h *Handler) PutJevCredential(w http.ResponseWriter, r *http.Request) {
	workspaceID, actorID, ok := h.governanceWorkspaceAccess(w, r, true)
	if !ok {
		return
	}
	if h.JevCredentials == nil {
		writeFeatureDisabled(w, "jev_credentials_not_configured", "Jev credentials are not configured on this deployment")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, governanceConfigBodyMaxBytes)
	var request putJevCredentialRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeGovernanceProblem(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || strings.TrimSpace(request.APIKey) == "" {
		writeGovernanceProblem(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	envelope, err := h.JevCredentials.Seal([]byte(strings.TrimSpace(request.APIKey)), credential.Binding{WorkspaceID: uuidToString(workspaceID), CredentialID: "api-key", Purpose: jevCredentialPurpose})
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	_, err = h.DB.Exec(r.Context(), `INSERT INTO governance_jev_credential (workspace_id, envelope, updated_by) VALUES ($1, $2, $3) ON CONFLICT (workspace_id) DO UPDATE SET envelope = EXCLUDED.envelope, updated_by = EXCLUDED.updated_by, updated_at = now()`, workspaceID, encoded, actorID)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, jevCredentialResponse{Present: true})
}

func (h *Handler) DeleteJevCredential(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := h.governanceWorkspaceAccess(w, r, true)
	if !ok {
		return
	}
	if _, err := h.DB.Exec(r.Context(), `DELETE FROM governance_jev_credential WHERE workspace_id = $1`, workspaceID); err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ResolveJevCredential is an internal-only resolver for the Jev transport.
// It is intentionally not reachable from HTTP: callers receive plaintext only
// long enough to make an authorized provider call. A successful old-key open
// rewrites the stored envelope under the current key before returning.
func (h *Handler) ResolveJevCredential(ctx context.Context, workspaceID pgtype.UUID) ([]byte, error) {
	if h.JevCredentials == nil {
		return nil, ErrJevCredentialUnavailable
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, ErrJevCredentialUnavailable
	}
	defer tx.Rollback(ctx)
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT envelope FROM governance_jev_credential WHERE workspace_id = $1 FOR UPDATE`, workspaceID).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrJevCredentialUnavailable
		}
		return nil, ErrJevCredentialUnavailable
	}
	var envelope credential.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, ErrJevCredentialUnavailable
	}
	plaintext, replacement, err := h.JevCredentials.OpenAndRotate(envelope, credential.Binding{WorkspaceID: uuidToString(workspaceID), CredentialID: "api-key", Purpose: jevCredentialPurpose})
	if err != nil {
		return nil, ErrJevCredentialUnavailable
	}
	if replacement != nil {
		encoded, err := json.Marshal(replacement)
		if err != nil {
			return nil, ErrJevCredentialUnavailable
		}
		if _, err := tx.Exec(ctx, `UPDATE governance_jev_credential SET envelope = $2, updated_at = now() WHERE workspace_id = $1`, workspaceID, encoded); err != nil {
			return nil, ErrJevCredentialUnavailable
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, ErrJevCredentialUnavailable
	}
	return plaintext, nil
}

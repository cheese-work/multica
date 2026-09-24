package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ExportRedactionModeSmall is the only implemented CHE-755 export redaction
// mode: unconditional secret/credential masking via redact.Text, and nothing
// more. See ExportRedactionModeStrict for why "strict" cannot be selected yet.
const ExportRedactionModeSmall = "small"

// ExportRedactionModeStrict names Cheese's approved roadmap mode (broader PII
// redaction beyond secrets/credentials). It is accepted by the database CHECK
// constraint for forward compatibility but has NO implementation: the export
// handler in provenance.go fails closed on any mode other than
// ExportRedactionModeSmall, and this config endpoint refuses to select it too,
// so a workspace can never be left believing strict protection is active when
// it is not.
const ExportRedactionModeStrict = "strict"

const (
	exportRetentionMinDays = 1
	exportRetentionMaxDays = 3650
)

type workspaceExportPrivacyResponse struct {
	RedactionMode         string `json:"redaction_mode"`
	ManifestRetentionDays int32  `json:"manifest_retention_days"`
}

// GetWorkspaceExportPrivacy (CHE-766) returns the workspace's export privacy
// policy: the CHE-755 redaction mode and audit-manifest retention window.
// Human owners/admins only, same actor gate as ExportProvenance itself —
// members and agents can trigger an export but must not see or change the
// policy that governs what it redacts and how long its audit trail lives.
func (h *Handler) GetWorkspaceExportPrivacy(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	id := workspaceIDFromURL(r, "id")
	if _, ok := h.requireWorkspaceRole(w, r, id, "workspace not found", "owner", "admin"); !ok {
		return
	}
	idUUID, ok := parseUUIDOrBadRequest(w, id, "workspace id")
	if !ok {
		return
	}

	row, err := h.Queries.GetWorkspaceExportPrivacy(r.Context(), idUUID)
	if err != nil {
		slog.Error("get workspace export privacy failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", id)...)
		writeError(w, http.StatusInternalServerError, "failed to load export privacy settings")
		return
	}
	writeJSON(w, http.StatusOK, workspaceExportPrivacyResponse{
		RedactionMode:         row.ExportRedactionMode,
		ManifestRetentionDays: row.ExportManifestRetentionDays,
	})
}

type updateWorkspaceExportPrivacyRequest struct {
	RedactionMode         *string `json:"redaction_mode"`
	ManifestRetentionDays *int32  `json:"manifest_retention_days"`
}

// UpdateWorkspaceExportPrivacy (CHE-766) sets the workspace's export privacy
// policy. Owner/admin only, human actor only — no member or agent can raise
// export privilege through this or any other surface. Fails closed on an
// unimplemented or unrecognized mode rather than silently accepting it: see
// ExportRedactionModeStrict.
func (h *Handler) UpdateWorkspaceExportPrivacy(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	id := workspaceIDFromURL(r, "id")
	if _, ok := h.requireWorkspaceRole(w, r, id, "workspace not found", "owner", "admin"); !ok {
		return
	}
	idUUID, ok := parseUUIDOrBadRequest(w, id, "workspace id")
	if !ok {
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	dec.DisallowUnknownFields()
	var req updateWorkspaceExportPrivacyRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RedactionMode == nil && req.ManifestRetentionDays == nil {
		writeError(w, http.StatusBadRequest, "at least one of redaction_mode or manifest_retention_days is required")
		return
	}

	current, err := h.Queries.GetWorkspaceExportPrivacy(r.Context(), idUUID)
	if err != nil {
		slog.Error("update workspace export privacy: load current failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", id)...)
		writeError(w, http.StatusInternalServerError, "failed to load export privacy settings")
		return
	}

	mode := current.ExportRedactionMode
	if req.RedactionMode != nil {
		mode = strings.TrimSpace(*req.RedactionMode)
		// Fail closed: only the fully implemented mode may be selected. This
		// also blocks re-selecting an already-stored "strict" row from a future
		// migration default change — the handler, not the column default, is
		// the source of truth for what is actually enforced today.
		if mode != ExportRedactionModeSmall {
			writeError(w, http.StatusBadRequest, "redaction_mode must be \"small\" — strict mode is not yet implemented")
			return
		}
	}

	retentionDays := current.ExportManifestRetentionDays
	if req.ManifestRetentionDays != nil {
		retentionDays = *req.ManifestRetentionDays
		if retentionDays < exportRetentionMinDays || retentionDays > exportRetentionMaxDays {
			writeError(w, http.StatusBadRequest, "manifest_retention_days must be between 1 and 3650")
			return
		}
	}

	updated, err := h.Queries.UpdateWorkspaceExportPrivacy(r.Context(), db.UpdateWorkspaceExportPrivacyParams{
		ID:                          idUUID,
		ExportRedactionMode:         mode,
		ExportManifestRetentionDays: retentionDays,
	})
	if err != nil {
		slog.Error("update workspace export privacy failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", id)...)
		writeError(w, http.StatusInternalServerError, "failed to update export privacy settings")
		return
	}

	slog.Info("workspace export privacy updated", append(logger.RequestAttrs(r),
		"workspace_id", id, "redaction_mode", updated.ExportRedactionMode,
		"manifest_retention_days", updated.ExportManifestRetentionDays)...)

	writeJSON(w, http.StatusOK, workspaceExportPrivacyResponse{
		RedactionMode:         updated.ExportRedactionMode,
		ManifestRetentionDays: updated.ExportManifestRetentionDays,
	})
}

// exportRedactionModeEnforced reports whether mode is the one CHE-755's
// export handler is actually allowed to run under. Shared by the handler and
// its tests so the fail-closed check has one definition.
func exportRedactionModeEnforced(mode string) bool {
	return mode == ExportRedactionModeSmall
}

var errExportRedactionModeUnimplemented = errors.New("export redaction mode is not implemented")

// requireEnforcedExportRedactionMode is called from ExportProvenance before
// any record leaves the server. A workspace row can only ever hold "small" or
// "strict" (DB CHECK), but only "small" is implemented — this is the runtime
// backstop for that gap, so a future default or manual DB edit that sets
// "strict" cannot silently start serving unredacted-by-strict-standards data
// under a policy no code actually enforces.
func requireEnforcedExportRedactionMode(mode string) error {
	if !exportRedactionModeEnforced(mode) {
		return errExportRedactionModeUnimplemented
	}
	return nil
}

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// exportPrivacyPolicyChangedActivity is the activity_log action name for a
// CHE-766 export-privacy policy change. Stable across releases — this is a
// durable audit key, not a display string.
const exportPrivacyPolicyChangedActivity = "export_privacy_policy_changed"

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
	// CHE-766: same kill switch as ExportProvenance — disabling it hides and
	// blocks the config surface along with the capability it configures.
	if !featureflags.ExportPrivacyControlsEnabled(r.Context(), h.FeatureFlags) {
		writeError(w, http.StatusServiceUnavailable, "export privacy controls are currently disabled")
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
	// CHE-766: same kill switch as ExportProvenance.
	if !featureflags.ExportPrivacyControlsEnabled(r.Context(), h.FeatureFlags) {
		writeError(w, http.StatusServiceUnavailable, "export privacy controls are currently disabled")
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
	actorUUID, err := util.ParseUUID(requestUserID(r))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "user not authenticated")
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

	var requestedMode *string
	if req.RedactionMode != nil {
		trimmed := strings.TrimSpace(*req.RedactionMode)
		// Fail closed: only the fully implemented mode may be selected. This
		// also blocks re-selecting an already-stored "strict" row from a future
		// migration default change — the handler, not the column default, is
		// the source of truth for what is actually enforced today.
		if trimmed != ExportRedactionModeSmall {
			writeError(w, http.StatusBadRequest, "redaction_mode must be \"small\" — strict mode is not yet implemented")
			return
		}
		requestedMode = &trimmed
	}
	if req.ManifestRetentionDays != nil {
		if *req.ManifestRetentionDays < exportRetentionMinDays || *req.ManifestRetentionDays > exportRetentionMaxDays {
			writeError(w, http.StatusBadRequest, "manifest_retention_days must be between 1 and 3650")
			return
		}
	}

	updated, changed, err := h.updateExportPrivacyAtomically(r.Context(), idUUID, actorUUID, requestedMode, req.ManifestRetentionDays)
	if err != nil {
		slog.Error("update workspace export privacy failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", id)...)
		writeError(w, http.StatusInternalServerError, "failed to update export privacy settings")
		return
	}

	slog.Info("workspace export privacy updated", append(logger.RequestAttrs(r),
		"workspace_id", id, "actor_id", requestUserID(r), "changed", changed,
		"redaction_mode", updated.ExportRedactionMode,
		"manifest_retention_days", updated.ExportManifestRetentionDays)...)

	writeJSON(w, http.StatusOK, workspaceExportPrivacyResponse{
		RedactionMode:         updated.ExportRedactionMode,
		ManifestRetentionDays: updated.ExportManifestRetentionDays,
	})
}

// updateExportPrivacyAtomically reads the current policy FOR UPDATE, applies
// the requested fields, writes the new row, and records an actor-attributed
// old→new activity_log entry — all inside one transaction. If the audit
// insert fails, the whole transaction (including the policy write) rolls
// back: a durable history of who changed retention/mode and when is a
// precondition for the change taking effect, not a best-effort side note.
// This is what closes the review finding that a policy change (e.g.
// shortening retention right before a scheduled sweep) could happen with
// only a non-durable slog.Info line to show for it.
//
// changed reports whether anything actually differed from the stored row,
// so a no-op PATCH (both fields resent unchanged) does not manufacture a
// misleading audit entry.
func (h *Handler) updateExportPrivacyAtomically(ctx context.Context, workspaceID, actorID pgtype.UUID, requestedMode *string, requestedRetentionDays *int32) (db.UpdateWorkspaceExportPrivacyRow, bool, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.UpdateWorkspaceExportPrivacyRow{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := h.Queries.WithTx(tx)

	current, err := q.GetWorkspaceExportPrivacyForUpdate(ctx, workspaceID)
	if err != nil {
		return db.UpdateWorkspaceExportPrivacyRow{}, false, fmt.Errorf("load current (for update): %w", err)
	}

	mode := current.ExportRedactionMode
	if requestedMode != nil {
		mode = *requestedMode
	}
	retentionDays := current.ExportManifestRetentionDays
	if requestedRetentionDays != nil {
		retentionDays = *requestedRetentionDays
	}
	changed := mode != current.ExportRedactionMode || retentionDays != current.ExportManifestRetentionDays

	updated, err := q.UpdateWorkspaceExportPrivacy(ctx, db.UpdateWorkspaceExportPrivacyParams{
		ID:                          workspaceID,
		ExportRedactionMode:         mode,
		ExportManifestRetentionDays: retentionDays,
	})
	if err != nil {
		return db.UpdateWorkspaceExportPrivacyRow{}, false, fmt.Errorf("update: %w", err)
	}

	if changed {
		details, err := json.Marshal(exportPrivacyAuditDetails{
			FromRedactionMode:         current.ExportRedactionMode,
			ToRedactionMode:           updated.ExportRedactionMode,
			FromManifestRetentionDays: current.ExportManifestRetentionDays,
			ToManifestRetentionDays:   updated.ExportManifestRetentionDays,
		})
		if err != nil {
			return db.UpdateWorkspaceExportPrivacyRow{}, false, fmt.Errorf("marshal audit details: %w", err)
		}
		if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
			WorkspaceID: workspaceID,
			ActorType:   pgtype.Text{String: "member", Valid: true},
			ActorID:     actorID,
			Action:      exportPrivacyPolicyChangedActivity,
			Details:     details,
		}); err != nil {
			// Fails the whole transaction: a policy change with no durable audit
			// record is exactly the gap this handler exists to close, so the
			// change itself must not persist if the record of it cannot.
			return db.UpdateWorkspaceExportPrivacyRow{}, false, fmt.Errorf("insert audit record: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return db.UpdateWorkspaceExportPrivacyRow{}, false, fmt.Errorf("commit: %w", err)
	}
	return updated, changed, nil
}

// exportPrivacyAuditDetails is the activity_log.details payload for
// exportPrivacyPolicyChangedActivity. Field names are stable — anything
// reading this history later (a dashboard, an export) depends on them.
type exportPrivacyAuditDetails struct {
	FromRedactionMode         string `json:"from_redaction_mode"`
	ToRedactionMode           string `json:"to_redaction_mode"`
	FromManifestRetentionDays int32  `json:"from_manifest_retention_days"`
	ToManifestRetentionDays   int32  `json:"to_manifest_retention_days"`
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

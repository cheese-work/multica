package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type applyInstructionPairRequest struct {
	Context                          *string `json:"context"`
	SquadID                          string  `json:"squad_id"`
	Instructions                     *string `json:"instructions"`
	ExpectedContextBeforeDigest      string  `json:"expected_context_before_digest"`
	ExpectedInstructionsBeforeDigest string  `json:"expected_instructions_before_digest"`
}

type applyInstructionPairResult struct {
	WorkspaceID              string `json:"workspace_id"`
	ContextBeforeDigest      string `json:"context_before_digest"`
	ContextAfterDigest       string `json:"context_after_digest"`
	SquadID                  string `json:"squad_id"`
	InstructionsBeforeDigest string `json:"instructions_before_digest"`
	InstructionsAfterDigest  string `json:"instructions_after_digest"`
}

type instructionPairRejectReason int

const (
	instructionPairRejectNone instructionPairRejectReason = iota
	instructionPairRejectMalformedDigest
	instructionPairRejectStaleDigest
	instructionPairRejectNotFound
	instructionPairRejectInternal
)

func (h *Handler) ApplyInstructionPair(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}

	workspaceID := workspaceIDFromURL(r, "id")
	workspaceUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil || !utf8.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var rawFields map[string]json.RawMessage
	var req applyInstructionPairRequest
	if err := json.Unmarshal(body, &rawFields); err != nil ||
		!hermesExceptionOnlyAllowedKeys(rawFields,
			"context", "squad_id", "instructions",
			"expected_context_before_digest", "expected_instructions_before_digest") ||
		len(rawFields) != 5 || json.Unmarshal(body, &req) != nil {
		writeError(w, http.StatusBadRequest, "request must include exactly context, squad_id, instructions, expected_context_before_digest, and expected_instructions_before_digest")
		return
	}
	if req.Context == nil || req.Instructions == nil {
		writeError(w, http.StatusBadRequest, "context and instructions are required")
		return
	}
	squadUUID, ok := parseUUIDOrBadRequest(w, req.SquadID, "squad id")
	if !ok {
		return
	}

	result, reason, err := h.applyInstructionPairCAS(r.Context(), workspaceUUID, squadUUID, workspaceID, req)
	if reason != instructionPairRejectNone {
		writeInstructionPairRejection(w, err, reason)
		return
	}

	if workspace, err := h.Queries.GetWorkspace(r.Context(), workspaceUUID); err == nil {
		h.publish(protocol.EventWorkspaceUpdated, workspaceID, "member", requestUserID(r), map[string]any{
			"workspace": h.workspaceToResponse(workspace),
		})
	} else {
		slog.Warn("instruction pair applied but workspace event readback failed", "workspace_id", workspaceID, "error", err)
	}
	if squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{
		ID: squadUUID, WorkspaceID: workspaceUUID,
	}); err == nil {
		if response, err := h.squadToResponseWithPreview(r.Context(), squad); err == nil {
			h.publish(protocol.EventSquadUpdated, workspaceID, "member", requestUserID(r), map[string]any{
				"squad": response,
			})
		} else {
			slog.Warn("instruction pair applied but squad event readback failed", "squad_id", req.SquadID, "error", err)
		}
	} else {
		slog.Warn("instruction pair applied but squad readback failed", "squad_id", req.SquadID, "error", err)
	}

	slog.Info("workspace and squad instructions applied atomically",
		"actor_id", requestUserID(r),
		"workspace_id", result.WorkspaceID,
		"context_before_digest", result.ContextBeforeDigest,
		"context_after_digest", result.ContextAfterDigest,
		"squad_id", result.SquadID,
		"instructions_before_digest", result.InstructionsBeforeDigest,
		"instructions_after_digest", result.InstructionsAfterDigest,
	)
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) applyInstructionPairCAS(
	ctx context.Context,
	workspaceUUID, squadUUID pgtype.UUID,
	workspaceID string,
	req applyInstructionPairRequest,
) (applyInstructionPairResult, instructionPairRejectReason, error) {
	if !isWellFormedDigestHex(req.ExpectedContextBeforeDigest) ||
		!isWellFormedDigestHex(req.ExpectedInstructionsBeforeDigest) {
		return applyInstructionPairResult{}, instructionPairRejectMalformedDigest, nil
	}

	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return applyInstructionPairResult{}, instructionPairRejectInternal, err
	}
	defer tx.Rollback(ctx)

	var currentContext string
	if err := tx.QueryRow(ctx, `SELECT context FROM workspace WHERE id = $1 FOR UPDATE`, workspaceUUID).Scan(&currentContext); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return applyInstructionPairResult{}, instructionPairRejectNotFound, nil
		}
		return applyInstructionPairResult{}, instructionPairRejectInternal, err
	}

	var currentInstructions string
	if err := tx.QueryRow(ctx, instructionPairLockSquadQuery, squadUUID, workspaceUUID).Scan(&currentInstructions); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return applyInstructionPairResult{}, instructionPairRejectNotFound, nil
		}
		return applyInstructionPairResult{}, instructionPairRejectInternal, err
	}

	contextBeforeDigest := instructionPairDigest(currentContext)
	instructionsBeforeDigest := instructionPairDigest(currentInstructions)
	if contextBeforeDigest != strings.ToLower(req.ExpectedContextBeforeDigest) ||
		instructionsBeforeDigest != strings.ToLower(req.ExpectedInstructionsBeforeDigest) {
		return applyInstructionPairResult{}, instructionPairRejectStaleDigest, nil
	}

	if tag, err := tx.Exec(ctx, `UPDATE workspace SET context = $1, updated_at = now() WHERE id = $2`, *req.Context, workspaceUUID); err != nil {
		return applyInstructionPairResult{}, instructionPairRejectInternal, err
	} else if tag.RowsAffected() != 1 {
		return applyInstructionPairResult{}, instructionPairRejectInternal, errors.New("workspace row disappeared during instruction pair update")
	}
	if tag, err := tx.Exec(ctx, `UPDATE squad SET instructions = $1, updated_at = now() WHERE id = $2 AND workspace_id = $3`, *req.Instructions, squadUUID, workspaceUUID); err != nil {
		return applyInstructionPairResult{}, instructionPairRejectInternal, err
	} else if tag.RowsAffected() != 1 {
		return applyInstructionPairResult{}, instructionPairRejectInternal, errors.New("squad row disappeared during instruction pair update")
	}
	if err := tx.Commit(ctx); err != nil {
		return applyInstructionPairResult{}, instructionPairRejectInternal, err
	}

	return applyInstructionPairResult{
		WorkspaceID:              workspaceID,
		ContextBeforeDigest:      contextBeforeDigest,
		ContextAfterDigest:       instructionPairDigest(*req.Context),
		SquadID:                  req.SquadID,
		InstructionsBeforeDigest: instructionsBeforeDigest,
		InstructionsAfterDigest:  instructionPairDigest(*req.Instructions),
	}, instructionPairRejectNone, nil
}

const instructionPairLockSquadQuery = `SELECT instructions FROM squad WHERE id = $1 AND workspace_id = $2 FOR UPDATE`

func instructionPairDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func writeInstructionPairRejection(w http.ResponseWriter, err error, reason instructionPairRejectReason) {
	switch reason {
	case instructionPairRejectMalformedDigest:
		writeError(w, http.StatusBadRequest, "expected before digests must be 64-character hex-encoded sha256 digests")
	case instructionPairRejectStaleDigest:
		writeError(w, http.StatusConflict, "one or both expected before digests do not match current live values; re-read and retry")
	case instructionPairRejectNotFound:
		writeError(w, http.StatusNotFound, "workspace or squad not found")
	default:
		slog.Warn("atomic instruction pair update failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to apply instruction pair")
	}
}

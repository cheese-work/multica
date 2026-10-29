package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2457: decision-bound agent writes on the CHE-1862 governed fields
// (workspace.context, squad.instructions, agent.instructions,
// project.description). An agent may write one when the request carries an
// answered decision bound to the exact change:
//
//	{<field>: <new value>, "decision_id": <decision request id>, "before_sha256": ..., "after_sha256": ...}
//
// This is the alternative to the vote receipt (vote_issue), not an addition:
// a body naming both is refused. verifyDecisionBinding (CHE-2455) holds the
// decision checks and records the single use; this file adds the field checks
// the vote path also makes:
//
//  1. before_sha256 is the sha256 of the live value, after_sha256 of the new
//     value (raw UTF-8 bytes, no normalisation).
//  2. Every governed:human block is byte-identical before and after (the
//     vote path's check 7).
//
// The write is the CHE-1300 compare-and-swap on before_sha256 in the same
// transaction as the single-use row, so a refused or stale write consumes no
// decision. The field label in the ask and the allow list is the column kind,
// for example "squad.instructions".

// decisionApply runs the checks and the write for one agent request; ok is
// true only after the write committed. Every failure response is already written.
func (h *Handler) decisionApply(w http.ResponseWriter, r *http.Request, writerID string, raw map[string]json.RawMessage, c ownerCASColumn, workspaceID, id string) (hermesExceptionSwapResult, bool) {
	fail := func(status int, message string) (hermesExceptionSwapResult, bool) {
		writeError(w, status, message)
		return hermesExceptionSwapResult{}, false
	}
	if !hermesExceptionOnlyAllowedKeys(raw, c.field, decisionIDKey, voteBeforeKey, voteAfterKey) {
		return fail(http.StatusBadRequest, "this request may only include "+c.field+", "+decisionIDKey+", "+voteBeforeKey+" and "+voteAfterKey)
	}
	content, ok := ownerCASRawString(raw, c.field)
	if !ok || strings.ContainsRune(content, 0) {
		return fail(http.StatusBadRequest, c.field+" must be present exactly once as a string without NUL bytes")
	}
	decisionRaw, _ := ownerCASRawString(raw, decisionIDKey)
	before, _ := ownerCASRawString(raw, voteBeforeKey)
	after, _ := ownerCASRawString(raw, voteAfterKey)
	decisionID, err := util.ParseUUID(decisionRaw)
	if err != nil || !isWellFormedDigestHex(before) || !isWellFormedDigestHex(after) {
		return fail(http.StatusBadRequest, decisionIDKey+" must be a decision request id and "+voteBeforeKey+"/"+voteAfterKey+" 64-character hex sha256 digests")
	}
	before, after = strings.ToLower(before), strings.ToLower(after)
	if !h.instructionTokenLimitOK(w, r, c, workspaceID, id, content) {
		return hermesExceptionSwapResult{}, false
	}

	wsUUID, _ := util.ParseUUID(workspaceID)
	idUUID, err := util.ParseUUID(id)
	if err != nil {
		return fail(http.StatusInternalServerError, "failed to verify decision")
	}
	live, err := h.governedLiveValue(r.Context(), c, wsUUID, idUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(http.StatusNotFound, "target row not found")
	}
	if err != nil {
		return fail(http.StatusInternalServerError, "failed to verify decision")
	}
	if reason := governedChangeRefusal(live, content, before, after); reason != "" {
		return fail(http.StatusForbidden, "decision rejected: "+reason)
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		return fail(http.StatusInternalServerError, "failed to verify decision")
	}
	defer tx.Rollback(r.Context())
	gate := func(ctx context.Context, _ *db.Queries, ws, agentID pgtype.UUID, field string) (bool, bool) {
		return h.delegatedWriteAllowed(ctx, ws, uuidToString(agentID), field)
	}
	agentUUID, err := util.ParseUUID(writerID)
	if err != nil {
		return fail(http.StatusInternalServerError, "failed to verify decision")
	}
	err = verifyDecisionBinding(r.Context(), h.Queries.WithTx(tx), gate, wsUUID, decisionID, agentUUID, c.kind, before, after)
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		return fail(refusal.status, "decision rejected: "+refusal.message)
	}
	if err != nil {
		return fail(http.StatusInternalServerError, "failed to verify decision")
	}

	result, rejected, err := ownerCASWriteTx(r.Context(), tx, c, workspaceID, id, content, before)
	if rejected == hermesExceptionRejectNone {
		if err = tx.Commit(r.Context()); err != nil {
			rejected = hermesExceptionRejectInternal
		}
	}
	if rejected != hermesExceptionRejectNone {
		hermesExceptionWriteRejection(w, r, rejected, err)
		return hermesExceptionSwapResult{}, false
	}
	return result, true
}

// governedLiveValue reads the governed column of the row in the workspace;
// a NULL column reads as empty. pgx.ErrNoRows when the row is not there.
func (h *Handler) governedLiveValue(ctx context.Context, c ownerCASColumn, wsUUID, idUUID pgtype.UUID) (string, error) {
	var live string
	err := h.DB.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(%[1]s, '') FROM %[2]s WHERE id = $1 AND %[3]s = $2`, c.col, c.table, c.scopeCol), idUUID, wsUUID).Scan(&live)
	return live, err
}

// governedChangeRefusal returns why the change from live to content does not
// match the declared digests or touches a governed:human block, or "" when
// both hold. The vote path and the decision path share it.
func governedChangeRefusal(live, content, before, after string) string {
	switch {
	case hermesExceptionDigest([]byte(live)) != before:
		return voteBeforeKey + " does not match the live value"
	case hermesExceptionDigest([]byte(content)) != after:
		return voteAfterKey + " does not match the new value"
	case !protectedBlocksUnchanged(live, content):
		return "a governed:human block changed"
	}
	return ""
}

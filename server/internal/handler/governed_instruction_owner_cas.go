package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-1300: owner-only, atomic expected-before-digest compare-and-swap for the
// three governed instruction fields — workspace.context, squad.instructions
// and agent.instructions.
//
// This is a separate path from the CHE-764 Hermes exception
// (governed_instruction_hermes_exception.go). It reuses that file's digest,
// digest-format and rejection helpers but widens nothing: the Hermes agent,
// workspace and squad constants are untouched, and every machine actor
// (`mat_` agent task token, in-process agent stamp, `mcn_` cloud-node PAT)
// keeps the CHE-455 default denial. The Hermes exception is evaluated first
// and the CHE-455 guard second, so a machine actor never reaches this path.
//
// Entry: a PUT/PATCH whose body carries an `expected_before_digest` key (any
// value, matched case-insensitively like encoding/json). A body without that
// key is the ordinary human update, unchanged. Once the key is present the
// request must satisfy all of:
//
//  1. Authorization comes from the server-authenticated identity only — the
//     X-User-ID the auth middleware stamps, resolved to a workspace member
//     row — never a body field. The caller must hold the workspace `owner`
//     role. Admins, plain members and agent owners get 403.
//  2. The body holds exactly the governed field and the digest, nothing else,
//     so no other field is silently dropped by the early return.
//  3. The digest is a JSON string of 64 hex characters (400 before any write).
//  4. The compare happens inside the UPDATE statement (see ownerCASWrite), so
//     a stale digest writes nothing and concurrent writers produce one winner.
//     Stale is 409.
//
// Digest contract — never substitute one convention for the other:
//
//   - This path and the Hermes exception hash the RAW UTF-8 bytes of the stored
//     value (sha256, lowercase hex; a NULL workspace.context hashes as empty).
//   - The CHE-1286 owner-apply card manifest strips ONE terminal LF before
//     hashing. A manifest digest of a value that ends in LF therefore does not
//     match here: it is refused as stale (409), never accepted. Callers hash
//     the exact bytes they read or send, with no normalisation.
//
// The post-write digest in the response is computed by the database over the
// stored column inside the same transaction and compared with the digest of
// the bytes that were sent; any difference rolls the write back.
//
// Rollback is the same call reversed: the recorded after_digest is the
// expected current value and the saved before bytes are the replacement.
//
// Candidate text is never logged: the audit line carries ids and digests only.

// ownerCASColumn names one governed column. All values are compile-time
// constants interpolated into SQL; none comes from a request.
type ownerCASColumn struct {
	kind  string // audit label, e.g. "squad.instructions"
	field string // JSON body key
	table string
	col   string
	// scopeCol is the column that must equal the caller's workspace id: the
	// target can never be written through another workspace's route.
	scopeCol string
}

var (
	ownerCASWorkspaceContext  = ownerCASColumn{"workspace.context", "context", "workspace", "context", "id"}
	ownerCASSquadInstructions = ownerCASColumn{"squad.instructions", "instructions", "squad", "instructions", "workspace_id"}
	ownerCASAgentInstructions = ownerCASColumn{"agent.instructions", "instructions", "agent", "instructions", "workspace_id"}
)

const ownerCASDigestKey = "expected_before_digest"

// digestSQL is the SQL spelling of hermesExceptionDigest over a column.
func (c ownerCASColumn) digestSQL() string {
	return fmt.Sprintf(`encode(sha256(convert_to(COALESCE(%s, ''), 'UTF8')), 'hex')`, c.col)
}

// ownerCASRequested reports whether the body asks for the digest path.
func ownerCASRequested(raw map[string]json.RawMessage) bool {
	return rawFieldsHasKeyFold(raw, ownerCASDigestKey)
}

// ownerCASRawString returns the value of a JSON string key, matched
// case-insensitively. ok is false when the key is absent more than once,
// null, or not a string (a duplicate under two casings is ambiguous, so it is
// refused rather than guessed).
func ownerCASRawString(raw map[string]json.RawMessage, key string) (string, bool) {
	var found json.RawMessage
	matches := 0
	for k, v := range raw {
		if strings.EqualFold(k, key) {
			found = v
			matches++
		}
	}
	if matches != 1 {
		return "", false
	}
	var s *string
	if err := json.Unmarshal(found, &s); err != nil || s == nil {
		return "", false
	}
	return *s, true
}

// ownerCASAuthorize enforces the owner-only rule. actorType must come from the
// same h.resolveActor call the handler already made; role is the caller's role
// in the target workspace, loaded by the handler from the authenticated user.
func ownerCASAuthorize(w http.ResponseWriter, r *http.Request, actorType, role string) bool {
	if isGovernedFieldMachineActor(r, actorType) || !roleAllowed(role, "owner") {
		writeError(w, http.StatusForbidden, "only a workspace owner may write "+ownerCASDigestKey+" guarded governed fields")
		return false
	}
	return true
}

// ownerCASParse validates the body shape and returns the new content and the
// lowercased digest. It writes the 400 itself.
func ownerCASParse(w http.ResponseWriter, raw map[string]json.RawMessage, c ownerCASColumn) (content, digest string, ok bool) {
	if !hermesExceptionOnlyAllowedKeys(raw, c.field, ownerCASDigestKey) {
		writeError(w, http.StatusBadRequest, "this request may only include "+c.field+" and "+ownerCASDigestKey)
		return "", "", false
	}
	content, ok = ownerCASRawString(raw, c.field)
	if !ok {
		writeError(w, http.StatusBadRequest, c.field+" must be present exactly once as a string")
		return "", "", false
	}
	if strings.ContainsRune(content, 0) {
		writeError(w, http.StatusBadRequest, c.field+" must not contain NUL bytes")
		return "", "", false
	}
	digest, _ = ownerCASRawString(raw, ownerCASDigestKey)
	if !isWellFormedDigestHex(digest) {
		writeError(w, http.StatusBadRequest, ownerCASDigestKey+" must be a 64-character hex-encoded sha256 digest")
		return "", "", false
	}
	return content, strings.ToLower(digest), true
}

// ownerCASWrite performs the compare-and-swap. The digest comparison is part
// of the UPDATE's WHERE clause, so Postgres re-evaluates it against the latest
// row version if a concurrent writer committed first; there is no
// read-then-write window. Zero rows updated means stale or missing, told apart
// by a read that writes nothing.
func (h *Handler) ownerCASWrite(ctx context.Context, c ownerCASColumn, workspaceID, id, newContent, expectedDigest string) (hermesExceptionSwapResult, hermesExceptionRejectReason, error) {
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}
	idUUID, err := util.ParseUUID(id)
	if err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}
	defer tx.Rollback(ctx)

	var storedDigest string
	err = tx.QueryRow(ctx, fmt.Sprintf(
		`UPDATE %[1]s SET %[2]s = $1, updated_at = now() WHERE id = $2 AND %[3]s = $3 AND %[4]s = $4 RETURNING %[5]s`,
		c.table, c.col, c.scopeCol, c.digestSQL(), c.digestSQL(),
	), newContent, idUUID, wsUUID, expectedDigest).Scan(&storedDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE id = $1 AND %s = $2)`, c.table, c.scopeCol), idUUID, wsUUID).Scan(&exists); err != nil {
			return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
		}
		if !exists {
			return hermesExceptionSwapResult{}, hermesExceptionRejectNotFound, nil
		}
		return hermesExceptionSwapResult{}, hermesExceptionRejectStaleDigest, nil
	}
	if err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}
	// Read the digest back from the stored column before committing.
	if want := hermesExceptionDigest([]byte(newContent)); storedDigest != want {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal,
			fmt.Errorf("post-write digest mismatch: stored %s, sent %s", storedDigest, want)
	}
	if err := tx.Commit(ctx); err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}
	return hermesExceptionSwapResult{BeforeDigest: expectedDigest, AfterDigest: storedDigest}, hermesExceptionRejectNone, nil
}

// ownerCASApply runs authorize, parse, write and audit for one request. It
// returns ok=true only after the write committed; every failure response has
// already been written.
func (h *Handler) ownerCASApply(w http.ResponseWriter, r *http.Request, actorType, role string, raw map[string]json.RawMessage, c ownerCASColumn, workspaceID, id string) (hermesExceptionSwapResult, bool) {
	if !ownerCASAuthorize(w, r, actorType, role) {
		logOwnerCASOutcome(r, c, id, hermesExceptionSwapResult{}, hermesExceptionRejectNone, "forbidden", nil)
		return hermesExceptionSwapResult{}, false
	}
	content, digest, ok := ownerCASParse(w, raw, c)
	if !ok {
		return hermesExceptionSwapResult{}, false
	}
	result, reason, err := h.ownerCASWrite(r.Context(), c, workspaceID, id, content, digest)
	logOwnerCASOutcome(r, c, id, result, reason, "", err)
	if reason != hermesExceptionRejectNone {
		hermesExceptionWriteRejection(w, r, reason, err)
		return hermesExceptionSwapResult{}, false
	}
	return result, true
}

// logOwnerCASOutcome writes one audit line per attempt. Ids and digests only.
func logOwnerCASOutcome(r *http.Request, c ownerCASColumn, id string, result hermesExceptionSwapResult, reason hermesExceptionRejectReason, denied string, err error) {
	attrs := append(logger.RequestAttrs(r),
		"che_issue", "CHE-1300",
		"actor_user_id", requestUserID(r),
		"task_id", r.Header.Get("X-Task-ID"),
		"target_kind", c.kind,
		"target_id", id,
		"reject_reason", int(reason),
	)
	if denied != "" {
		attrs = append(attrs, "denied", denied)
	}
	if result.BeforeDigest != "" {
		attrs = append(attrs, "before_digest", result.BeforeDigest, "after_digest", result.AfterDigest)
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	if reason == hermesExceptionRejectNone && denied == "" && err == nil {
		slog.Info("owner governed-field CAS write applied", attrs...)
		return
	}
	slog.Warn("owner governed-field CAS write refused", attrs...)
}

// ownerCASUpdateWorkspace handles an UpdateWorkspace request that carries the
// digest key. Called after the Hermes exception and the CHE-455 guard.
func (h *Handler) ownerCASUpdateWorkspace(w http.ResponseWriter, r *http.Request, workspaceID, actorType string, raw map[string]json.RawMessage) {
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	result, ok := h.ownerCASApply(w, r, actorType, member.Role, raw, ownerCASWorkspaceContext, workspaceID, workspaceID)
	if !ok {
		return
	}
	idUUID, _ := util.ParseUUID(workspaceID)
	ws, err := h.Queries.GetWorkspace(r.Context(), idUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload workspace after guarded write")
		return
	}
	resp := h.workspaceToResponse(ws)
	h.publish(protocol.EventWorkspaceUpdated, workspaceID, "member", requestUserID(r), map[string]any{"workspace": resp})
	writeJSON(w, http.StatusOK, map[string]any{"workspace": resp, "before_digest": result.BeforeDigest, "after_digest": result.AfterDigest})
}

// ownerCASUpdateSquad is the squad.instructions analogue.
func (h *Handler) ownerCASUpdateSquad(w http.ResponseWriter, r *http.Request, workspaceID string, member db.Member, squad db.Squad, actorType string, raw map[string]json.RawMessage) {
	result, ok := h.ownerCASApply(w, r, actorType, member.Role, raw, ownerCASSquadInstructions, workspaceID, uuidToString(squad.ID))
	if !ok {
		return
	}
	wsUUID, _ := util.ParseUUID(workspaceID)
	updated, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{ID: squad.ID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload squad after guarded write")
		return
	}
	resp, err := h.squadToResponseWithPreview(r.Context(), updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load squad member preview")
		return
	}
	h.publish(protocol.EventSquadUpdated, workspaceID, "member", requestUserID(r), map[string]any{"squad": resp})
	writeJSON(w, http.StatusOK, map[string]any{"squad": resp, "before_digest": result.BeforeDigest, "after_digest": result.AfterDigest})
}

// ownerCASUpdateAgent is the agent.instructions analogue.
func (h *Handler) ownerCASUpdateAgent(w http.ResponseWriter, r *http.Request, existing db.Agent, actorType string, raw map[string]json.RawMessage) {
	workspaceID := uuidToString(existing.WorkspaceID)
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	result, ok := h.ownerCASApply(w, r, actorType, member.Role, raw, ownerCASAgentInstructions, workspaceID, uuidToString(existing.ID))
	if !ok {
		return
	}
	updated, err := h.Queries.GetAgent(r.Context(), existing.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload agent after guarded write")
		return
	}
	resp, ok := h.updatedAgentResponse(w, r, updated)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": resp, "before_digest": result.BeforeDigest, "after_digest": result.AfterDigest})
}

package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5491 (S2b): the operation receipt of a status move. A move that carries an
// operation_key writes one issue_handoff_operation row, first in its status
// transaction, whatever the handler type (agent, squad, member or none). A retry
// of the same request (same key, same request hash, same actor) returns the first
// response and writes nothing. The same key with another body or actor is refused
// operation_key_reused. The handoff sweep deletes receipts older than 7 days, so
// deduplication holds for 7 days only: a later retry is a new move.

// handoffOperationTTL is how long a receipt deduplicates retries.
const handoffOperationTTL = 7 * 24 * time.Hour

// handoffOperationSeed is what the status transaction needs to write the receipt.
type handoffOperationSeed struct {
	workspaceID pgtype.UUID
	issueID     pgtype.UUID
	key         pgtype.UUID
	actorType   string
	actorID     pgtype.UUID
	hash        string
	// prefix is the issue prefix of the replay response written in the
	// transaction.
	prefix string
}

var errOperationKeyReused = errors.New("operation_key was already used for a different request or actor")

// errOperationReplay carries the receipt a retry found.
type errOperationReplay struct{ op db.IssueHandoffOperation }

func (errOperationReplay) Error() string { return "operation already applied" }

// handoffOperationHash binds an operation_key to one request: the whole body
// except the key itself. Every field the request can change is bound, so a
// retry with any field changed is refused instead of dropping the change.
func handoffOperationHash(req UpdateIssueRequest) string {
	req.OperationKey = nil
	body, _ := json.Marshal(req)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// newHandoffOperationSeed returns nil when the request has no key or is not a
// status move.
func newHandoffOperationSeed(w http.ResponseWriter, req UpdateIssueRequest, issue db.Issue, prefix, actorType, actorID string) (*handoffOperationSeed, bool) {
	if req.OperationKey == nil || req.Status == nil {
		return nil, true
	}
	key, ok := parseUUIDOrBadRequest(w, *req.OperationKey, "operation_key")
	if !ok {
		return nil, false
	}
	return &handoffOperationSeed{
		workspaceID: issue.WorkspaceID, issueID: issue.ID, key: key,
		actorType: actorType, actorID: optionalActorUUID(actorID), hash: handoffOperationHash(req), prefix: prefix,
	}, true
}

func (s *handoffOperationSeed) matches(op db.IssueHandoffOperation) bool {
	return op.RequestHash == s.hash && op.ActorType == s.actorType && op.ActorID == s.actorID
}

// claim inserts the receipt as the first statement of the status transaction.
// A key that already has one ends the transaction with the replay or the refusal.
func (s *handoffOperationSeed) claim(ctx context.Context, q *db.Queries) error {
	if s == nil {
		return nil
	}
	_, err := q.InsertHandoffOperation(ctx, db.InsertHandoffOperationParams{
		WorkspaceID: s.workspaceID, IssueID: s.issueID, OperationKey: s.key,
		ActorType: s.actorType, ActorID: s.actorID, RequestHash: s.hash,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("insert handoff operation: %w", err)
	}
	return s.existing(ctx, q)
}

// existing is the outcome for a key that has a receipt.
func (s *handoffOperationSeed) existing(ctx context.Context, q *db.Queries) error {
	op, err := q.GetHandoffOperation(ctx, db.GetHandoffOperationParams{IssueID: s.issueID, OperationKey: s.key})
	if err != nil {
		return fmt.Errorf("read handoff operation: %w", err)
	}
	if !s.matches(op) {
		return errOperationKeyReused
	}
	return errOperationReplay{op}
}

// claimHandoffOperation inserts the receipt of the status record in ctx, if any.
func claimHandoffOperation(ctx context.Context, q *db.Queries) error {
	if rec := statusRecordFrom(ctx); rec != nil {
		return rec.operation.claim(ctx, q)
	}
	return nil
}

// recordWrites names what the move wrote, in the same transaction as the receipt.
// It also stores the replayable response: the moved issue as the transaction
// leaves it, so a retry returns the first result even if the process stops
// after the commit.
func (s *handoffOperationSeed) recordWrites(ctx context.Context, q *db.Queries, rec *statusRecord, moved db.Issue) error {
	if s == nil {
		return nil
	}
	response, err := json.Marshal(issueToResponse(moved, s.prefix))
	if err != nil {
		return fmt.Errorf("marshal handoff operation response: %w", err)
	}
	params := db.RecordHandoffOperationWritesParams{IssueID: s.issueID, OperationKey: s.key, DispatchID: rec.intentID, Response: response}
	if rec.activity != nil {
		params.StatusHistoryID = rec.activity.ID
	}
	if rec.created != nil {
		params.CreatedCommentID = rec.created.ID
	}
	return q.RecordHandoffOperationWrites(ctx, params)
}

// storeHandoffOperationResponse replaces the in-transaction response with the
// full one the first request sent (step brief, hints). A failure is harmless:
// the receipt already holds the moved issue.
func (h *Handler) storeHandoffOperationResponse(ctx context.Context, s *handoffOperationSeed, resp any) {
	if s == nil {
		return
	}
	body, err := json.Marshal(resp)
	if err == nil {
		err = h.Queries.StoreHandoffOperationResponse(ctx, db.StoreHandoffOperationResponseParams{IssueID: s.issueID, OperationKey: s.key, Response: body})
	}
	if err != nil {
		slog.Warn("store handoff operation response", "issue_id", uuidToString(s.issueID), "error", err)
	}
}

// writeHandoffOperationError renders the refusal or the replay of a key's
// transaction outcome and reports whether it handled the error.
func (h *Handler) writeHandoffOperationError(ctx context.Context, w http.ResponseWriter, err error) bool {
	var replay errOperationReplay
	switch {
	case errors.Is(err, errOperationKeyReused):
		writeErrorCode(w, http.StatusConflict, "operation_key_reused", "operation_key was already used for a different request or actor")
	case errors.As(err, &replay):
		h.writeStoredHandoffResult(ctx, w, replay.op)
	default:
		return false
	}
	return true
}

// writeStoredHandoffResult sends the first response. The transaction stores it
// with the move, so it survives a stop after the commit. A custom status
// category is resolved now, as for any response. A receipt with no response
// (the write moved nothing) gets the issue as it is now.
func (h *Handler) writeStoredHandoffResult(ctx context.Context, w http.ResponseWriter, op db.IssueHandoffOperation) {
	if len(op.Response) > 0 {
		var stored IssueResponse
		if err := json.Unmarshal(op.Response, &stored); err == nil {
			h.fillStatusCategory(ctx, op.WorkspaceID, &stored)
			writeJSON(w, http.StatusOK, stored)
			return
		}
		slog.Warn("stored handoff operation response unreadable", "issue_id", uuidToString(op.IssueID))
	}
	issue, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: op.IssueID, WorkspaceID: op.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load issue")
		return
	}
	resp := issueToResponse(issue, h.getIssuePrefix(ctx, issue.WorkspaceID))
	h.fillStatusCategory(ctx, issue.WorkspaceID, &resp)
	writeJSON(w, http.StatusOK, resp)
}

// replayHandoffOperation answers a retry before any validation that reads the
// issue's current state: the first move changed it, so the same request would
// not pass those checks again. It reports whether it answered.
func (h *Handler) replayHandoffOperation(w http.ResponseWriter, r *http.Request, seed *handoffOperationSeed) bool {
	if seed == nil {
		return false
	}
	err := seed.existing(r.Context(), h.Queries)
	switch {
	case err == nil:
		return false
	case errors.Is(err, pgx.ErrNoRows):
		return false
	}
	if h.writeHandoffOperationError(r.Context(), w, err) {
		return true
	}
	slog.Error("read handoff operation", "issue_id", uuidToString(seed.issueID), "error", err)
	writeError(w, http.StatusInternalServerError, "failed to read operation receipt")
	return true
}

// pruneHandoffOperations deletes receipts past their 7 days.
func (h *Handler) pruneHandoffOperations(ctx context.Context, now time.Time) {
	if _, err := h.Queries.DeleteHandoffOperationsBefore(ctx, pgtype.Timestamptz{Time: now.Add(-handoffOperationTTL), Valid: true}); err != nil {
		slog.Warn("handoff operation prune failed", "error", err)
	}
}

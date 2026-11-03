package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// reconcileAskGuard is the ask guard pass of the decision worker. It writes one
// event for each unserved target of a sealed intent once the intent's run has
// ended (S3). A restart or a second pass finds the same rows: the unique index
// on (intent, member) makes the insert a no-op. A workspace with the guard off
// is skipped in the query, so it never holds up a batch.
func (h *Handler) reconcileAskGuard(ctx context.Context) error {
	targets, err := h.Queries.ListAskGuardIntentTargets(ctx, decisionBatchSize)
	if err != nil {
		return err
	}
	var failures error
	for _, t := range targets {
		sum := sha256.Sum256([]byte(uuidToString(t.IntentID) + ":" + uuidToString(t.HumanUserID)))
		_, err := h.Queries.InsertAskGuardIntentTargetEvent(ctx, db.InsertAskGuardIntentTargetEventParams{
			ID: dbid.NewV7(), WorkspaceID: t.WorkspaceID, IssueID: t.IssueID, TaskID: t.TaskID,
			PayloadSha256: hex.EncodeToString(sum[:]), Mode: t.Mode, IntentID: t.IntentID, MemberID: t.HumanUserID,
		})
		switch {
		case err == nil:
			h.Metrics.RecordAskGuardEvent(t.Mode, "created")
		case !errors.Is(err, pgx.ErrNoRows):
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// clearAskGuardEvent is the matching outcome of --recovers: the request clears
// the event only when the event is for this issue and the person the request
// asks. A request for A never clears B's event, and a request without recovers
// clears nothing. An event that is already cleared stays cleared. A request
// answers one event: the unique index on the clearing request refuses a retry
// that names a second one, even when two retries race.
func clearAskGuardEvent(ctx context.Context, queries *db.Queries, issue db.Issue, eventID, member, requestID pgtype.UUID) error {
	if !eventID.Valid {
		return nil
	}
	event, err := queries.GetAskGuardEventForMember(ctx, db.GetAskGuardEventForMemberParams{ID: eventID, WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, MemberID: member})
	if errors.Is(err, pgx.ErrNoRows) {
		return decisionIntentRefusal{http.StatusBadRequest, "recovers names no guard event for this issue and the person this request asks"}
	}
	if err != nil {
		return err
	}
	if event.ClearedByRequestID == requestID {
		return nil // a retry of the request that already cleared it
	}
	err = queries.ClearAskGuardEvent(ctx, db.ClearAskGuardEventParams{ID: eventID, ClearedByRequestID: requestID})
	var constraint *pgconn.PgError
	if errors.As(err, &constraint) && constraint.Code == "23505" {
		return decisionIntentRefusal{http.StatusBadRequest, "this request already recovers another guard event"}
	}
	return err
}

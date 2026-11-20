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
// that names a second one, even when two retries race. An event with no member
// is recovered only by the repair run its root admitted, named by taskID. A
// request whose event's run already asked the same person on the same issue is
// refused with the existing request's id.
func clearAskGuardEvent(ctx context.Context, queries *db.Queries, issue db.Issue, eventID, member, requestID, taskID pgtype.UUID) error {
	if !eventID.Valid {
		return nil
	}
	event, err := queries.GetAskGuardEventForMember(ctx, db.GetAskGuardEventForMemberParams{ID: eventID, WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, MemberID: member, TaskID: taskID})
	if errors.Is(err, pgx.ErrNoRows) {
		return decisionIntentRefusal{http.StatusBadRequest, "recovers names no guard event for this issue and the person this request asks"}
	}
	if err != nil {
		return err
	}
	if event.ClearedByRequestID == requestID {
		return nil // a retry of the request that already cleared it
	}
	// The run that left the event already asked this person on this issue: a
	// second card would repeat it (CHE-8752). The server names the first one.
	earlier, err := queries.GetEarlierRequestOfTaskForMember(ctx, db.GetEarlierRequestOfTaskForMemberParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, HumanUserID: member, TaskID: event.TaskID, ID: requestID})
	if err == nil {
		return decisionIntentRefusal{http.StatusConflict, "recovers names an event whose run already asked this person on this issue: decision request " + uuidToString(earlier) + " serves it; do not open another"}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	err = queries.ClearAskGuardEvent(ctx, db.ClearAskGuardEventParams{ID: eventID, ClearedByRequestID: requestID})
	var constraint *pgconn.PgError
	if errors.As(err, &constraint) && constraint.Code == "23505" {
		return decisionIntentRefusal{http.StatusBadRequest, "this request already recovers another guard event"}
	}
	return err
}

// serveAskGuardEvents is the other half of the same rule: a decision request from
// a run to a person on an issue serves that run's open events for that person
// and issue, so recovery sends no repair run and the person gets no second card.
func serveAskGuardEvents(ctx context.Context, queries *db.Queries, issue db.Issue, member, taskID pgtype.UUID) error {
	return queries.ServeAskGuardEventsForRequest(ctx, db.ServeAskGuardEventsForRequestParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaskID: taskID, MemberID: member})
}

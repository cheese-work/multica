package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// I2 timing (CHE-3369): a batch goes out 15 min after its oldest row (later
// rows never push it back); cards of one lane are 5 min apart; a root's
// immediate change may jump the batch wait once per 60 min.
const (
	progressBatchWindow  = 15 * time.Minute
	progressLaneGap      = 5 * time.Minute
	progressImmediateCap = 60 * time.Minute
	// Set while the lane's DM has no native, active, single-session route. The
	// successor router (C8) replaces this check; until then nothing is sent to
	// a session other than the lineage root.
	progressRoutePending = "route_pending"
)

// DMProgressDispatcher turns dm_progress_pending rows into progress cards
// (CHE-3369 I1.3-I1.6, I2, I3).
type DMProgressDispatcher struct {
	Pool    *pgxpool.Pool
	Queries *db.Queries
	Bus     *events.Bus
}

// Run sends every card due at now. A lane that fails is retried by the next
// run; the other lanes still go out.
func (d *DMProgressDispatcher) Run(ctx context.Context, now time.Time) error {
	lanes, err := d.Queries.ListDMProgressPendingLanes(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, lane := range lanes {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := d.dispatchLane(ctx, lane, now); err != nil {
			errs = append(errs, fmt.Errorf("lane %s: %w", util.UUIDToString(lane), err))
		}
	}
	return errors.Join(errs...)
}

// tracked is one issue with rows in the lane. deny is a reason code when the
// rows must not become a card.
type tracked struct {
	issue  db.Issue
	follow db.DmProgressFollow
	deny   string
	kinds  []string
	// immediate: a row of this issue may jump the batch wait now.
	immediate bool
}

// dispatchLane handles one lane in one transaction: the card, its delivery
// row, the lane and follow stamps and the deleted pending rows commit together
// or not at all (I1.3-I1.5). The websocket event follows the commit.
func (d *DMProgressDispatcher) dispatchLane(ctx context.Context, lineage pgtype.UUID, now time.Time) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := d.Queries.WithTx(tx)

	lane, err := q.LockDMProgressLaneSkipLocked(ctx, lineage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // another dispatcher holds the lane, or it is gone
	}
	if err != nil {
		return err
	}
	if lane.SuspendedReason.Valid && lane.SuspendedReason.String != progressRoutePending {
		return nil
	}
	// Route first, under a share lock held to commit: an archive or a
	// continuation cannot slip in between this read and the card, and a lane
	// with no route never loads its backlog.
	ok, err := d.routable(ctx, q, lane)
	if err != nil {
		return err
	}
	if !ok {
		if !lane.SuspendedReason.Valid {
			if err := q.SuspendDMProgressLane(ctx, db.SuspendDMProgressLaneParams{LineageID: lineage, SuspendedReason: pgtype.Text{String: progressRoutePending, Valid: true}}); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		return nil
	}
	rows, err := q.ListDMProgressPending(ctx, lineage)
	if err != nil {
		return err
	}

	// Rows of a denied issue are deleted, never replayed (I3.3); the log holds
	// IDs and a reason code only.
	byIssue := map[pgtype.UUID]*tracked{}
	var order []*tracked
	var kept, dropped []db.DmProgressPending
	for _, r := range rows {
		tr, ok := byIssue[r.IssueID]
		if !ok {
			if tr, err = trackIssue(ctx, q, lane, r.IssueID); err != nil {
				return err
			}
			byIssue[r.IssueID] = tr
			order = append(order, tr)
		}
		if tr.deny != "" {
			slog.Info("dm progress rows dropped", "lineage_id", util.UUIDToString(lineage), "issue_id", util.UUIDToString(r.IssueID), "reason", tr.deny)
			dropped = append(dropped, r)
			continue
		}
		kept = append(kept, r)
		if !slices.Contains(tr.kinds, r.EventKind) {
			tr.kinds = append(tr.kinds, r.EventKind)
		}
		// An immediate row jumps the batch wait unless the follow already spent
		// its allowance this hour; then it waits like any other row.
		if r.Immediate && (!tr.follow.LastImmediateAt.Valid || now.Sub(tr.follow.LastImmediateAt.Time) >= progressImmediateCap) {
			tr.immediate = true
		}
	}
	if err := deletePending(ctx, q, lineage, dropped); err != nil {
		return err
	}

	var msg *db.ChatMessage
	if sendAt, ok := dueAt(lane, kept, byIssue, now); ok && !now.Before(sendAt) {
		if msg, err = d.deliver(ctx, q, lane, kept, order, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if msg != nil {
		d.publish(lane, *msg)
	}
	return nil
}

// trackIssue loads an issue and its follow, and says why a card must not
// mention them: the follow is gone or muted, the issue is gone, or the owner
// can no longer read the issue.
func trackIssue(ctx context.Context, q *db.Queries, lane db.DmProgressLane, issueID pgtype.UUID) (*tracked, error) {
	tr := &tracked{}
	follow, err := q.GetActiveDMProgressFollow(ctx, db.GetActiveDMProgressFollowParams{IssueID: issueID, LineageID: lane.LineageID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		tr.deny = "not_following"
		return tr, nil
	case err != nil:
		return nil, err
	}
	tr.follow = follow
	if follow.MutedAt.Valid {
		tr.deny = "muted"
		return tr, nil
	}
	issue, err := q.GetIssue(ctx, issueID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		tr.deny = "issue_gone"
		return tr, nil
	case err != nil:
		return nil, err
	}
	tr.issue = issue
	if !ProgressVisible(ctx, q, lane.OwnerUserID, issue) {
		tr.deny = "not_visible"
	}
	return tr, nil
}

// dueAt is the time the next card may go out (I2): 15 min after the oldest
// kept row, or at the first row that may jump the wait, and never inside the
// lane's 5 min gap. ok is false when nothing is kept.
func dueAt(lane db.DmProgressLane, kept []db.DmProgressPending, byIssue map[pgtype.UUID]*tracked, now time.Time) (time.Time, bool) {
	if len(kept) == 0 {
		return time.Time{}, false
	}
	due := kept[0].CreatedAt.Time.Add(progressBatchWindow)
	for _, r := range kept {
		if r.Immediate && byIssue[r.IssueID].immediate && r.CreatedAt.Time.Before(due) {
			due = r.CreatedAt.Time
		}
	}
	if lane.LastCardAt.Valid {
		if gap := lane.LastCardAt.Time.Add(progressLaneGap); gap.After(due) {
			due = gap
		}
	}
	return due, true
}

// deliver writes the card for every kept row of the lane and clears them. It
// returns nil when nothing is left to say. The caller has checked the route.
func (d *DMProgressDispatcher) deliver(ctx context.Context, q *db.Queries, lane db.DmProgressLane, kept []db.DmProgressPending, order []*tracked, now time.Time) (*db.ChatMessage, error) {
	var payload protocol.ProgressCardPayload
	var sent []*tracked
	for _, tr := range order {
		if tr.deny != "" {
			continue
		}
		// The card shows the issue as it is now, against what the DM was last
		// told, so a late older event never overwrites a newer state. Status
		// events that net out to the same status say nothing.
		if tr.issue.Status == tr.follow.LastRenderedStatus {
			tr.kinds = slices.DeleteFunc(tr.kinds, func(k string) bool { return k == "status" })
		}
		if len(tr.kinds) == 0 {
			continue
		}
		payload.Items = append(payload.Items, protocol.ProgressCardItem{
			IssueID: util.UUIDToString(tr.issue.ID), Status: tr.issue.Status, FromStatus: tr.follow.LastRenderedStatus, Kinds: tr.kinds,
		})
		sent = append(sent, tr)
	}
	if err := deletePending(ctx, q, lane.LineageID, kept); err != nil {
		return nil, err
	}
	if len(sent) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	noun := "issues"
	if len(sent) == 1 {
		noun = "issue"
	}
	// A neutral insert: no task, no queue re-anchor (I5.1).
	msg, err := q.CreateChatMessage(ctx, db.CreateChatMessageParams{
		ID:            dbid.NewV7(),
		ChatSessionID: lane.LineageID,
		Role:          "assistant",
		Content:       fmt.Sprintf("Progress update: %d %s", len(sent), noun),
		MessageKind:   pgtype.Text{String: protocol.ChatMessageKindProgressCard, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	// next_seq moves only with this commit, so a retry reuses the same seq and
	// the (lineage, seq) key rejects a second card (I1.3).
	if err := q.InsertDMProgressDelivery(ctx, db.InsertDMProgressDeliveryParams{
		LineageID: lane.LineageID, Seq: lane.NextSeq, WorkspaceID: lane.WorkspaceID, ChatMessageID: msg.ID, Payload: body,
	}); err != nil {
		return nil, err
	}
	at := pgtype.Timestamptz{Time: now, Valid: true}
	if err := q.AdvanceDMProgressLane(ctx, db.AdvanceDMProgressLaneParams{LineageID: lane.LineageID, LastCardAt: at}); err != nil {
		return nil, err
	}
	for _, tr := range sent {
		if err := q.MarkDMProgressFollowRendered(ctx, db.MarkDMProgressFollowRenderedParams{
			IssueID: tr.issue.ID, LineageID: lane.LineageID, LastRenderedStatus: tr.issue.Status, Immediate: tr.immediate, At: at,
		}); err != nil {
			return nil, err
		}
	}
	return &msg, nil
}

// routable: the card goes only to the lineage root, and only while it is a
// native, active DM of the lane owner that nothing continues from. A moved DM
// (successor) is C8's routing, so it is held, never sent to an ancestor.
func (d *DMProgressDispatcher) routable(ctx context.Context, q *db.Queries, lane db.DmProgressLane) (bool, error) {
	session, err := q.LockNativeChatSessionShare(ctx, lane.LineageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return session.Status == "active" && session.CreatorID == lane.OwnerUserID && !session.ContinuedIntoSessionID.Valid, nil
}

func (d *DMProgressDispatcher) publish(lane db.DmProgressLane, msg db.ChatMessage) {
	if d.Bus == nil {
		return
	}
	d.Bus.Publish(events.Event{
		Type:               protocol.EventChatMessage,
		WorkspaceID:        util.UUIDToString(lane.WorkspaceID),
		ActorType:          "system",
		ChatSessionID:      util.UUIDToString(lane.LineageID),
		ChatSessionOwnerID: util.UUIDToString(lane.OwnerUserID),
		Payload: protocol.ChatMessagePayload{
			ChatSessionID: util.UUIDToString(msg.ChatSessionID),
			MessageID:     util.UUIDToString(msg.ID),
			Role:          msg.Role,
			Content:       msg.Content,
			CreatedAt:     msg.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
			MessageKind:   msg.MessageKind,
		},
	})
}

func deletePending(ctx context.Context, q *db.Queries, lineage pgtype.UUID, rows []db.DmProgressPending) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]pgtype.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.EventID
	}
	return q.DeleteDMProgressPendingEvents(ctx, db.DeleteDMProgressPendingEventsParams{LineageID: lineage, EventIds: ids})
}

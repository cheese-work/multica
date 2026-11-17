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
// immediate change may jump the batch wait once per 60 min (the 60 is in
// GetDMProgressLaneWindow and ListDMProgressLaneIssues).
const (
	progressBatchWindow = 15 * time.Minute
	progressLaneGap     = 5 * time.Minute
	// A card lists at most this many issues; the flush of the lane's rows is
	// never capped.
	progressCardMaxItems = 100
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

// dispatchLane handles one lane in one transaction: the card, its delivery
// row, the lane and follow stamps and the deleted pending rows commit together
// or not at all (I1.3-I1.5). The websocket event follows the commit.
//
// The work a pass does is bounded by what it may skip (CHE-3369 review of
// 27e44a0): a lane with no route does nothing, a lane not yet due does one
// aggregate over its pending rows, and a due lane reads its issues, access and
// follows in one statement however many issues it holds.
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

	// The route is read first, under a share lock held to commit: an archive or
	// a continuation cannot slip in between this read and the card. A lane with
	// no route keeps its rows untouched (C8 routes them) and is held; the denial
	// sweep runs when the route returns, before any card (I3.3).
	routed, err := d.routable(ctx, q, lane)
	if err != nil {
		return err
	}
	if !routed {
		if !lane.SuspendedReason.Valid {
			if err := q.SuspendDMProgressLane(ctx, db.SuspendDMProgressLaneParams{LineageID: lineage, SuspendedReason: pgtype.Text{String: progressRoutePending, Valid: true}}); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}

	at := pgtype.Timestamptz{Time: now, Valid: true}
	win, err := q.GetDMProgressLaneWindow(ctx, db.GetDMProgressLaneWindowParams{LineageID: lineage, Now: at})
	if err != nil {
		return err
	}
	if !win.OldestAt.Valid || now.Before(dueAt(lane, win.OldestAt, win.ImmediateAt)) {
		return nil
	}

	issues, err := q.ListDMProgressLaneIssues(ctx, db.ListDMProgressLaneIssuesParams{LineageID: lineage, Now: at})
	if err != nil {
		return err
	}
	// Rows of a denied issue are deleted, never replayed (I3.3); the log holds
	// IDs and a reason code only.
	var kept []db.ListDMProgressLaneIssuesRow
	var droppedIDs []pgtype.UUID
	for _, it := range issues {
		if it.DenyReason == "" {
			kept = append(kept, it)
			continue
		}
		slog.Info("dm progress rows dropped", "lineage_id", util.UUIDToString(lineage), "issue_id", util.UUIDToString(it.IssueID), "reason", it.DenyReason)
		droppedIDs = append(droppedIDs, it.EventIds...)
	}
	if err := deletePending(ctx, q, lineage, droppedIDs); err != nil {
		return err
	}

	var msg *db.ChatMessage
	if len(kept) > 0 {
		// kept is oldest first, so kept[0] holds the oldest row.
		immediate := pgtype.Timestamptz{}
		for _, it := range kept {
			if it.ImmediateAt.Valid && (!immediate.Valid || it.ImmediateAt.Time.Before(immediate.Time)) {
				immediate = it.ImmediateAt
			}
		}
		if !now.Before(dueAt(lane, kept[0].FirstAt, immediate)) {
			if msg, err = d.deliver(ctx, q, lane, kept, now); err != nil {
				return err
			}
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

// dueAt is the time the next card may go out (I2): 15 min after the oldest
// row, or at the first row that may jump the wait, and never inside the lane's
// 5 min gap.
func dueAt(lane db.DmProgressLane, oldest, immediate pgtype.Timestamptz) time.Time {
	due := oldest.Time.Add(progressBatchWindow)
	if immediate.Valid && immediate.Time.Before(due) {
		due = immediate.Time
	}
	if lane.LastCardAt.Valid {
		if gap := lane.LastCardAt.Time.Add(progressLaneGap); gap.After(due) {
			due = gap
		}
	}
	return due
}

// deliver writes one card for the kept issues and flushes every row it read
// (I2.4). The card lists at most progressCardMaxItems issues, oldest first; the
// cap shapes the card, never the flush. It returns nil when nothing is left to
// say. The caller has checked the route.
func (d *DMProgressDispatcher) deliver(ctx context.Context, q *db.Queries, lane db.DmProgressLane, kept []db.ListDMProgressLaneIssuesRow, now time.Time) (*db.ChatMessage, error) {
	var payload protocol.ProgressCardPayload
	var sent []db.ListDMProgressLaneIssuesRow
	var flushed []pgtype.UUID
	for _, it := range kept {
		flushed = append(flushed, it.EventIds...)
		// The card shows the issue as it is now, against what the DM was last
		// told, so a late older event never overwrites a newer state. Status
		// events that net out to the same status say nothing.
		kinds := it.Kinds
		if it.Status == it.LastRenderedStatus {
			kinds = slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return k == "status" })
		}
		if len(kinds) == 0 || len(sent) == progressCardMaxItems {
			continue
		}
		payload.Items = append(payload.Items, protocol.ProgressCardItem{
			IssueID: util.UUIDToString(it.IssueID), Status: it.Status, FromStatus: it.LastRenderedStatus, Kinds: kinds,
		})
		sent = append(sent, it)
	}
	if err := deletePending(ctx, q, lane.LineageID, flushed); err != nil {
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
	for _, it := range sent {
		if err := q.MarkDMProgressFollowRendered(ctx, db.MarkDMProgressFollowRenderedParams{
			IssueID: it.IssueID, LineageID: lane.LineageID, LastRenderedStatus: it.Status, Immediate: it.ImmediateAt.Valid, At: at,
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

func deletePending(ctx context.Context, q *db.Queries, lineage pgtype.UUID, eventIDs []pgtype.UUID) error {
	if len(eventIDs) == 0 {
		return nil
	}
	return q.DeleteDMProgressPendingEvents(ctx, db.DeleteDMProgressPendingEventsParams{LineageID: lineage, EventIds: eventIDs})
}

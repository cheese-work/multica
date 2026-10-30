package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Shadow capture of the events that start leader runs (CHE-1918, CHE-1917
// Slice 0a). See docs/run-gate-ledger.md for the table and the Slice 0b join.
//
// Capture only. Nothing here admits, folds, delays or skips a run, and a failed
// write is logged and dropped: the event it describes goes on exactly as before.

const (
	RunGateSourceComment        = "comment_leader"
	RunGateSourcePRMerged       = "wakeup_pr_merged"
	RunGateSourcePRChecksFailed = "wakeup_pr_checks_failed"
	RunGateOriginSystemWakeup   = "system_wakeup"
	runGateCaptureTimeout       = 2 * time.Second
)

// RunGateEvent is one input that may start a leader run. Comment text is never
// part of it.
type RunGateEvent struct {
	Issue         db.Issue
	Source        string
	EventKey      string
	CandidateKey  string
	Origin        string
	EventTime     time.Time
	PRHeadSHA     string
	AgentID       pgtype.UUID
	SquadID       pgtype.UUID
	AuthorType    string
	NonLeaderResp bool
	CommentID     pgtype.UUID
	SystemRule    string
	Wakeup        db.IssueWakeup
	ReceiptID     pgtype.UUID
}

// RunGateSourceForRule maps a PR system rule to its ledger source; ok is false
// for rules outside Slice 0a.
func RunGateSourceForRule(rule string) (string, bool) {
	switch rule {
	case SystemRulePRMerged:
		return RunGateSourcePRMerged, true
	case SystemRulePRChecksFailed:
		return RunGateSourcePRChecksFailed, true
	}
	return "", false
}

// CaptureRunGateEvent records ev with q's own handle. It never returns an error.
func CaptureRunGateEvent(ctx context.Context, q *db.Queries, ev RunGateEvent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runGateCaptureTimeout)
	defer cancel()
	if err := captureRunGate(ctx, q, ev); err != nil {
		logRunGateFailure(ev, err)
	}
}

// CaptureRunGateEventInTx records the event build returns inside the caller's
// transaction behind a savepoint, so a failed read or write rolls back to the
// savepoint and leaves the caller's own writes alone. build runs on the
// savepoint's handle. It never returns an error.
func CaptureRunGateEventInTx(ctx context.Context, tx pgx.Tx, q *db.Queries, build func(*db.Queries) RunGateEvent) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		slog.Warn("run gate capture failed; the event continues unchanged", "error", err)
		return
	}
	ev := build(q.WithTx(sp))
	if err = captureRunGate(ctx, q.WithTx(sp), ev); err == nil {
		err = sp.Commit(ctx)
	} else {
		_ = sp.Rollback(ctx)
	}
	if err != nil {
		logRunGateFailure(ev, err)
	}
}

// RecordRunGateAdmission copies the disposition a producer returned onto the
// event's row. It never returns an error.
func RecordRunGateAdmission(ctx context.Context, q *db.Queries, ev RunGateEvent, status, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runGateCaptureTimeout)
	defer cancel()
	if err := q.RecordRunGateAdmission(ctx, db.RecordRunGateAdmissionParams{
		AdmissionStatus: pgtype.Text{String: status, Valid: true}, AdmissionReason: pgtype.Text{String: reason, Valid: true},
		Source: ev.Source, IssueID: ev.Issue.ID, EventKey: ev.EventKey, CandidateKey: ev.CandidateKey,
	}); err != nil {
		logRunGateFailure(ev, err)
	}
}

func logRunGateFailure(ev RunGateEvent, err error) {
	slog.Warn("run gate capture failed; the event continues unchanged",
		"source", ev.Source, "issue_id", util.UUIDToString(ev.Issue.ID), "event_key", ev.EventKey, "error", err)
}

type runGateQueuedTask struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	IsLeaderTask     bool      `json:"is_leader_task"`
	SquadID          string    `json:"squad_id,omitempty"`
	HeadSHA          string    `json:"head_sha,omitempty"`
	TriggerCommentID string    `json:"trigger_comment_id,omitempty"`
	WakeupID         string    `json:"wakeup_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

func captureRunGate(ctx context.Context, q *db.Queries, ev RunGateEvent) error {
	snapshot := []runGateQueuedTask{}
	var active, fold pgtype.UUID
	if ev.AgentID.Valid {
		rows, err := q.ListRunGateQueue(ctx, db.ListRunGateQueueParams{IssueID: ev.Issue.ID, AgentID: ev.AgentID})
		if err != nil {
			return err
		}
		for _, r := range rows {
			snapshot = append(snapshot, runGateQueuedTask{
				ID: util.UUIDToString(r.ID), Status: r.Status, IsLeaderTask: r.IsLeaderTask, SquadID: uuidOrEmpty(r.SquadID),
				HeadSHA: r.HeadSha, TriggerCommentID: uuidOrEmpty(r.TriggerCommentID), WakeupID: r.WakeupID, CreatedAt: r.CreatedAt.Time,
			})
			if !active.Valid && (r.Status == "running" || r.Status == "dispatched" || r.Status == "waiting_local_directory") {
				active = r.ID
			}
		}
		if ev.Source == RunGateSourceComment {
			fold, err = q.FindRunGateCommentFoldTarget(ctx, db.FindRunGateCommentFoldTargetParams{
				IssueID: ev.Issue.ID, AgentID: ev.AgentID, SquadID: ev.SquadID, HeadSha: headShaText(ev.PRHeadSHA),
			})
		} else {
			fold, err = q.FindRunGateWakeupFoldTarget(ctx, util.UUIDToString(ev.Wakeup.ID))
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = q.UpsertRunGateEvent(ctx, db.UpsertRunGateEventParams{
		ID: dbid.NewV7(), WorkspaceID: ev.Issue.WorkspaceID, IssueID: ev.Issue.ID, Source: ev.Source,
		EventKey: ev.EventKey, CandidateKey: ev.CandidateKey, Origin: ev.Origin,
		AuthorType:          pgtype.Text{String: ev.AuthorType, Valid: ev.AuthorType != ""},
		NonLeaderAgentReply: ev.NonLeaderResp, TriggerCommentID: ev.CommentID,
		SystemRule:     pgtype.Text{String: ev.SystemRule, Valid: ev.SystemRule != ""},
		WakeupID:       ev.Wakeup.ID,
		WakeupRevision: pgtype.Int8{Int64: ev.Wakeup.Revision, Valid: ev.Wakeup.ID.Valid},
		ReceiptID:      ev.ReceiptID, AgentID: ev.AgentID, SquadID: ev.SquadID,
		IssueRevision: ev.Issue.Revision, IssueStatus: ev.Issue.Status, PrHeadSha: headShaText(ev.PRHeadSHA),
		EventTime:          pgtype.Timestamptz{Time: ev.EventTime, Valid: true},
		NativeFoldEligible: fold.Valid, NativeFoldTaskID: fold, QueueSnapshot: encoded, ActiveTaskID: active,
	})
	return err
}

func uuidOrEmpty(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return util.UUIDToString(id)
}

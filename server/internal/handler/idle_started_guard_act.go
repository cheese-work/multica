package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	idleGuardNotifyAttempts = 3

	idleGuardMoved    = "moved"
	idleGuardNotified = "notified"
	idleGuardGaveUp   = "gave_up"

	idleGuardKindHumanWait      = "human_wait"
	idleGuardKindNoContinuation = "no_continuation"
)

// idleGuardReceipt is the issue.metadata.idle_guard episode record. It is the
// once-per-episode mark: the main scan skips an issue that carries one, and the
// episode pass removes it when the issue leaves the guard's target status.
type idleGuardReceipt struct {
	State     string `json:"state"`
	Kind      string `json:"kind"`
	From      string `json:"from"`
	To        string `json:"to"`
	IdleSince string `json:"idle_since"`
	MovedAt   string `json:"moved_at"`
	Attempts  int    `json:"attempts"`
	AlertID   string `json:"alert_id,omitempty"`
}

// issueMoveGuard runs inside a status write's transaction, after the issue row
// is locked and before the write. A non-nil error stops the write.
type issueMoveGuard func(ctx context.Context, q *db.Queries, params *db.UpdateIssueParams) error

type issueMoveGuardKey struct{}

// withIssueMoveGuard arms guard for the next status write on ctx; it runs once.
func withIssueMoveGuard(ctx context.Context, guard issueMoveGuard) context.Context {
	var used atomic.Bool
	once := issueMoveGuard(func(ctx context.Context, q *db.Queries, params *db.UpdateIssueParams) error {
		if !used.CompareAndSwap(false, true) {
			return nil
		}
		return guard(ctx, q, params)
	})
	return context.WithValue(ctx, issueMoveGuardKey{}, once)
}

func issueMoveGuardFrom(ctx context.Context) issueMoveGuard {
	guard, _ := ctx.Value(issueMoveGuardKey{}).(issueMoveGuard)
	return guard
}

// actOnIdleFinding moves one idle issue and alerts its leader. The move
// re-checks idleness under the issue lock, in the same transaction as the status
// write, so a run or wakeup that landed after the scan keeps the issue where it is.
func (handler *Handler) actOnIdleFinding(ctx context.Context, settings idleGuardSettings, finding idleGuardFinding) error {
	issue, err := handler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: finding.WorkspaceID, ID: finding.IssueID})
	if err != nil {
		return err
	}
	waiting, err := handler.Queries.IdleStartedGuardHumanWait(ctx, issue.ID)
	if err != nil {
		return err
	}
	receipt := idleGuardReceipt{State: idleGuardMoved, Kind: idleGuardKindNoContinuation, From: issue.Status, To: issuestatus.Blocked,
		IdleSince: finding.IdleSince.UTC().Format(time.RFC3339), MovedAt: time.Now().UTC().Format(time.RFC3339)}
	if waiting {
		receipt.Kind, receipt.To = idleGuardKindHumanWait, "need_decision"
	}
	guard := func(ctx context.Context, q *db.Queries, params *db.UpdateIssueParams) error {
		if _, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID}); err != nil {
			return err
		}
		still, err := q.ListIdleStartedGuardIssues(ctx, db.ListIdleStartedGuardIssuesParams{
			WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, Statuses: settings.statuses(), AfterSeconds: settings.threshold().Seconds(),
		})
		if err != nil {
			return err
		}
		if len(still) == 0 {
			return fmt.Errorf("idle guard: issue is no longer idle: %w", pgx.ErrNoRows)
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		written, err := q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: "idle_guard", Value: raw})
		if err != nil {
			return err
		}
		if receipt.To == issuestatus.Blocked {
			reason, _ := json.Marshal("idle-no-continuation")
			if written, err = q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: "blocked_reason", Value: reason}); err != nil {
				return err
			}
		}
		params.ExpectedRevision = pgtype.Int8{Int64: written.Revision, Valid: true}
		return nil
	}
	extra := map[string]any{"kind": receipt.Kind, "idle_since": receipt.IdleSince}
	if moved, _ := handler.moveIssueOnEdge(withIssueMoveGuard(ctx, guard), issue, receipt.To, "idle_started_guard", extra, false); !moved {
		return nil
	}
	slog.Info("idle_started_guard moved", "issue_id", uuidToString(issue.ID), "from", receipt.From, "to", receipt.To, "kind", receipt.Kind)
	return handler.notifyIdleGuardLeader(ctx, issue.WorkspaceID, issue.ID)
}

// notifyIdleGuardLeader posts one system comment and admits one leader task,
// together with the receipt that says so. A failure counts an attempt; the
// third gives up.
func (handler *Handler) notifyIdleGuardLeader(ctx context.Context, workspaceID, issueID pgtype.UUID) error {
	task, comment, receipt, err := handler.admitIdleGuardNotice(ctx, workspaceID, issueID)
	if err != nil {
		return errors.Join(err, handler.recordIdleGuardNoticeFailure(ctx, workspaceID, issueID))
	}
	if comment.ID.Valid {
		if issue, err := handler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: issueID}); err == nil {
			handler.publish(protocol.EventCommentCreated, uuidToString(workspaceID), "system", "", map[string]any{
				"comment": commentToResponse(comment, nil, nil), "issue_title": issue.Title, "issue_assignee_type": textToPtr(issue.AssigneeType),
				"issue_assignee_id": uuidToPtr(issue.AssigneeID), "issue_status": issue.Status, "issue_revision": issue.Revision,
			})
		}
	}
	if task.ID.Valid {
		handler.TaskService.PublishTaskEnqueued(ctx, task)
		slog.Info("idle_started_guard notified", "issue_id", uuidToString(issueID), "alert_id", receipt.AlertID)
	}
	return nil
}

func (handler *Handler) admitIdleGuardNotice(ctx context.Context, workspaceID, issueID pgtype.UUID) (db.AgentTaskQueue, db.Comment, idleGuardReceipt, error) {
	var none db.AgentTaskQueue
	tx, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return none, db.Comment{}, idleGuardReceipt{}, err
	}
	defer tx.Rollback(ctx)
	q := handler.Queries.WithTx(tx)
	if _, err := q.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return none, db.Comment{}, idleGuardReceipt{}, err
	}
	if _, err := q.LockDecisionIssueNowait(ctx, db.LockDecisionIssueNowaitParams{WorkspaceID: workspaceID, ID: issueID}); err != nil {
		return none, db.Comment{}, idleGuardReceipt{}, err
	}
	issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: issueID})
	if err != nil {
		return none, db.Comment{}, idleGuardReceipt{}, err
	}
	receipt, ok := readIdleGuardReceipt(issue)
	if !ok || receipt.State != idleGuardMoved || issue.Status != receipt.To {
		return none, db.Comment{}, receipt, nil
	}
	squad, err := q.GetDecisionWaitGuardSquad(ctx, db.GetDecisionWaitGuardSquadParams{WorkspaceID: workspaceID, IssueID: issueID})
	if err != nil {
		return none, db.Comment{}, receipt, fmt.Errorf("idle guard: owning squad: %w", err)
	}
	leader, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: workspaceID, ID: squad.LeaderID})
	if err != nil {
		return none, db.Comment{}, receipt, fmt.Errorf("idle guard: squad leader: %w", err)
	}
	if leader.ArchivedAt.Valid {
		return none, db.Comment{}, receipt, errors.New("idle guard: squad leader is archived")
	}
	note := fmt.Sprintf("Idle started-issue guard: issue %s was idle in %s since %s with no run, wakeup or sub-issue left to continue it, so it moved to %s (%s). Decide whether to continue it, re-route it, or open a decision request if a person must choose. This alert grants no new decision, merge or deploy authority.",
		uuidToString(issueID), receipt.From, receipt.IdleSince, receipt.To, receipt.Kind)
	task, err := handler.TaskService.EnqueueGuardLeaderTaskInTx(ctx, q, issue, leader.ID, squad.ID, note, "idle started guard", "Idle started issue moved")
	if err != nil {
		return none, db.Comment{}, receipt, err
	}
	created, err := q.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issueID, WorkspaceID: workspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
		Content: fmt.Sprintf("Idle started-issue guard: no run, wakeup or sub-issue was left to continue this issue and it had been idle since %s. Moved from %s to %s (%s). The squad leader has been alerted.", receipt.IdleSince, receipt.From, receipt.To, receipt.Kind),
		Type:    "system",
	})
	if err != nil {
		return none, db.Comment{}, receipt, err
	}
	receipt.State, receipt.AlertID = idleGuardNotified, util.UUIDToString(task.ID)
	if err := writeIdleGuardReceipt(ctx, q, issue, receipt); err != nil {
		return none, db.Comment{}, receipt, err
	}
	if receipt.To == "need_decision" {
		// The decision wait guard would alert the same leader ten minutes later.
		raw, _ := json.Marshal(receipt.AlertID)
		if _, err := q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: workspaceID, ID: issueID, Key: "decision_wait_alert_id", Value: raw}); err != nil {
			return none, db.Comment{}, receipt, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return none, db.Comment{}, receipt, err
	}
	return task, created.Comment(), receipt, nil
}

// recordIdleGuardNoticeFailure counts a failed notice; the third gives up so a
// permanently unroutable issue stops being retried every pass.
func (handler *Handler) recordIdleGuardNoticeFailure(ctx context.Context, workspaceID, issueID pgtype.UUID) error {
	issue, err := handler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: issueID})
	if err != nil {
		return err
	}
	receipt, ok := readIdleGuardReceipt(issue)
	if !ok || receipt.State != idleGuardMoved {
		return nil
	}
	receipt.Attempts++
	if receipt.Attempts >= idleGuardNotifyAttempts {
		receipt.State = idleGuardGaveUp
		slog.Warn("idle_started_guard gave up notifying", "issue_id", uuidToString(issueID))
	}
	return writeIdleGuardReceipt(ctx, handler.Queries, issue, receipt)
}

// settleIdleGuardEpisodes retries notices still owed and ends episodes whose
// issue left the guard's target status, so a repaired issue can be caught again.
func (handler *Handler) settleIdleGuardEpisodes(ctx context.Context) error {
	workspaces, err := handler.Queries.ListIdleStartedGuardWorkspaces(ctx)
	if err != nil {
		return err
	}
	var failures error
	for _, workspace := range workspaces {
		var settings idleGuardSettings
		if err := json.Unmarshal(workspace.Settings, &settings); err != nil || settings.Guard.Mode != "act" {
			continue
		}
		episodes, err := handler.Queries.ListIdleStartedGuardEpisodes(ctx, db.ListIdleStartedGuardEpisodesParams{WorkspaceID: workspace.ID, Statuses: settings.statuses()})
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		for _, episode := range episodes {
			var receipt idleGuardReceipt
			if err := json.Unmarshal(episode.Receipt, &receipt); err != nil {
				failures = errors.Join(failures, err)
				continue
			}
			if episode.Status == receipt.To {
				failures = errors.Join(failures, handler.notifyIdleGuardLeader(ctx, episode.WorkspaceID, episode.ID))
				continue
			}
			_, err := handler.Queries.DeleteIssueMetadataKey(ctx, db.DeleteIssueMetadataKeyParams{WorkspaceID: episode.WorkspaceID, ID: episode.ID, Key: "idle_guard"})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				failures = errors.Join(failures, err)
			}
		}
	}
	return failures
}

func readIdleGuardReceipt(issue db.Issue) (idleGuardReceipt, bool) {
	var metadata map[string]json.RawMessage
	var receipt idleGuardReceipt
	if json.Unmarshal(issue.Metadata, &metadata) != nil || json.Unmarshal(metadata["idle_guard"], &receipt) != nil {
		return receipt, false
	}
	return receipt, true
}

func writeIdleGuardReceipt(ctx context.Context, q *db.Queries, issue db.Issue, receipt idleGuardReceipt) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: "idle_guard", Value: raw})
	return err
}

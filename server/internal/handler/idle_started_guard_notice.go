package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const idleGuardErrorLimit = 300

func truncateIdleGuardError(err error) string {
	text := err.Error()
	if len(text) > idleGuardErrorLimit {
		return text[:idleGuardErrorLimit]
	}
	return text
}

// idleGuardNotice is what a committed notice transaction leaves to publish.
type idleGuardNotice struct {
	task    db.AgentTaskQueue
	comment db.Comment
}

// noticeIdleGuardLeader tells the owning squad leader about a moved issue, once
// per episode. The status explanation already exists (the move wrote it), so a
// failure here counts an attempt and keeps the explanation.
func (handler *Handler) noticeIdleGuardLeader(ctx context.Context, settings idleGuardSettings, workspaceID, issueID pgtype.UUID) error {
	notice, err := handler.admitIdleGuardNotice(ctx, settings, workspaceID, issueID)
	handler.publishIdleGuardComment(ctx, workspaceID, issueID, notice.comment)
	if notice.task.ID.Valid {
		handler.TaskService.PublishTaskEnqueued(ctx, notice.task)
		slog.Info("idle_started_guard notified", "issue_id", uuidToString(issueID), "alert_id", uuidToString(notice.task.ID))
	}
	return err
}

// admitIdleGuardNotice decides under the issue lock, in order: a decision wait
// alert already sent is reused; an issue that has resumed gets no alert; an
// open decision request is already the person's to answer. Only then does it
// route a leader task, inside a savepoint so a routing failure keeps the
// attempt count and the receipt.
func (handler *Handler) admitIdleGuardNotice(ctx context.Context, settings idleGuardSettings, workspaceID, issueID pgtype.UUID) (idleGuardNotice, error) {
	var none idleGuardNotice
	tx, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return none, err
	}
	defer tx.Rollback(ctx)
	q := handler.Queries.WithTx(tx)
	if _, err := q.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return none, err
	}
	if _, err := q.LockDecisionIssueNowait(ctx, db.LockDecisionIssueNowaitParams{WorkspaceID: workspaceID, ID: issueID}); err != nil {
		return none, err
	}
	issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: issueID})
	if err != nil {
		return none, err
	}
	receipt, ok := readIdleGuardReceipt(issue)
	if !ok || receipt.State != idleGuardMoved || issue.Status != receipt.To {
		return none, nil
	}
	settle := func(state string) (idleGuardNotice, error) {
		receipt.State = state
		if _, err := writeIdleGuardReceipt(ctx, q, issue, receipt); err != nil {
			return none, err
		}
		return none, tx.Commit(ctx)
	}
	if alert := issueMetadataString(issue, "decision_wait_alert_id"); alert != "" && receipt.To == idleGuardNeedDecision {
		receipt.AlertID = alert
		return settle(idleGuardNotified)
	}
	still, err := q.ListIdleStartedGuardIssues(ctx, db.ListIdleStartedGuardIssuesParams{
		WorkspaceID: workspaceID, IssueID: issueID, Statuses: []string{receipt.To}, AfterSeconds: settings.threshold().Seconds(),
		Episode: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		return none, err
	}
	if len(still) == 0 {
		return settle(idleGuardResumed)
	}
	requests, err := q.ListDecisionRequestsByIssue(ctx, db.ListDecisionRequestsByIssueParams{WorkspaceID: workspaceID, IssueID: issueID})
	if err != nil {
		return none, err
	}
	for _, request := range requests {
		if request.Status == "open" {
			return settle(idleGuardNotified)
		}
	}

	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return none, err
	}
	task, routeErr := handler.admitIdleGuardLeaderTask(ctx, handler.Queries.WithTx(savepoint), issue, idleGuardLeaderNote(issue, receipt))
	if routeErr != nil {
		if err := savepoint.Rollback(ctx); err != nil {
			return none, err
		}
		var notice idleGuardNotice
		receipt.Attempts++
		receipt.Error = truncateIdleGuardError(routeErr)
		if receipt.Attempts >= idleGuardAttempts {
			receipt.State = idleGuardGaveUp
			slog.Warn("idle_started_guard gave up notifying", "issue_id", uuidToString(issueID), "err", routeErr)
			created, err := q.CreateComment(ctx, db.CreateCommentParams{
				ID: dbid.NewV7(), IssueID: issueID, WorkspaceID: workspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
				Content: fmt.Sprintf("Idle started-issue guard: moved this issue to %s but could not alert its squad leader after %d attempts. Last error: %s", receipt.To, receipt.Attempts, receipt.Error),
				Type:    "system",
			})
			if err != nil {
				return none, err
			}
			notice.comment = created.Comment()
		}
		if _, err := writeIdleGuardReceipt(ctx, q, issue, receipt); err != nil {
			return none, err
		}
		if err := tx.Commit(ctx); err != nil {
			return none, err
		}
		return notice, routeErr
	}
	if err := savepoint.Commit(ctx); err != nil {
		return none, err
	}
	receipt.State, receipt.AlertID, receipt.Error = idleGuardNotified, util.UUIDToString(task.ID), ""
	if _, err := writeIdleGuardReceipt(ctx, q, issue, receipt); err != nil {
		return none, err
	}
	if receipt.To == idleGuardNeedDecision {
		// The decision wait guard would alert the same leader ten minutes later.
		raw, _ := json.Marshal(receipt.AlertID)
		if _, err := q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: workspaceID, ID: issueID, Key: "decision_wait_alert_id", Value: raw}); err != nil {
			return none, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return none, err
	}
	return idleGuardNotice{task: task}, nil
}

func idleGuardLeaderNote(issue db.Issue, receipt idleGuardReceipt) string {
	return fmt.Sprintf("Idle started-issue guard: issue %s was idle in %s since %s with no run, wakeup or sub-issue left to continue it, so it moved to %s (%s). Decide whether to continue it, re-route it, or open a decision request if a person must choose. This alert grants no new decision, merge or deploy authority.",
		uuidToString(issue.ID), receipt.From, idleGuardWhen(receipt.IdleSince), receipt.To, receipt.Kind)
}

// admitIdleGuardLeaderTask routes the issue's owning squad and queues one task
// for its leader.
func (handler *Handler) admitIdleGuardLeaderTask(ctx context.Context, q *db.Queries, issue db.Issue, note string) (db.AgentTaskQueue, error) {
	squad, err := q.GetDecisionWaitGuardSquad(ctx, db.GetDecisionWaitGuardSquadParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("idle guard: owning squad: %w", err)
	}
	leader, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: squad.LeaderID})
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("idle guard: squad leader: %w", err)
	}
	if leader.ArchivedAt.Valid {
		return db.AgentTaskQueue{}, errors.New("idle guard: squad leader is archived")
	}
	return handler.TaskService.EnqueueGuardLeaderTaskInTx(ctx, q, issue, leader.ID, squad.ID, note, "idle started guard", "Idle started issue moved")
}

func (handler *Handler) publishIdleGuardComment(ctx context.Context, workspaceID, issueID pgtype.UUID, comment db.Comment) {
	if !comment.ID.Valid {
		return
	}
	issue, err := handler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: issueID})
	if err != nil {
		return
	}
	handler.publish(protocol.EventCommentCreated, uuidToString(workspaceID), "system", "", map[string]any{
		"comment": commentToResponse(comment, nil, nil), "issue_title": issue.Title, "issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id": uuidToPtr(issue.AssigneeID), "issue_status": issue.Status, "issue_revision": issue.Revision,
	})
}

// settleIdleGuardEpisodes carries earlier episodes forward: a notice still owed,
// a refused move to retry, and the end of an episode whose issue left it, so a
// repaired issue can be caught again.
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
		episodes, err := handler.Queries.ListIdleStartedGuardEpisodes(ctx, workspace.ID)
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
			switch {
			case receipt.State == idleGuardMoved && episode.Status == receipt.To:
				failures = errors.Join(failures, handler.noticeIdleGuardLeader(ctx, settings, episode.WorkspaceID, episode.ID))
			case receipt.State == idleGuardRefused && episode.Status == receipt.From:
				failures = errors.Join(failures, handler.retryIdleGuardMove(ctx, settings, episode.WorkspaceID, episode.ID, receipt))
			default:
				failures = errors.Join(failures, handler.endIdleGuardEpisode(ctx, episode.WorkspaceID, episode.ID))
			}
		}
	}
	return failures
}

// retryIdleGuardMove repeats a refused move while the issue is still idle.
func (handler *Handler) retryIdleGuardMove(ctx context.Context, settings idleGuardSettings, workspaceID, issueID pgtype.UUID, receipt idleGuardReceipt) error {
	idle, err := handler.Queries.ListIdleStartedGuardIssues(ctx, db.ListIdleStartedGuardIssuesParams{
		WorkspaceID: workspaceID, IssueID: issueID, Statuses: []string{receipt.From}, AfterSeconds: settings.threshold().Seconds(),
		Episode: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		return err
	}
	if len(idle) == 0 {
		return handler.endIdleGuardEpisode(ctx, workspaceID, issueID)
	}
	return handler.actOnIdleFinding(ctx, settings, idleGuardFinding{WorkspaceID: workspaceID, IssueID: issueID, Status: idle[0].Status, IdleSince: idle[0].IdleSince.Time, settings: settings})
}

func (handler *Handler) endIdleGuardEpisode(ctx context.Context, workspaceID, issueID pgtype.UUID) error {
	_, err := handler.Queries.DeleteIssueMetadataKey(ctx, db.DeleteIssueMetadataKeyParams{WorkspaceID: workspaceID, ID: issueID, Key: "idle_guard"})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

func readIdleGuardReceipt(issue db.Issue) (idleGuardReceipt, bool) {
	var metadata map[string]json.RawMessage
	var receipt idleGuardReceipt
	if json.Unmarshal(issue.Metadata, &metadata) != nil || json.Unmarshal(metadata["idle_guard"], &receipt) != nil {
		return receipt, false
	}
	return receipt, true
}

func issueMetadataString(issue db.Issue, key string) string {
	var metadata map[string]json.RawMessage
	var value string
	if json.Unmarshal(issue.Metadata, &metadata) != nil || json.Unmarshal(metadata[key], &value) != nil {
		return ""
	}
	return value
}

func writeIdleGuardReceipt(ctx context.Context, q *db.Queries, issue db.Issue, receipt idleGuardReceipt) (db.SetIssueMetadataKeyRow, error) {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return db.SetIssueMetadataKeyRow{}, err
	}
	return q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: "idle_guard", Value: raw})
}

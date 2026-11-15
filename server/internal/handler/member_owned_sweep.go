package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// SweepMemberOwnedIssues is the scheduler entry point (CHE-5838, fix 3 of
// CHE-5516). Every open issue assigned to a member with no open decision card
// goes back to the last agent that ran on it, with a run and one system note.
// An issue the sweep cannot return (a paused agent, an exhausted run budget)
// stays with the member and is tried again on the next pass.
func (handler *Handler) SweepMemberOwnedIssues(ctx context.Context) error {
	rows, err := handler.Queries.ListMemberOwnedSweepIssues(ctx, pgtype.UUID{})
	if err != nil {
		return err
	}
	var failures error
	for _, row := range rows {
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		if err := handler.returnMemberOwnedIssue(ctx, row.WorkspaceID, row.ID); err != nil {
			slog.Warn("member_owned_sweep: issue not returned", "issue_id", uuidToString(row.ID), "err", err)
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// returnMemberOwnedIssue re-checks the issue under its row lock, then reassigns
// it, enqueues the agent's run and writes the note in one transaction.
func (handler *Handler) returnMemberOwnedIssue(ctx context.Context, workspaceID, issueID pgtype.UUID) error {
	var (
		updated      db.Issue
		prev         db.Issue
		comment      db.Comment
		runAfter     func()
		cancelledRun []db.AgentTaskQueue
	)
	// The Composio overlay reads the pool and calls out, so it is built before
	// the transaction opens, for the agent the sweep is about to return the issue
	// to. The recheck under the lock decides whether the run is inserted at all.
	overlay := handler.prepareMemberOwnedOverlay(ctx, issueID)
	err := handler.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := handler.Queries.WithTx(tx)
		var err error
		if prev, err = q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issueID, WorkspaceID: workspaceID}); err != nil {
			return err
		}
		still, err := q.ListMemberOwnedSweepIssues(ctx, issueID)
		if err != nil || len(still) == 0 {
			return err
		}
		agent, err := q.GetAgent(ctx, still[0].AgentID)
		if err != nil {
			return err
		}
		updated, cancelledRun, err = updateIssueStoppingWakeups(ctx, q, db.UpdateIssueParams{
			ID: prev.ID, ExpectedRevision: pgtype.Int8{Int64: prev.Revision, Valid: true},
			AssigneeType: pgtype.Text{String: "agent", Valid: true}, AssigneeID: agent.ID,
			StartDate: prev.StartDate, DueDate: prev.DueDate, ParentIssueID: prev.ParentIssueID, ProjectID: prev.ProjectID, Stage: prev.Stage,
		})
		if err != nil {
			return err
		}
		created, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: updated.ID, WorkspaceID: updated.WorkspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
			Content: fmt.Sprintf("Member-owned sweep: no open decision card, so this issue went back to %s and the agent was woken.", agent.Name),
			Type:    "system",
		})
		if err != nil {
			return err
		}
		comment = created.Comment()
		// TxScoped: reads inside the enqueue go through q, not a second pool connection.
		_, runAfter, err = handler.TaskService.TxScoped(q).EnqueueIssueRunInTx(ctx, q, updated, pgtype.UUID{}, pgtype.UUID{}, overlay)
		if errors.Is(err, service.ErrDuplicatePendingTask) {
			return nil
		}
		return err
	})
	if err != nil || !updated.ID.Valid {
		return err
	}
	handler.broadcastCancelledWakeups(ctx, updated.WorkspaceID, cancelledRun)
	handler.publish(protocol.EventIssueUpdated, uuidToString(updated.WorkspaceID), "system", "", map[string]any{
		"issue": handler.issueResponseWithCategory(ctx, updated), "assignee_changed": true, "status_changed": false,
		"prev_assignee_type": textToPtr(prev.AssigneeType), "prev_assignee_id": uuidToPtr(prev.AssigneeID), "source": "member_owned_sweep",
	})
	handler.publishIdleGuardComment(ctx, updated.WorkspaceID, updated.ID, comment)
	if runAfter != nil {
		runAfter()
	}
	slog.Info("member_owned_sweep returned", "issue_id", uuidToString(updated.ID), "agent_id", uuidToString(updated.AssigneeID))
	return nil
}

// prepareMemberOwnedOverlay builds the run overlay for the issue's last agent
// outside any transaction. A missing candidate or issue gives no overlay; the
// transaction's recheck then skips or fails the issue on its own.
func (handler *Handler) prepareMemberOwnedOverlay(ctx context.Context, issueID pgtype.UUID) *service.PreparedRuntimeMCPOverlay {
	candidate, err := handler.Queries.ListMemberOwnedSweepIssues(ctx, issueID)
	if err != nil || len(candidate) == 0 {
		return nil
	}
	issue, err := handler.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return nil
	}
	issue.AssigneeType = pgtype.Text{String: "agent", Valid: true}
	issue.AssigneeID = candidate[0].AgentID
	return handler.TaskService.PrepareIssueRunOverlay(ctx, issue, pgtype.UUID{})
}

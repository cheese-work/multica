package handler

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// handoffThreadCap is the thread size at which a status-handoff run's comment
// starts a new thread instead of joining the handoff thread.
const handoffThreadCap = 30

// statusHandoffDerivesParent reports whether the server derives the parent of
// this request: an agent-authored plain comment with no explicit parent, posted
// by a run that a status move started with a handoff comment (CHE-6120). An
// explicit parent keeps the trigger-parent guard, and so do member comments,
// comment-triggered runs, decision, notify-only and supersede requests.
func statusHandoffDerivesParent(task db.AgentTaskQueue, req CreateCommentRequest) bool {
	return task.TriggerKind.Valid && task.TriggerKind.String == "status_handoff" &&
		task.TriggerCommentID.Valid &&
		req.ParentID == nil && req.Type == "comment" &&
		!req.DecisionIntent && !req.DecisionRecord && !req.DecisionVeto &&
		!req.NotifyOnly && len(req.Supersedes) == 0
}

// deriveStatusHandoffParent returns the thread root of the task's handoff
// comment, or an invalid UUID (post at top level) when that thread already
// holds handoffThreadCap comments or the handoff comment is gone. Derived
// parents never feed trigger routing: the caller routes the comment as a
// top-level one.
func deriveStatusHandoffParent(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, issue db.Issue) (pgtype.UUID, error) {
	root, err := q.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: task.TriggerCommentID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	if root.IssueID != issue.ID {
		return pgtype.UUID{}, nil
	}
	replies, err := q.CountThreadCommentsSince(ctx, db.CountThreadCommentsSinceParams{
		TargetID:    root.ID,
		Since:       pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	if replies+1 >= handoffThreadCap {
		return pgtype.UUID{}, nil
	}
	return root.ID, nil
}

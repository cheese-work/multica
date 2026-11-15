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
// by a run that is either started by a status move with a handoff comment
// (CHE-6120) or has no trigger comment, where the handoff chain decides
// (CHE-6121). An explicit parent keeps the trigger-parent guard, and so do
// member comments, comment-triggered runs, decision, notify-only and supersede
// requests.
func statusHandoffDerivesParent(task db.AgentTaskQueue, req CreateCommentRequest) bool {
	handoffRun := task.TriggerKind.Valid && task.TriggerKind.String == "status_handoff" && task.TriggerCommentID.Valid
	return (handoffRun || !task.TriggerCommentID.Valid) &&
		req.ParentID == nil && req.Type == "comment" &&
		!req.DecisionIntent && !req.DecisionRecord && !req.DecisionVeto &&
		!req.NotifyOnly && len(req.Supersedes) == 0
}

// deriveStatusHandoffParent returns the parent of the task's plain comment, or
// an invalid UUID (post at top level). Derived parents never feed trigger
// routing: the caller routes the comment as a top-level one. It runs in the
// insert transaction, before the thread lock.
//
// With an explicit handoff comment the parent is that comment's thread root.
// Otherwise it is the thread root of the newest comment in the task's handoff
// chain: the task plus every task reached through issue_handoff_dispatch edges,
// in either direction, so the mover and the started run join each other's thread
// in whichever order they post. A move that carries a handoff_note writes no
// dispatch row (armHandoffIntent), so it makes no edge and its run posts at top
// level unless another edge links it; so does a move by a member. The handoff
// lock makes two first posts of one chain take turns: the second reads the
// first one's committed comment.
func deriveStatusHandoffParent(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, issue db.Issue) (pgtype.UUID, error) {
	anchor := task.TriggerCommentID
	if !(task.TriggerKind.Valid && task.TriggerKind.String == "status_handoff" && anchor.Valid) {
		edge := db.LockHandoffThreadParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, TaskID: task.ID}
		if err := q.LockHandoffThread(ctx, edge); err != nil {
			return pgtype.UUID{}, err
		}
		var err error
		anchor, err = q.GetNewestHandoffChainComment(ctx, db.GetNewestHandoffChainCommentParams(edge))
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, nil
		}
		if err != nil {
			return pgtype.UUID{}, err
		}
	}
	root, err := q.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: anchor, WorkspaceID: issue.WorkspaceID})
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

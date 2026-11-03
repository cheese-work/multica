package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DMLane names the progress lane of one DM continuation chain: its root session
// and the member who owns it.
type DMLane struct {
	Lineage pgtype.UUID
	Owner   pgtype.UUID
}

// ProgressVisible is the one gate between a DM owner and an issue's progress.
// Issue read access is workspace membership today and no private issues exist,
// so a private issue or project changes only this function (CHE-3369 I3.4).
func ProgressVisible(ctx context.Context, q *db.Queries, owner pgtype.UUID, issue db.Issue) bool {
	_, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: owner, WorkspaceID: issue.WorkspaceID})
	return err == nil
}

// DMLaneForTask is the lane of the DM a running task answers in. It holds only
// for a native DM of this workspace whose creator started the task; a
// channel-bridged chat or a task with no DM has no lane (I3.1).
func DMLaneForTask(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, workspaceID pgtype.UUID) (DMLane, bool) {
	if !task.ChatSessionID.Valid || !task.InitiatorUserID.Valid {
		return DMLane{}, false
	}
	return dmLane(ctx, q, task.ChatSessionID, task.InitiatorUserID, workspaceID)
}

// DMLaneForMember is the lane of a DM that member created. Any session of the
// chain names it; the lane is the chain's root.
func DMLaneForMember(ctx context.Context, q *db.Queries, sessionID, member, workspaceID pgtype.UUID) (DMLane, bool) {
	return dmLane(ctx, q, sessionID, member, workspaceID)
}

func dmLane(ctx context.Context, q *db.Queries, sessionID, creator, workspaceID pgtype.UUID) (DMLane, bool) {
	session, err := q.GetNativeChatSession(ctx, sessionID)
	if err != nil || session.WorkspaceID != workspaceID || session.CreatorID != creator {
		return DMLane{}, false
	}
	root, err := q.ResolveChatLineageRoot(ctx, session.ID)
	if err != nil {
		return DMLane{}, false
	}
	return DMLane{Lineage: root, Owner: creator}, true
}

// AddProgressFollow follows issue in lane. It takes the lane row lock, so it
// serializes with every other write to the lane. The bool is false when the
// owner cannot read the issue or an active follow already exists.
func AddProgressFollow(ctx context.Context, q *db.Queries, lane DMLane, issue db.Issue, source string, mutedAt pgtype.Timestamptz, parent pgtype.UUID) (db.DmProgressFollow, bool, error) {
	if !ProgressVisible(ctx, q, lane.Owner, issue) {
		return db.DmProgressFollow{}, false, nil
	}
	if err := q.EnsureDMProgressLane(ctx, db.EnsureDMProgressLaneParams{LineageID: lane.Lineage, WorkspaceID: issue.WorkspaceID, OwnerUserID: lane.Owner}); err != nil {
		return db.DmProgressFollow{}, false, err
	}
	locked, err := q.LockDMProgressLane(ctx, lane.Lineage)
	if err != nil {
		return db.DmProgressFollow{}, false, err
	}
	if locked.OwnerUserID != lane.Owner || locked.WorkspaceID != issue.WorkspaceID {
		return db.DmProgressFollow{}, false, nil
	}
	follow, err := q.InsertDMProgressFollow(ctx, db.InsertDMProgressFollowParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, LineageID: lane.Lineage, OwnerUserID: lane.Owner,
		Source: source, MutedAt: mutedAt, LastRenderedStatus: issue.Status, ParentFollowID: parent,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.DmProgressFollow{}, false, nil
	}
	return follow, err == nil, err
}

// followNewIssue runs inside the create transaction. The origin follow comes
// only from the authenticated acting task (I3.1); a child inherits each active
// follow of its parent, mute included (I3.2).
func followNewIssue(ctx context.Context, q *db.Queries, p IssueCreateParams, issue db.Issue) error {
	if p.CreatorType == "agent" && p.OriginType.String == "agent_create" && p.OriginID.Valid {
		task, err := q.GetAgentTaskInWorkspace(ctx, db.GetAgentTaskInWorkspaceParams{ID: p.OriginID, WorkspaceID: p.WorkspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && task.AgentID == p.CreatorID {
			if lane, ok := DMLaneForTask(ctx, q, task, p.WorkspaceID); ok {
				if _, _, err := AddProgressFollow(ctx, q, lane, issue, "origin", pgtype.Timestamptz{}, pgtype.UUID{}); err != nil {
					return err
				}
			}
		}
	}
	if !p.ParentIssueID.Valid {
		return nil
	}
	parentFollows, err := q.ListActiveDMProgressFollowsForIssue(ctx, p.ParentIssueID)
	if err != nil {
		return err
	}
	for _, listed := range parentFollows {
		// The list above predates the lane lock, so a mute or unfollow can commit
		// while we wait for it. Re-read the parent under the lock.
		if _, err := q.LockDMProgressLane(ctx, listed.LineageID); err != nil {
			return err
		}
		parent, err := q.GetActiveDMProgressFollow(ctx, db.GetActiveDMProgressFollowParams{IssueID: p.ParentIssueID, LineageID: listed.LineageID})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		lane := DMLane{Lineage: parent.LineageID, Owner: parent.OwnerUserID}
		if _, _, err := AddProgressFollow(ctx, q, lane, issue, "child", parent.MutedAt, parent.ID); err != nil {
			return err
		}
	}
	return nil
}

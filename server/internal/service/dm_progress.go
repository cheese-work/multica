package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Follow reasons. A follow is effective while it has at least one (M12).
const (
	ReasonOrigin = "origin"
	ReasonManual = "manual"
	// An inherited reason names the root the follow came from: "inherited:<issue id>".
	reasonInheritedPrefix = "inherited:"
)

func isInherited(reason string) bool { return strings.HasPrefix(reason, reasonInheritedPrefix) }

// InheritedReason is the reason a follow carries for coming from root.
func InheritedReason(root pgtype.UUID) string {
	return reasonInheritedPrefix + uuid.UUID(root.Bytes).String()
}

// IsRoot reports whether a follow holds an independent reason, so that its issue
// is the root of the follows its children inherit.
func IsRoot(f db.DmProgressFollow) bool {
	return slices.ContainsFunc(f.Reasons, func(r string) bool { return !isInherited(r) })
}

// inheritedReasons is what a child receives from a parent follow: one reason per
// root of the parent. An independent reason makes the parent itself a root; a
// reason inherited from a root passes on unchanged.
func inheritedReasons(parent db.Issue, f db.DmProgressFollow) []string {
	var out []string
	for _, r := range f.Reasons {
		if !isInherited(r) {
			r = InheritedReason(parent.ID)
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

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

// AddProgressFollow adds reasons to the follow of issue in lane. root is the
// reporting root a new follow gets (the issue itself for an origin or manual
// follow); an existing follow keeps the root it was created with. It takes the
// lane row lock, so it serializes with every other write to the lane. The bool
// is false when the owner cannot read the issue, or when the reasons are all
// inherited and the issue was explicitly unfollowed (a negative override).
func AddProgressFollow(ctx context.Context, q *db.Queries, lane DMLane, issue db.Issue, reasons []string, root pgtype.UUID, mutedAt pgtype.Timestamptz) (db.DmProgressFollow, bool, error) {
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
		Reasons: reasons, Explicit: slices.ContainsFunc(reasons, func(r string) bool { return !isInherited(r) }),
		MutedAt: mutedAt, LastRenderedStatus: issue.Status, ReportingRootID: root,
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
				if _, _, err := AddProgressFollow(ctx, q, lane, issue, []string{ReasonOrigin}, issue.ID, pgtype.Timestamptz{}); err != nil {
					return err
				}
			}
		}
	}
	if !p.ParentIssueID.Valid {
		return nil
	}
	parent, err := q.GetIssue(ctx, p.ParentIssueID)
	if err != nil {
		return err
	}
	return InheritProgressFollows(ctx, q, parent, issue)
}

// InheritProgressFollows gives child each active follow of parent, mute
// included (I3.2). Callers run it on child creation and on any later child
// event. A child the member unfollowed is not followed again (M12).
func InheritProgressFollows(ctx context.Context, q *db.Queries, parent, child db.Issue) error {
	parentFollows, err := q.ListActiveDMProgressFollowsForIssue(ctx, parent.ID)
	if err != nil {
		return err
	}
	// Nothing to inherit: skip the ancestor walk, which runs under the workspace
	// issue-counter lock on child creation (SEC-1).
	if len(parentFollows) == 0 {
		return nil
	}
	// A looping parent chain has no top: the child reports to itself.
	chain, err := q.IssueParentChainLoops(ctx, child.ID)
	if err != nil {
		return err
	}
	loops := chain.Bool
	for _, listed := range parentFollows {
		// The list above predates the lane lock, so a mute or unfollow can commit
		// while we wait for it. Re-read the parent under the lock.
		if _, err := q.LockDMProgressLane(ctx, listed.LineageID); err != nil {
			return err
		}
		current, err := q.GetActiveDMProgressFollow(ctx, db.GetActiveDMProgressFollowParams{IssueID: parent.ID, LineageID: listed.LineageID})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		lane := DMLane{Lineage: current.LineageID, Owner: current.OwnerUserID}
		root := current.ReportingRootID
		if loops {
			root = child.ID
		}
		follow, ok, err := AddProgressFollow(ctx, q, lane, child, inheritedReasons(parent, current), root, current.MutedAt)
		if err != nil {
			return err
		}
		if loops && ok {
			err = q.InsertDMProgressDataIncident(ctx, db.InsertDMProgressDataIncidentParams{
				WorkspaceID: child.WorkspaceID, IssueID: child.ID, FollowID: follow.ID, Kind: "parent_cycle",
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// CountProgressDescendants counts the tracked descendants of root in lane: each
// issue once, however deep, with no cap.
func CountProgressDescendants(ctx context.Context, q *db.Queries, lane, root pgtype.UUID) (int64, error) {
	return q.CountDMProgressDescendants(ctx, db.CountDMProgressDescendantsParams{LineageID: lane, ReportingRootID: root})
}

// ListProgressDescendants returns one page of root's tracked descendants in
// issue-id order, starting after the given id (invalid for the first page). An
// empty page ends the walk.
func ListProgressDescendants(ctx context.Context, q *db.Queries, lane, root, after pgtype.UUID, limit int32) ([]pgtype.UUID, error) {
	return q.ListDMProgressDescendants(ctx, db.ListDMProgressDescendantsParams{LineageID: lane, ReportingRootID: root, AfterIssueID: after, Limit: limit})
}

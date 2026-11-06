package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/ciwatch"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/issueposition"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CIWatchDestinations returns the transactional issue, comment and inbox entry points
// the CI watcher's executors write through. Every write joins the ledger's transaction;
// events and runs go out only after it commits.
func (h *Handler) CIWatchDestinations() ciwatch.Destinations {
	d := ciWatchDest{h: h}
	return ciwatch.Destinations{Issue: d, Comment: d, Inbox: d}
}

type ciWatchDest struct{ h *Handler }

func (d ciWatchDest) CreateIssue(ctx context.Context, tx pgx.Tx, s ciwatch.IssueSpec) (string, error) {
	ws, err := util.ParseUUID(s.WorkspaceID)
	if err != nil {
		return "", err
	}
	creator, err := util.ParseUUID(s.CreatorID)
	if err != nil || s.CreatorID == "" {
		return "", &ciwatch.PermanentError{Reason: "no_creator", Err: errors.New("workspace has no member to author the issue")}
	}
	qtx := d.h.Queries.WithTx(tx)
	number, err := service.AllocateIssueNumber(ctx, qtx, ws, service.ResolveIssueCountPolicy(ctx, d.h.Entitlements, ws))
	if err != nil {
		return "", err
	}
	position, err := issueposition.NextTopPosition(ctx, tx, ws, "todo")
	if err != nil {
		return "", err
	}
	params := db.CreateIssueWithOriginParams{
		ID: dbid.NewV7(), WorkspaceID: ws, Title: s.Title,
		Description: pgtype.Text{String: s.Body, Valid: true}, Status: "todo", Priority: s.Priority,
		CreatorType: "member", CreatorID: creator, Position: position, Number: number,
	}
	if s.AssigneeType != "" && s.AssigneeID != "" {
		if params.AssigneeID, err = util.ParseUUID(s.AssigneeID); err != nil {
			return "", err
		}
		params.AssigneeType = pgtype.Text{String: s.AssigneeType, Valid: true}
	}
	issue, err := qtx.CreateIssueWithOrigin(ctx, params)
	if err != nil {
		return "", err
	}
	if err := writeIssueMetadata(ctx, tx, issue.ID, s.Metadata); err != nil {
		return "", err
	}
	var task *db.AgentTaskQueue
	if s.Wake {
		if task, err = d.enqueueOwnerRun(ctx, qtx, issue, s.CreatorID); err != nil {
			return "", fmt.Errorf("enqueue owner run: %w", err)
		}
	}
	ciwatch.AfterCommit(ctx, func() {
		resp := issueToResponse(issue, d.h.getIssuePrefix(context.WithoutCancel(ctx), issue.WorkspaceID))
		d.h.publish(protocol.EventIssueCreated, s.WorkspaceID, "member", s.CreatorID, map[string]any{"issue": resp})
		if task != nil {
			d.h.TaskService.PublishTaskEnqueued(context.WithoutCancel(ctx), *task)
		}
	})
	return util.UUIDToString(issue.ID), nil
}

// enqueueOwnerRun enqueues the run of the issue's owner in the caller's tx: the agent
// itself, or a squad's leader. It returns nil for an unassigned or member-owned issue,
// and when the principal may not invoke the agent: the issue stays, no run starts.
// The principal is the repo's configured notify member; permissions are read now,
// not when the repo was configured, so a revoked grant or archived agent stops the wake.
func (d ciWatchDest) enqueueOwnerRun(ctx context.Context, qtx *db.Queries, issue db.Issue, principal string) (*db.AgentTaskQueue, error) {
	var task db.AgentTaskQueue
	// The task is attributed to the member the permission check ran against, not the
	// issue creator (an adopted issue has another creator).
	actor, err := util.ParseUUID(principal)
	if err != nil {
		return nil, nil
	}
	switch issue.AssigneeType.String {
	case "agent":
		if !d.mayInvoke(ctx, qtx, issue.AssigneeID, issue.WorkspaceID, principal) {
			return nil, nil
		}
		task, err = d.h.TaskService.EnqueueIssueTaskInTxAs(ctx, qtx, issue, actor)
	case "squad":
		var squad db.Squad
		if squad, err = qtx.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID}); err != nil {
			return nil, fmt.Errorf("load squad: %w", err)
		}
		if !d.mayInvoke(ctx, qtx, squad.LeaderID, issue.WorkspaceID, principal) {
			return nil, nil
		}
		task, err = d.h.TaskService.EnqueueDecisionTaskInTxAs(ctx, qtx, issue, squad.LeaderID, squad.ID, pgtype.UUID{}, nil, "", actor)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// mayInvoke is true when the member principal is in the workspace and may invoke the
// live, unarchived agent. Any lookup failure denies: an unknown answer is not a grant.
func (d ciWatchDest) mayInvoke(ctx context.Context, qtx *db.Queries, agentID, workspaceID pgtype.UUID, principal string) bool {
	if principal == "" {
		return false
	}
	ws := util.UUIDToString(workspaceID)
	agent, err := qtx.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
	if err != nil || agent.ArchivedAt.Valid {
		return false
	}
	if _, err := workspaceMemberVia(ctx, qtx, principal, ws); err != nil {
		return false
	}
	ok, err := d.h.invokeAgentDecisionVia(ctx, qtx, agent, "member", principal, "", ws)
	return err == nil && ok
}

func writeIssueMetadata(ctx context.Context, tx pgx.Tx, id pgtype.UUID, m map[string]any) error {
	if len(m) == 0 {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE issue SET metadata = metadata || $2::jsonb WHERE id = $1`, id, raw)
	return err
}

func (d ciWatchDest) UpdateIssue(ctx context.Context, tx pgx.Tx, u ciwatch.IssueUpdate) error {
	id, err := util.ParseUUID(u.IssueID)
	if err != nil {
		return err
	}
	// Idempotent: the prefix is added once, and an already-urgent issue stays as is.
	if _, err = tx.Exec(ctx, `UPDATE issue SET
			title = CASE WHEN $2 <> '' AND left(title, length($2)) <> $2 THEN $2 || title ELSE title END,
			priority = CASE WHEN $3 <> '' THEN $3 ELSE priority END, updated_at = now()
		WHERE id = $1`, id, u.TitlePrefix, u.Priority); err != nil || !u.Wake {
		return err
	}
	qtx := d.h.Queries.WithTx(tx)
	issue, err := qtx.GetIssue(ctx, id)
	if err != nil {
		return err
	}
	// A savepoint: a run already pending for the owner is the wake's goal, not a failure.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sp.Rollback(ctx) }()
	task, err := d.enqueueOwnerRun(ctx, d.h.Queries.WithTx(sp), issue, u.PrincipalID)
	if errors.Is(err, service.ErrDuplicatePendingTask) || (err == nil && task == nil) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("enqueue owner run: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return err
	}
	ciwatch.AfterCommit(ctx, func() { d.h.TaskService.PublishTaskEnqueued(context.WithoutCancel(ctx), *task) })
	return nil
}

func (d ciWatchDest) AddComment(ctx context.Context, tx pgx.Tx, s ciwatch.CommentSpec) (string, error) {
	issueID, err := util.ParseUUID(s.IssueID)
	if err != nil {
		return "", err
	}
	issue, err := d.h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return "", err
	}
	// The set the comment-trigger path would act on for this exact body. Storing all of it
	// as suppressed makes completion replay skip every one of them too.
	triggers, targets := d.h.computeCommentAgentTriggers(ctx, issue, s.Body, nil, "member", s.AuthorID, commentTriggerComputeOptions{})
	suppress := ciWatchSuppressSet(triggers, targets)
	author, err := util.ParseUUID(s.AuthorID)
	if err != nil {
		return "", &ciwatch.PermanentError{Reason: "no_author", Err: errors.New("workspace has no member to author the comment")}
	}
	created, err := d.h.Queries.WithTx(tx).CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "member", AuthorID: author,
		Content: s.Body, Type: "comment", SuppressedAgentIds: suppress,
	})
	if err != nil {
		return "", err
	}
	comment := created.Comment()
	ciwatch.AfterCommit(ctx, func() {
		d.h.publish(protocol.EventCommentCreated, s.WorkspaceID, "member", s.AuthorID, map[string]any{
			"comment": commentToResponse(comment, nil, nil), "issue_title": issue.Title,
			"issue_assignee_type": textToPtr(issue.AssigneeType), "issue_assignee_id": uuidToPtr(issue.AssigneeID),
			"issue_status": issue.Status, "issue_revision": created.IssueRevision,
		})
	})
	return util.UUIDToString(comment.ID), nil
}

// ciWatchSuppressSet is every agent a comment would run, deduped, in a stable order.
func ciWatchSuppressSet(triggers []commentAgentTrigger, targets []commentMentionTarget) []pgtype.UUID {
	seen := map[string]bool{}
	out := []pgtype.UUID{}
	add := func(id pgtype.UUID) {
		if k := util.UUIDToString(id); id.Valid && !seen[k] {
			seen[k] = true
			out = append(out, id)
		}
	}
	for _, t := range triggers {
		add(t.Agent.ID)
	}
	for _, t := range targets {
		if id, err := util.ParseUUID(strings.TrimSpace(t.ExecAgentID)); err == nil {
			add(id)
		}
	}
	return out
}

func (d ciWatchDest) Notify(ctx context.Context, tx pgx.Tx, s ciwatch.NotifySpec) (string, error) {
	ws, err := util.ParseUUID(s.WorkspaceID)
	if err != nil {
		return "", err
	}
	member, err := util.ParseUUID(s.MemberID)
	if err != nil || s.MemberID == "" {
		return "", &ciwatch.PermanentError{Reason: "no_recipient", Err: errors.New("no Notify member configured")}
	}
	item, inserted, err := inbox.CreateIfEligible(ctx, d.h.Queries.WithTx(tx), db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: ws, RecipientType: "member", RecipientID: member,
		Type: "ci_watch", Severity: "attention", Title: s.Title, Body: pgtype.Text{String: s.Body, Valid: true},
		ActorType: pgtype.Text{String: "system", Valid: true}, Details: []byte("{}"),
	})
	if err != nil || !inserted {
		return "", err
	}
	ciwatch.AfterCommit(ctx, func() {
		d.h.publish(protocol.EventInboxNew, s.WorkspaceID, "system", "", map[string]any{"item": inboxToResponse(item)})
	})
	return util.UUIDToString(item.ID), nil
}

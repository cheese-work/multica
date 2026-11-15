package handler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// System close (CHE-2591 L5 design §4.5.1). Readiness rules 1 and 2 of §6 end
// the episode without a continuation: the issue is closed, or the asking agent
// can never receive one. One transaction cancels every bound request that is
// still open or answered, acknowledges the rest of B, and closes the episode,
// so no later reopen or restore finds anything to answer, withdraw or wake.

// systemCloseReason is the §6 rule that applies, or "" when neither does.
func systemCloseReason(snapshot decisionWakeSnapshot) string {
	if episodeClosedStatus(snapshot.issue.Status) {
		return "issue_closed"
	}
	if recipient := snapshot.recipient; recipient.ArchivedAt.Valid || !recipient.RuntimeID.Valid || recipient.Kind == "system" {
		return "no_resume_agent"
	}
	return ""
}

// systemCloseCancelReason is the cancel reason the lifecycle's routine records.
func systemCloseCancelReason(snapshot decisionWakeSnapshot, closeReason string) string {
	switch {
	case closeReason == "issue_closed":
		return "issue_closed"
	case snapshot.recipient.ArchivedAt.Valid:
		return "agent_archived"
	}
	return "agent_unavailable"
}

// lockDecisionSystemClose takes the lifecycle's lock order (workspace, session,
// asking agent, thread, issue) and then the episode's items. restart is true
// when a row the close needs is gone.
func (handler *Handler) lockDecisionSystemClose(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot) (roots map[pgtype.UUID]*db.Comment, restart bool, err error) {
	workspaceID, wait := snapshot.wait.WorkspaceID, snapshot.wait
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return nil, false, err
	}
	locked := make(map[pgtype.UUID]bool)
	for _, request := range snapshot.requests {
		if locked[request.ChatSessionID] {
			continue
		}
		locked[request.ChatSessionID] = true
		if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: workspaceID, ID: request.ChatSessionID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, err
		}
	}
	if _, err := queries.LockDecisionAskingAgent(ctx, db.LockDecisionAskingAgentParams{WorkspaceID: workspaceID, ID: wait.AskingAgentID}); err != nil {
		return nil, false, err
	}
	roots = make(map[pgtype.UUID]*db.Comment)
	for _, request := range snapshot.requests {
		root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, workspaceID, request.IssueCommentID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, err
		}
		roots[request.ID] = root
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: wait.IssueID}); err != nil {
		return nil, false, err
	}
	for _, intent := range snapshot.intents {
		if _, err := queries.LockDecisionIntent(ctx, db.LockDecisionIntentParams{WorkspaceID: workspaceID, ID: intent.ID}); err != nil {
			return nil, false, err
		}
		if slices.Contains(wait.GroupAckIntentIds, intent.ID) {
			if _, err := queries.LockDecisionDeliveryTargets(ctx, db.LockDecisionDeliveryTargetsParams{WorkspaceID: workspaceID, ID: intent.ID}); err != nil {
				return nil, false, err
			}
		}
	}
	for _, request := range snapshot.requests {
		if _, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: request.ID}); err != nil {
			return nil, false, err
		}
	}
	for _, answer := range snapshot.answers {
		if _, err := queries.LockDecisionAnswer(ctx, db.LockDecisionAnswerParams{WorkspaceID: workspaceID, ID: answer.ID}); err != nil {
			return nil, false, err
		}
	}
	_, err = queries.LockDecisionWait(ctx, db.LockDecisionWaitParams{WorkspaceID: workspaceID, ID: wait.ID})
	return roots, errors.Is(err, pgx.ErrNoRows), ignoreNoRows(err)
}

// closeDecisionEpisode is design §4.5.1. restart is true when the snapshot is
// stale; when the rule no longer holds under the locks nothing is committed.
func (handler *Handler) closeDecisionEpisode(ctx context.Context, snapshot decisionWakeSnapshot, reason string) (bool, error) {
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	workspaceID, wait := snapshot.wait.WorkspaceID, snapshot.wait
	roots, restart, err := handler.lockDecisionSystemClose(ctx, queries, snapshot)
	if err != nil || restart {
		return restart, err
	}
	current, err := handler.loadDecisionEpisode(ctx, queries, workspaceID, wait.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(current, snapshot) {
		return true, nil
	}
	if systemCloseReason(current) != reason {
		return true, nil
	}
	// §4.5: no resume agent closes only with the issue blocked. A workflow without
	// a blocked step cannot take that write, so the episode stays open and
	// recoverable (answers deliverable) and retries behind the others.
	if reason == "no_resume_agent" && current.issue.Status != issuestatus.Blocked {
		def, _, err := issueworkflow.ForIssue(ctx, queries, current.issue.WorkspaceID, current.issue.WorkflowID, current.issue.ProjectID)
		if err != nil {
			return false, err
		}
		if def != nil && !def.Has(issuestatus.Blocked) {
			transaction.Rollback(ctx)
			return false, handler.Queries.MarkDecisionWaitCloseRefused(ctx, db.MarkDecisionWaitCloseRefusedParams{WorkspaceID: workspaceID, ID: wait.ID})
		}
	}
	// The episode is claimed before any status write, so the write finds no open wait.
	if rows, err := queries.ClaimDecisionWait(ctx, db.ClaimDecisionWaitParams{WorkspaceID: workspaceID, ID: wait.ID, CloseReason: pgtype.Text{String: reason, Valid: true}}); err != nil {
		return false, err
	} else if rows != 1 {
		return true, nil
	}
	// Acknowledge first, then cancel with the zero answer, so the live answer is skipped once.
	if restart, err := ackEpisodeItems(ctx, queries, snapshot, "skipped_ineligible"); err != nil || restart {
		return restart, err
	}
	cancelReason := systemCloseCancelReason(snapshot, reason)
	var outcomes []decisionCancelOutcome
	var cancelled []db.DecisionRequest
	for _, request := range snapshot.requests {
		if request.Status != "open" && request.Status != "answered" {
			continue
		}
		out, err := cancelDecisionLifecycleLocked(ctx, queries, request, db.DecisionAnswer{}, roots[request.ID], cancelReason)
		if err != nil {
			return false, err
		}
		outcomes = append(outcomes, out)
		cancelled = append(cancelled, request)
	}
	var move edgeMove
	blocked := false
	if reason == "no_resume_agent" && current.issue.Status != issuestatus.Blocked {
		if move, blocked, err = handler.blockOnNoResumeAgent(ctx, queries, snapshot, cancelled, cancelReason); err != nil {
			return false, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, err
	}
	for index, out := range outcomes {
		handler.publishDecisionCancelNote(workspaceID, cancelled[index], out)
		handler.publishWithdrawal(out.withdrawal)
	}
	if blocked {
		move.AfterCommit(ctx)
	}
	return false, nil
}

// blockOnNoResumeAgent moves the issue to blocked with one comment that names
// the requests, quotes the answers, names R and states why nothing resumes.
func (handler *Handler) blockOnNoResumeAgent(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot, cancelled []db.DecisionRequest, cancelReason string) (edgeMove, bool, error) {
	// The cancel comments advanced the revision the move expects.
	issue, err := queries.GetIssue(ctx, snapshot.issue.ID)
	if err != nil {
		return edgeMove{}, false, err
	}
	move, err := handler.moveIssueOnEdgeTx(ctx, queries, issue, issuestatus.Blocked, "decision_wait", nil, false, nil)
	if err != nil {
		return edgeMove{}, false, err
	}
	human, err := queries.GetUser(ctx, snapshot.wait.HumanUserID)
	if err != nil {
		return edgeMove{}, false, err
	}
	var content strings.Builder
	why := "The asking agent is archived."
	switch {
	case snapshot.recipient.Kind == "system":
		why = "The asking agent is a system agent and cannot continue this work."
	case cancelReason == "agent_unavailable":
		why = "The asking agent lost its runtime binding."
	}
	fmt.Fprintf(&content, "Decision wait closed: %s No agent can resume this ask, so the issue is blocked. The human in charge was %s.\n", why, human.Name)
	for _, request := range cancelled {
		fmt.Fprintf(&content, "- Request %s cancelled `%s`.\n", uuidToString(request.ID), cancelReason)
	}
	for _, answer := range snapshot.answers {
		if !episodeLiveAnswer(answer) {
			continue
		}
		quoted, err := queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: answer.IssueCommentID})
		if err != nil {
			return edgeMove{}, false, err
		}
		fmt.Fprintf(&content, "- Answer to request %s, not delivered:\n> %s\n", uuidToString(answer.DecisionRequestID), strings.ReplaceAll(quoted.Content, "\n", "\n> "))
	}
	if _, err := queries.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "comment", Content: content.String(),
	}); err != nil {
		return edgeMove{}, false, err
	}
	return move, true, nil
}

package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Episode delivery (CHE-2591 L5 design §4.4, §6, §6.1). One delivery covers
// every answer, re-ask and retraction of the bound set B, and goes through the
// same dispatcher as an ordinary wake. The system close is in
// decision_wait_close.go and the suspension write in decision_wait_suspend.go.

// terminalRow is one request's row of the §6.1 terminal-state map.
type terminalRow struct {
	priority int
	reason   string
}

func episodeLiveAnswer(answer db.DecisionAnswer) bool {
	return !answer.DisclaimedAt.Valid && !answer.WokenAt.Valid
}

func episodePendingRetraction(answer db.DecisionAnswer) bool {
	return answer.WokenAt.Valid && answer.WakeOutcome.Valid && answer.WakeOutcome.String != "skipped_ineligible" &&
		answer.DisclaimedAt.Valid && answer.WithdrawalCommentID.Valid && !answer.RetractionDeliveredAt.Valid
}

func episodePendingReask(request db.DecisionRequest) bool {
	return request.Status == "cancelled" && (request.CancelReason.String == "human_removed" || request.CancelReason.String == "lane_archived") &&
		request.CancelNoticePostedAt.Valid && !request.ReaskDeliveredAt.Valid
}

// episodeLiveRequests is the set of requests that hold a live answer.
func episodeLiveRequests(snapshot decisionWakeSnapshot) map[pgtype.UUID]bool {
	live := make(map[pgtype.UUID]bool)
	for _, answer := range snapshot.answers {
		if episodeLiveAnswer(answer) {
			live[answer.DecisionRequestID] = true
		}
	}
	return live
}

// classifyEpisodeRequest is the §6.1 map. Every value the check constraints
// allow lands in a row; a value without an explicit row falls to other_terminal.
func classifyEpisodeRequest(request db.DecisionRequest, live, actionExpired map[pgtype.UUID]bool) terminalRow {
	switch request.Status {
	case "answered":
		if live[request.ID] {
			return terminalRow{1, "answered"}
		}
	case "cancelled":
		switch request.CancelReason.String {
		case "human_removed", "lane_archived":
			return terminalRow{2, "reask"}
		case "expired":
			if actionExpired[request.ID] {
				return terminalRow{3, "action_expired"}
			}
			return terminalRow{3, "expired"}
		case "agent_withdrawn":
			if request.WithdrawnByUserID.Valid {
				return terminalRow{4, "human_withdrawn"}
			}
			if request.WithdrawnByTaskID.Valid {
				return terminalRow{6, "agent_withdrawn"}
			}
		case "issue_closed", "agent_archived", "agent_unavailable":
			return terminalRow{5, "system_cancelled"}
		}
	}
	return terminalRow{7, "other_terminal"}
}

type episodeVerdict struct {
	// wait is true when nothing is delivered this pass.
	wait    bool
	reason  string
	rows    map[pgtype.UUID]terminalRow
	running pgtype.UUID // the withdrawing task that is still running and is the continuation
	// unauthorized is set with wait when a live request's person or authority is
	// gone. The episode stays open; only the system close (L5d) ends it.
	unauthorized bool
}

// memberMovedAfter reports whether a person's move opened a run-budget window
// after t: only an interactive session's move does (CHE-2798 S1d).
func memberMovedAfter(ctx context.Context, queries *db.Queries, issueID pgtype.UUID, t time.Time) (bool, error) {
	moved, err := queries.LatestMemberStatusChange(ctx, issueID)
	return movedAfter(moved, err, t)
}

// memberMovedStatusAfter reports whether a person moved the issue's status
// after t, whatever session made the move. A refused dispatch resumes on it, so
// it must match the dispatch_refused branch of ListDecisionEpisodeCandidates.
func memberMovedStatusAfter(ctx context.Context, queries *db.Queries, issueID pgtype.UUID, t time.Time) (bool, error) {
	moved, err := queries.LatestMemberStatusMove(ctx, issueID)
	return movedAfter(moved, err, t)
}

func movedAfter(moved pgtype.Timestamptz, err error, t time.Time) (bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && moved.Valid && moved.Time.After(t), err
}

// episodeResumeHolds is the resume condition of the current suspend_reason
// (design §4.4.6), checked under the locks.
func (handler *Handler) episodeResumeHolds(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot) (bool, error) {
	wait, issue := snapshot.wait, snapshot.issue
	if wait.Phase != "suspended" {
		return true, nil
	}
	suspendedAt := wait.SuspendedAt.Time
	switch wait.SuspendReason.String {
	case "run_budget":
		def, _, err := issueworkflow.ForIssue(ctx, queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
		if err != nil {
			return false, err
		}
		if def != nil {
			start, err := service.RunBudgetStart(ctx, queries, issue.ID, def)
			return start.After(suspendedAt), err
		}
		return memberMovedAfter(ctx, queries, issue.ID, suspendedAt)
	case "dispatch_refused":
		if wait.RefusedTaskID.Valid {
			task, err := queries.GetAgentTask(ctx, wait.RefusedTaskID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return false, err
			}
			switch task.Status {
			case "queued", "dispatched", "running", "waiting_local_directory":
				return memberMovedStatusAfter(ctx, queries, issue.ID, suspendedAt)
			}
		}
		return true, nil
	}
	return true, nil
}

// evaluateDecisionEpisode is design §6 over the locked bound set.
func (handler *Handler) evaluateDecisionEpisode(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot, now time.Time) (episodeVerdict, error) {
	verdict := episodeVerdict{wait: true}
	// Rules 1 and 2 never reach here: deliverDecisionEpisode closes first (decision_wait_close.go).
	if holds, err := handler.episodeResumeHolds(ctx, queries, snapshot); err != nil || !holds {
		return verdict, err
	}
	live := episodeLiveRequests(snapshot)
	for _, request := range snapshot.requests {
		if request.Status == "open" {
			return verdict, nil
		}
	}
	for _, answer := range snapshot.answers {
		if episodeLiveAnswer(answer) && live[answer.DecisionRequestID] && (!answer.WakeAt.Valid || answer.WakeAt.Time.After(now)) {
			return verdict, nil
		}
	}
	// Ordinary delivery re-checks the person and the authority under the locks
	// (decision_delivery.go); a live answer is delivered on the same terms.
	for _, request := range snapshot.requests {
		if !live[request.ID] {
			continue
		}
		var refusal decisionIntentRefusal
		if err := requireRequestAuthority(ctx, queries, snapshot.issue, request, http.StatusConflict); errors.As(err, &refusal) {
			verdict.unauthorized = true
			return verdict, nil
		} else if err != nil {
			return verdict, err
		}
	}
	expiredActions, err := queries.ListDecisionWaitExpiredActionRequests(ctx, db.ListDecisionWaitExpiredActionRequestsParams{WorkspaceID: snapshot.wait.WorkspaceID, RequestIds: snapshot.wait.RequestIds})
	if err != nil {
		return verdict, err
	}
	actionExpired := make(map[pgtype.UUID]bool, len(expiredActions))
	for _, id := range expiredActions {
		actionExpired[id] = true
	}
	verdict.wait, verdict.rows = false, make(map[pgtype.UUID]terminalRow, len(snapshot.requests))
	best := terminalRow{priority: 99}
	allTaskWithdrawn := len(snapshot.requests) > 0
	var withdrawnBy pgtype.UUID
	for _, request := range snapshot.requests {
		row := classifyEpisodeRequest(request, live, actionExpired)
		verdict.rows[request.ID] = row
		if row.priority < best.priority {
			best = row
		}
		if row.reason != "agent_withdrawn" || (withdrawnBy.Valid && withdrawnBy != request.WithdrawnByTaskID) {
			allTaskWithdrawn = false
		}
		withdrawnBy = request.WithdrawnByTaskID
	}
	verdict.reason = best.reason
	if allTaskWithdrawn {
		task, err := queries.GetAgentTask(ctx, withdrawnBy)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return verdict, err
		}
		if err == nil && task.Status == "running" && task.AgentID == snapshot.recipient.ID && task.IssueID == snapshot.issue.ID {
			verdict.running = task.ID
		}
	}
	return verdict, nil
}

// loadDecisionEpisode reads an episode and everything its delivery depends on:
// the issue, B with its answers, the intents, and the asking agent in the
// saved squad context. A closed episode is no item.
func (handler *Handler) loadDecisionEpisode(ctx context.Context, queries *db.Queries, workspaceID, waitID pgtype.UUID) (decisionWakeSnapshot, error) {
	snapshot := decisionWakeSnapshot{kind: "episode", itemID: waitID}
	wait, err := queries.GetDecisionWait(ctx, db.GetDecisionWaitParams{WorkspaceID: workspaceID, ID: waitID})
	if err != nil {
		return snapshot, err
	}
	if wait.ClosedAt.Valid {
		return snapshot, pgx.ErrNoRows
	}
	snapshot.wait = wait
	if snapshot.issue, err = queries.GetIssue(ctx, wait.IssueID); err != nil || snapshot.issue.WorkspaceID != workspaceID {
		if err == nil {
			err = pgx.ErrNoRows
		}
		return snapshot, err
	}
	if snapshot.requests, err = queries.ListDecisionWaitRequests(ctx, db.ListDecisionWaitRequestsParams{WorkspaceID: workspaceID, RequestIds: wait.RequestIds}); err != nil {
		return snapshot, err
	}
	if snapshot.answers, err = queries.ListDecisionWaitAnswers(ctx, db.ListDecisionWaitAnswersParams{WorkspaceID: workspaceID, RequestIds: wait.RequestIds}); err != nil {
		return snapshot, err
	}
	if snapshot.intents, err = queries.ListDecisionWaitIntents(ctx, db.ListDecisionWaitIntentsParams{WorkspaceID: workspaceID, IntentIds: wait.IntentIds}); err != nil {
		return snapshot, err
	}
	for _, intentID := range wait.GroupAckIntentIds {
		targets, err := queries.ListDecisionDeliveryTargets(ctx, db.ListDecisionDeliveryTargetsParams{WorkspaceID: workspaceID, ID: intentID})
		if err != nil {
			return snapshot, err
		}
		snapshot.targets = append(snapshot.targets, targets...)
	}
	if wait.ResumeSquadID.Valid {
		squad, err := queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: wait.ResumeSquadID, WorkspaceID: workspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return snapshot, err
		}
		if err == nil && !squad.ArchivedAt.Valid && squad.LeaderID == wait.AskingAgentID {
			snapshot.squadID = squad.ID
		}
	}
	recipientID, err := handler.episodeRecipient(ctx, queries, &snapshot)
	if err != nil {
		return snapshot, err
	}
	err = handler.loadDecisionRecipient(ctx, queries, &snapshot, recipientID)
	return snapshot, err
}

// episodeRecipient is the agent the continuation wakes: the issue's owner
// (the placement the episode restores, or the current assignee when nothing is
// restored) when that is an eligible agent, otherwise the asking agent. A squad
// owner wakes its leader in the squad context.
func (handler *Handler) episodeRecipient(ctx context.Context, queries *db.Queries, snapshot *decisionWakeSnapshot) (pgtype.UUID, error) {
	wait, issue := snapshot.wait, snapshot.issue
	ownerType, ownerID := issue.AssigneeType, issue.AssigneeID
	if wait.RestoreOnResume {
		ownerType, ownerID = wait.RestoreAssigneeType, wait.RestoreAssigneeID
	}
	agentID, squadID := pgtype.UUID{}, pgtype.UUID{}
	switch ownerType.String {
	case "agent":
		agentID = ownerID
	case "squad":
		squad, err := queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: ownerID, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return wait.AskingAgentID, nil
		} else if err != nil {
			return wait.AskingAgentID, err
		}
		agentID, squadID = squad.LeaderID, squad.ID
	}
	if !agentID.Valid || agentID == wait.AskingAgentID {
		return wait.AskingAgentID, nil
	}
	owner, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: agentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return wait.AskingAgentID, nil
	} else if err != nil {
		return wait.AskingAgentID, err
	}
	if owner.ArchivedAt.Valid || !owner.RuntimeID.Valid || owner.Kind == "system" {
		return wait.AskingAgentID, nil
	}
	if invocable, err := handler.personMayInvoke(ctx, queries, owner.ID, wait.HumanUserID, issue.WorkspaceID); err != nil {
		return wait.AskingAgentID, err
	} else if !invocable {
		return wait.AskingAgentID, nil
	}
	snapshot.squadID = squadID
	return owner.ID, nil
}

// episodeRestore is design §4.4.2 step 6: one explicit-assignee write of the
// saved placement. A status the workflow no longer has becomes in_progress.
// The status row is written in the same transaction (nil when nothing moved).
func (handler *Handler) episodeRestore(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot) (db.Issue, *db.ActivityLog, error) {
	wait, issue := snapshot.wait, snapshot.issue
	status := wait.RestoreStatus
	def, _, err := issueworkflow.ForIssue(ctx, queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil {
		return issue, nil, err
	}
	if def != nil {
		if _, ok := def.Step(status); !ok {
			status = "in_progress"
		}
	}
	updated, err := queries.UpdateIssue(ctx, db.UpdateIssueParams{
		ID: issue.ID, Status: pgtype.Text{String: status, Valid: true},
		AssigneeType: wait.RestoreAssigneeType, AssigneeID: wait.RestoreAssigneeID,
		PreserveProject: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		return issue, nil, err
	}
	recorded, err := recordSystemStatusChange(ctx, queries, issue, updated, service.StatusCauseDecision, nil)
	return updated, recorded, err
}

// episodeInputs builds what the continuation carries: the comments it must
// read, one summary line per item of B (§6.1) and the comment that triggers it.
func (handler *Handler) episodeInputs(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot, verdict episodeVerdict) ([]pgtype.UUID, string, db.Comment, error) {
	var commentIDs []pgtype.UUID
	var trigger db.Comment
	var summary strings.Builder
	workspaceID := snapshot.issue.WorkspaceID
	collect := func(comment db.Comment) {
		commentIDs = append(commentIDs, comment.ID)
		if !trigger.ID.Valid || comment.CreatedAt.Time.After(trigger.CreatedAt.Time) ||
			(comment.CreatedAt.Time.Equal(trigger.CreatedAt.Time) && uuidToString(comment.ID) > uuidToString(trigger.ID)) {
			trigger = comment
		}
	}
	quote := func(commentID pgtype.UUID) (db.Comment, error) {
		return queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: workspaceID, ID: commentID})
	}
	fmt.Fprintf(&summary, "Decision wait ended (%s). Cover every item in this delivery:\n", verdict.reason)
	for _, request := range snapshot.requests {
		id := uuidToString(request.ID)
		row := verdict.rows[request.ID]
		if request.Status == "cancelled" {
			if cancel, err := queries.GetDecisionCancelComment(ctx, db.GetDecisionCancelCommentParams{WorkspaceID: workspaceID, ID: request.ID}); err == nil {
				collect(cancel)
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return nil, "", trigger, err
			}
		}
		switch row.reason {
		case "answered":
			// The quotes are listed with the answers below.
		case "reask":
			fmt.Fprintf(&summary, "- Request %s was cancelled (%s). Ask again through a new decision-intent window and a fresh lane listing. No automatic re-ping or server-created DM.\n", id, request.CancelReason.String)
		case "expired", "action_expired":
			human, err := queries.GetUser(ctx, request.HumanUserID)
			if err != nil {
				return nil, "", trigger, err
			}
			what, next := "expired", "Re-resolve the human in charge, or move the issue to blocked with this reason."
			if row.reason == "action_expired" {
				what, next = "expired: the prepared action expired before approval", "Move the issue to blocked with the expiry stated as the reason."
			}
			fmt.Fprintf(&summary, "- Request %s %s. It was cancelled `expired` and no answer was delivered. Expiry is not an answer: do not ask %s the same question again. %s\n", id, what, human.Name, next)
		case "human_withdrawn":
			human, err := queries.GetUser(ctx, request.WithdrawnByUserID)
			if err != nil {
				return nil, "", trigger, err
			}
			fmt.Fprintf(&summary, "- %s withdrew the ask: %s. Do not ask %s the same question again. Re-resolve the human in charge, or move the issue to blocked with this reason.\n", human.Name, request.WithdrawReason, human.Name)
		case "system_cancelled":
			human, err := queries.GetUser(ctx, request.HumanUserID)
			if err != nil {
				return nil, "", trigger, err
			}
			cause := "issue closed"
			if request.CancelReason.String != "issue_closed" {
				cause = "you were unavailable"
			}
			fmt.Fprintf(&summary, "- Your ask to %s was cancelled `%s` (%s). No answer was delivered. The card is no longer answerable.\n", human.Name, request.CancelReason.String, cause)
		case "agent_withdrawn":
			fmt.Fprintf(&summary, "- You withdrew your ask (request %s); continue from %s.\n", id, snapshot.issue.Status)
		default:
			fmt.Fprintf(&summary, "- Request %s ended: %s/%s\n", id, request.Status, request.CancelReason.String)
		}
	}
	for _, answer := range snapshot.answers {
		request := answer.DecisionRequestID
		switch {
		case episodeLiveAnswer(answer) && verdict.rows[request].reason != "" && snapshotRequest(snapshot, request).Status == "answered":
			comment, err := quote(answer.IssueCommentID)
			if err != nil {
				return nil, "", trigger, err
			}
			collect(comment)
			fmt.Fprintf(&summary, "- Quote comment %s (request %s)\n", uuidToString(comment.ID), uuidToString(request))
		case episodePendingRetraction(answer):
			comment, err := quote(answer.WithdrawalCommentID)
			if err != nil {
				return nil, "", trigger, err
			}
			collect(comment)
			fmt.Fprintf(&summary, "- Withdrawal comment %s (request %s, reason %s). Do not rely on the withdrawn quote.\n", uuidToString(comment.ID), uuidToString(request), answer.DisclaimReason.String)
		case answer.WokenAt.Valid && answer.WakeOutcome.String == "skipped_ineligible" && !answer.DisclaimedAt.Valid && verdict.rows[request].reason == "system_cancelled":
			comment, err := quote(answer.IssueCommentID)
			if err != nil {
				return nil, "", trigger, err
			}
			collect(comment)
			fmt.Fprintf(&summary, "- Quote comment %s (request %s), quoted for context only; it is not an answer; ask the person again in a new request if you still need the decision.\n", uuidToString(comment.ID), uuidToString(request))
		}
	}
	for _, target := range snapshot.targets {
		if !target.ConsumedAt.Valid {
			fmt.Fprintf(&summary, "- Target not asked: user %s\n", uuidToString(target.HumanUserID))
		}
	}
	if !trigger.ID.Valid && len(snapshot.requests) > 0 {
		comment, err := quote(snapshot.requests[0].IssueCommentID)
		if err != nil {
			return nil, "", trigger, err
		}
		collect(comment)
	}
	return commentIDs, summary.String(), trigger, nil
}

func snapshotRequest(snapshot decisionWakeSnapshot, id pgtype.UUID) db.DecisionRequest {
	for _, request := range snapshot.requests {
		if request.ID == id {
			return request
		}
	}
	return db.DecisionRequest{}
}

// lockDecisionEpisode takes the locks of design §4.4.2 step 1 in the existing
// order. restart is true when a row it needs is gone.
func (handler *Handler) lockDecisionEpisode(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot) (restart bool, err error) {
	workspaceID, wait := snapshot.wait.WorkspaceID, snapshot.wait
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return false, err
	}
	if snapshot.recipient.RuntimeID.Valid {
		if _, err := queries.LockDecisionRecipientRuntime(ctx, db.LockDecisionRecipientRuntimeParams{WorkspaceID: workspaceID, ID: snapshot.recipient.RuntimeID}); errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	if _, err := queries.LockDecisionAskingAgent(ctx, db.LockDecisionAskingAgentParams{WorkspaceID: workspaceID, ID: wait.AskingAgentID}); err != nil {
		return false, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: wait.IssueID}); err != nil {
		return false, err
	}
	if snapshot.task.ID.Valid {
		if _, err := queries.LockDecisionDeliveryTask(ctx, db.LockDecisionDeliveryTaskParams{WorkspaceID: workspaceID, ID: snapshot.task.ID}); errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	var humans, sources []pgtype.UUID
	for _, request := range snapshot.requests {
		humans = append(humans, request.HumanUserID)
	}
	for _, answer := range snapshot.answers {
		if episodeLiveAnswer(answer) {
			sources = append(sources, answer.ChatMessageIds...)
		}
	}
	for _, humanID := range decisionWakeIDs(humans) {
		if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: workspaceID, UserID: humanID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	sources = decisionWakeIDs(sources)
	locked, err := queries.LockDecisionSourceMessages(ctx, db.LockDecisionSourceMessagesParams{WorkspaceID: workspaceID, MessageIds: sources})
	if err != nil || len(locked) != len(sources) {
		return false, errors.Join(err, errors.New("decision episode source messages changed"))
	}
	for _, intent := range snapshot.intents {
		if _, err := queries.LockDecisionIntent(ctx, db.LockDecisionIntentParams{WorkspaceID: workspaceID, ID: intent.ID}); err != nil {
			return false, err
		}
		if slices.Contains(wait.GroupAckIntentIds, intent.ID) {
			if _, err := queries.LockDecisionDeliveryTargets(ctx, db.LockDecisionDeliveryTargetsParams{WorkspaceID: workspaceID, ID: intent.ID}); err != nil {
				return false, err
			}
		}
	}
	for _, request := range snapshot.requests {
		if _, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: request.ID}); err != nil {
			return false, err
		}
	}
	for _, answer := range snapshot.answers {
		if _, err := queries.LockDecisionAnswer(ctx, db.LockDecisionAnswerParams{WorkspaceID: workspaceID, ID: answer.ID}); err != nil {
			return false, err
		}
	}
	_, err = queries.LockDecisionWait(ctx, db.LockDecisionWaitParams{WorkspaceID: workspaceID, ID: wait.ID})
	return errors.Is(err, pgx.ErrNoRows), ignoreNoRows(err)
}

func ignoreNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// ackEpisodeItems is design §4.4.2 step 9: each item of B is acknowledged by the
// state it has under the locks. Every write must change exactly one row.
func ackEpisodeItems(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot, outcome string) (restart bool, err error) {
	workspaceID, wait := snapshot.wait.WorkspaceID, snapshot.wait
	text := pgtype.Text{String: outcome, Valid: true}
	exactlyOne := func(rows int64, err error, what string) error {
		if err == nil && rows != 1 {
			err = errors.New("decision episode " + what + " acknowledgement changed")
		}
		return err
	}
	live := episodeLiveRequests(snapshot)
	for _, answer := range snapshot.answers {
		switch {
		case episodeLiveAnswer(answer) && live[answer.DecisionRequestID]:
			rows, err := queries.AcknowledgeDecisionWake(ctx, db.AcknowledgeDecisionWakeParams{WorkspaceID: workspaceID, ID: answer.ID, Outcome: text})
			if err := exactlyOne(rows, err, "wake"); err != nil {
				return false, err
			}
		case episodePendingRetraction(answer):
			rows, err := queries.AcknowledgeDecisionRetraction(ctx, db.AcknowledgeDecisionRetractionParams{WorkspaceID: workspaceID, ID: answer.ID, Outcome: text})
			if err := exactlyOne(rows, err, "retraction"); err != nil {
				return false, err
			}
		}
	}
	for _, request := range snapshot.requests {
		if episodePendingReask(request) {
			rows, err := queries.AcknowledgeDecisionReask(ctx, db.AcknowledgeDecisionReaskParams{WorkspaceID: workspaceID, ID: request.ID, Outcome: text})
			if err := exactlyOne(rows, err, "re-ask"); err != nil {
				return false, err
			}
		}
	}
	for _, intent := range snapshot.intents {
		if slices.Contains(wait.GroupAckIntentIds, intent.ID) {
			rows, err := queries.AcknowledgeDecisionGroupWake(ctx, db.AcknowledgeDecisionGroupWakeParams{WorkspaceID: workspaceID, ID: intent.ID})
			if err := exactlyOne(rows, err, "group"); err != nil {
				return false, err
			}
		} else if !intent.GroupWokenAt.Valid {
			return true, nil
		}
	}
	return false, nil
}

// deliverDecisionEpisode is one attempt of design §4.4.2. restart is true when
// the snapshot is stale.
func (handler *Handler) deliverDecisionEpisode(ctx context.Context, snapshot decisionWakeSnapshot) (bool, error) {
	if reason := systemCloseReason(snapshot); reason != "" {
		return handler.closeDecisionEpisode(ctx, snapshot, reason)
	}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	workspaceID, wait := snapshot.wait.WorkspaceID, snapshot.wait
	if restart, err := handler.lockDecisionEpisode(ctx, queries, snapshot); err != nil || restart {
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
	now, err := queries.DecisionDeliveryClock(ctx)
	if err != nil {
		return false, err
	}
	verdict, err := handler.evaluateDecisionEpisode(ctx, queries, snapshot, now.Time)
	if err != nil {
		return false, err
	}
	if verdict.unauthorized {
		// Same demotion as a refused dispatch: the episode keeps its state and
		// retries behind the others instead of holding a batch slot.
		transaction.Rollback(ctx)
		return false, handler.Queries.MarkDecisionWaitDispatchRefused(ctx, db.MarkDecisionWaitDispatchRefusedParams{WorkspaceID: workspaceID, ID: wait.ID})
	}
	if verdict.wait {
		return false, nil
	}
	if refused, err := handler.refuseGatedAgentRecords(ctx, transaction, queries, snapshot); err != nil || refused {
		return false, err
	}
	// A refusal below takes everything after this point back and suspends instead.
	if _, err := transaction.Exec(ctx, "SAVEPOINT episode_resume"); err != nil {
		return false, err
	}
	suspend := func(kind string, conflict db.AgentTaskQueue) (bool, error) {
		if _, err := transaction.Exec(ctx, "ROLLBACK TO SAVEPOINT episode_resume"); err != nil {
			return false, err
		}
		move, err := handler.suspendDecisionEpisode(ctx, queries, snapshot, kind, conflict)
		if err != nil {
			return false, err
		}
		if err := transaction.Commit(ctx); err != nil {
			return false, err
		}
		if move.AfterCommit != nil {
			move.AfterCommit(ctx)
		}
		return false, nil
	}
	if rows, err := queries.ClaimDecisionWait(ctx, db.ClaimDecisionWaitParams{WorkspaceID: workspaceID, ID: wait.ID, CloseReason: pgtype.Text{String: verdict.reason, Valid: true}}); err != nil {
		return false, err
	} else if rows != 1 {
		return true, nil
	}
	restored := false
	var restoredActivity *db.ActivityLog
	if wait.RestoreOnResume {
		updated, recorded, err := handler.episodeRestore(ctx, queries, snapshot)
		if err != nil {
			return false, err
		}
		snapshot.issue, restored, restoredActivity = updated, true, recorded
	}
	commentIDs, summary, trigger, err := handler.episodeInputs(ctx, queries, snapshot, verdict)
	if err != nil {
		return false, err
	}
	for _, answer := range snapshot.answers {
		if episodeLiveAnswer(answer) {
			if err := captureDecisionAnswerWakeup(ctx, transaction, snapshot.issue.ID, answer.ID, workspaceID); err != nil {
				return false, err
			}
		}
	}
	outcome, continuation := "planned_input", verdict.running
	var queued db.AgentTaskQueue
	if !verdict.running.Valid {
		if outcome, queued, err = handler.dispatchDecisionInputs(ctx, queries, snapshot, trigger, commentIDs, summary); errors.Is(err, service.ErrRunBudgetExhausted) {
			return suspend(suspendKindRunBudget, db.AgentTaskQueue{})
		} else if err != nil {
			return false, err
		}
		if outcome == "" {
			if wait.Phase == "suspended" || (wait.DispatchRefusedSince.Valid && now.Time.Sub(wait.DispatchRefusedSince.Time) > dispatchRefusedSuspendAfter) {
				return suspend(suspendKindDispatchRefused, snapshot.task)
			}
			// The refusal keeps the episode open and unchanged; only the clock starts.
			transaction.Rollback(ctx)
			return false, handler.Queries.MarkDecisionWaitDispatchRefused(ctx, db.MarkDecisionWaitDispatchRefusedParams{WorkspaceID: workspaceID, ID: wait.ID})
		}
		continuation = snapshot.task.ID
		if queued.ID.Valid {
			continuation = queued.ID
		}
	}
	if rows, err := queries.RecordDecisionWaitResume(ctx, db.RecordDecisionWaitResumeParams{WorkspaceID: workspaceID, ID: wait.ID, ResumeTaskID: continuation, ResumeOutcome: pgtype.Text{String: outcome, Valid: true}}); err != nil || rows != 1 {
		return false, errors.Join(err, errors.New("decision episode resume record changed"))
	}
	if outcome != "run_enqueued" {
		step := pgtype.Text{String: snapshot.issue.Status, Valid: true}
		if err := queries.SetTaskWorkflowStep(ctx, db.SetTaskWorkflowStepParams{WorkflowStep: step, ID: continuation}); err != nil {
			return false, err
		}
	}
	if restart, err := ackEpisodeItems(ctx, queries, snapshot, outcome); err != nil || restart {
		return restart, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, err
	}
	askingID := uuidToString(wait.AskingAgentID)
	if restored {
		handler.publishSystemStatusChange(uuidToString(workspaceID), restoredActivity, map[string]any{
			"issue": handler.issueResponseWithCategory(ctx, snapshot.issue), "assignee_changed": true, "status_changed": restoredActivity != nil,
			"prev_status": current.issue.Status, "creator_type": current.issue.CreatorType, "creator_id": uuidToString(current.issue.CreatorID),
			"source": "decision_wait",
		})
	}
	if queued.ID.Valid {
		handler.TaskService.PublishTaskEnqueued(ctx, queued)
	}
	for _, answer := range snapshot.answers {
		if episodeLiveAnswer(answer) {
			handler.publishDecisionAnswerUpdated(uuidToString(workspaceID), "agent", askingID, answer.ID, snapshot.issue.ID, decisionStateEffective)
		}
	}
	return false, nil
}

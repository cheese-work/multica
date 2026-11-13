package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type decisionWakeSnapshot struct {
	kind      string
	itemID    pgtype.UUID
	issue     db.Issue
	request   db.DecisionRequest
	intent    db.DecisionIntent
	requests  []db.DecisionRequest
	answers   []db.DecisionAnswer
	targets   []db.DecisionIntentTarget
	recipient db.Agent
	squadID   pgtype.UUID
	task      db.AgentTaskQueue
	head      pgtype.Text
	group     bool
	// binding is the non-closed episode that binds an ordinary item's request;
	// wait and intents are the episode and its intents when kind is "episode".
	binding db.DecisionWait
	wait    db.DecisionWait
	intents []db.DecisionIntent
}

func (handler *Handler) DeliverDecisionWake(ctx context.Context, workspaceID, requestID pgtype.UUID) error {
	return handler.deliverDecisionItem(ctx, workspaceID, requestID, "")
}

func (handler *Handler) deliverDecisionItem(ctx context.Context, workspaceID, itemID pgtype.UUID, kind string) error {
	for attempt := 0; attempt < 3; attempt++ {
		snapshot, err := handler.loadDecisionItem(ctx, handler.Queries, workspaceID, itemID, kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		restart, err := handler.deliverDecisionWake(ctx, snapshot)
		if err != nil || !restart {
			return err
		}
	}
	return nil
}

func (handler *Handler) loadDecisionItem(ctx context.Context, queries *db.Queries, workspaceID, itemID pgtype.UUID, kind string) (decisionWakeSnapshot, error) {
	if kind == "episode" {
		return handler.loadDecisionEpisode(ctx, queries, workspaceID, itemID)
	}
	snapshot := decisionWakeSnapshot{kind: kind, itemID: itemID}
	requestID := itemID
	if kind == "retraction" {
		answer, err := queries.GetDecisionAnswer(ctx, db.GetDecisionAnswerParams{WorkspaceID: workspaceID, ID: itemID})
		if err != nil {
			return snapshot, err
		}
		requestID = answer.DecisionRequestID
		snapshot.answers = []db.DecisionAnswer{answer}
	}
	request, err := queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if err != nil {
		return snapshot, err
	}
	snapshot.request = request
	snapshot.binding, err = queries.GetBindingDecisionWait(ctx, db.GetBindingDecisionWaitParams{WorkspaceID: workspaceID, RequestID: requestID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return snapshot, err
	}
	snapshot.issue, err = queries.GetIssue(ctx, request.IssueID)
	if err != nil || snapshot.issue.WorkspaceID != workspaceID {
		if err == nil {
			err = pgx.ErrNoRows
		}
		return snapshot, err
	}
	params := db.ListDecisionDeliveryRequestsParams{WorkspaceID: workspaceID, RequestID: requestID}
	if kind == "" && request.DecisionIntentID.Valid {
		snapshot.intent, err = queries.GetDecisionIntent(ctx, db.GetDecisionIntentParams{WorkspaceID: workspaceID, ID: request.DecisionIntentID})
		if err != nil {
			return snapshot, err
		}
		snapshot.group = !snapshot.intent.GroupWokenAt.Valid
		if snapshot.group {
			params.IntentID, params.RequestID = request.DecisionIntentID, pgtype.UUID{}
			snapshot.targets, err = queries.ListDecisionDeliveryTargets(ctx, db.ListDecisionDeliveryTargetsParams{WorkspaceID: workspaceID, ID: request.DecisionIntentID})
			if err != nil {
				return snapshot, err
			}
		}
	}
	snapshot.requests, err = queries.ListDecisionDeliveryRequests(ctx, params)
	if err != nil {
		return snapshot, err
	}
	requestIDs := make([]pgtype.UUID, 0, len(snapshot.requests))
	for _, member := range snapshot.requests {
		if member.IssueID != request.IssueID || member.AskingAgentID != request.AskingAgentID {
			return snapshot, errors.New("decision group context mismatch")
		}
		requestIDs = append(requestIDs, member.ID)
	}
	if kind == "" {
		snapshot.answers, err = queries.ListDecisionDeliveryAnswers(ctx, db.ListDecisionDeliveryAnswersParams{WorkspaceID: workspaceID, RequestIds: requestIDs})
		if err != nil {
			return snapshot, err
		}
	}
	agentID := pgtype.UUID{}
	switch snapshot.issue.AssigneeType.String {
	case "agent":
		agentID = snapshot.issue.AssigneeID
	case "squad":
		squad, err := queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: snapshot.issue.AssigneeID, WorkspaceID: workspaceID})
		if err != nil {
			return snapshot, err
		}
		agentID, snapshot.squadID = squad.LeaderID, squad.ID
	}
	if kind == "" || kind == "retraction" {
		askingID := request.AskingAgentID
		if request.Origin == "unprompted" && len(snapshot.answers) == 1 {
			quote, err := queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: workspaceID, ID: snapshot.answers[0].IssueCommentID})
			if err != nil {
				return snapshot, err
			}
			askingID = pgtype.UUID{}
			if quote.ParentID.Valid {
				parent, err := queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: workspaceID, ID: quote.ParentID})
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return snapshot, err
				}
				if err == nil && parent.IssueID == request.IssueID && !parent.DeletedAt.Valid && parent.AuthorType == "agent" {
					askingID = parent.AuthorID
				}
			}
		}
		// A need_decision issue belongs to its assignee; the asking agent (often
		// the routing agent that posted the card) must not take the wake.
		ownedByAssignee, assigneeDenied := false, false
		if snapshot.issue.Status == "need_decision" && agentID.Valid {
			if ownedByAssignee, err = handler.personMayInvoke(ctx, queries, agentID, request.HumanUserID, workspaceID); err != nil {
				return snapshot, err
			}
			assigneeDenied = !ownedByAssignee
		}
		if askingID.Valid && !ownedByAssignee {
			asking, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: workspaceID, ID: askingID})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return snapshot, err
			}
			if err == nil && !asking.ArchivedAt.Valid && asking.RuntimeID.Valid && asking.Kind != "system" {
				if asking.ID != agentID {
					snapshot.squadID = pgtype.UUID{}
				}
				agentID, assigneeDenied = asking.ID, false
			}
		}
		if assigneeDenied {
			// Neither the owner nor the asker can take the wake: nobody runs.
			agentID, snapshot.squadID = pgtype.UUID{}, pgtype.UUID{}
		}
	}
	err = handler.loadDecisionRecipient(ctx, queries, &snapshot, agentID)
	return snapshot, err
}

// personMayInvoke reports whether the decision's person could run the agent
// themselves. A wake that moves from the asking agent to the issue owner must
// not give the person a run on an agent they cannot invoke. A missing agent is
// not invocable. A failed lookup is an error, never a denial or an approval;
// it reads through queries, so a caller in a transaction stays on its connection.
func (handler *Handler) personMayInvoke(ctx context.Context, queries *db.Queries, agentID, humanID, workspaceID pgtype.UUID) (bool, error) {
	agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: workspaceID, ID: agentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return handler.invokeAgentDecisionVia(ctx, queries, agent, "member", uuidToString(humanID), "", uuidToString(workspaceID))
}

// loadDecisionRecipient reads the recipient, its active task on the issue and
// the review head the dispatch targets. An invalid agentID leaves them empty.
func (handler *Handler) loadDecisionRecipient(ctx context.Context, queries *db.Queries, snapshot *decisionWakeSnapshot, agentID pgtype.UUID) error {
	if !agentID.Valid {
		return nil
	}
	var err error
	workspaceID := snapshot.issue.WorkspaceID
	snapshot.recipient, err = queries.GetAgent(ctx, agentID)
	if err != nil || snapshot.recipient.WorkspaceID != workspaceID {
		if err == nil {
			err = pgx.ErrNoRows
		}
		return err
	}
	tasks, err := queries.ListActiveTasksByIssue(ctx, snapshot.issue.ID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.AgentID == agentID && (!snapshot.task.ID.Valid || (task.Status == "queued" && snapshot.task.Status != "queued")) {
			snapshot.task = task
		}
	}
	head, err := queries.GetIssueReviewHeadSha(ctx, snapshot.issue.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	snapshot.head = pgtype.Text{String: head, Valid: head != ""}
	return nil
}

func decisionWakeReady(snapshot decisionWakeSnapshot, now time.Time) bool {
	if snapshot.kind == "reask" {
		request := snapshot.request
		return request.Status == "cancelled" && (request.CancelReason.String == "human_removed" || request.CancelReason.String == "lane_archived") && request.CancelNoticePostedAt.Valid && !request.ReaskDeliveredAt.Valid
	}
	if snapshot.kind == "retraction" {
		answer := snapshot.answers[0]
		return answer.DisclaimedAt.Valid && answer.WokenAt.Valid && answer.WakeOutcome.Valid && answer.WakeOutcome.String != "skipped_ineligible" && answer.WithdrawalCommentID.Valid && !answer.RetractionDeliveredAt.Valid
	}
	if len(snapshot.answers) == 0 || (snapshot.group && !snapshot.intent.SealedAt.Valid) {
		return false
	}
	ready := make(map[pgtype.UUID]bool, len(snapshot.answers))
	for _, answer := range snapshot.answers {
		if answer.DisclaimedAt.Valid || answer.WokenAt.Valid || !answer.WakeAt.Valid || answer.WakeAt.Time.After(now) {
			return false
		}
		ready[answer.DecisionRequestID] = true
	}
	for _, request := range snapshot.requests {
		if request.Status != "cancelled" && (request.Status != "answered" || !ready[request.ID]) {
			return false
		}
	}
	return true
}

func decisionWakeIDs(values []pgtype.UUID) []pgtype.UUID {
	sort.Slice(values, func(left, right int) bool { return uuidToString(values[left]) < uuidToString(values[right]) })
	unique := values[:0]
	for _, value := range values {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	return unique
}

func (handler *Handler) deliverDecisionWake(ctx context.Context, snapshot decisionWakeSnapshot) (bool, error) {
	if snapshot.kind == "episode" {
		return handler.deliverDecisionEpisode(ctx, snapshot)
	}
	if snapshot.binding.ID.Valid {
		// A bound request belongs to its episode (design §4.4.1).
		return false, nil
	}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	workspaceID := snapshot.request.WorkspaceID
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
	if snapshot.recipient.ID.Valid && snapshot.recipient.ID != snapshot.request.AskingAgentID {
		if _, err := queries.LockDecisionRecipientAgent(ctx, db.LockDecisionRecipientAgentParams{WorkspaceID: workspaceID, ID: snapshot.recipient.ID}); errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	if _, err := queries.LockDecisionAskingAgent(ctx, db.LockDecisionAskingAgentParams{WorkspaceID: workspaceID, ID: snapshot.request.AskingAgentID}); err != nil {
		return false, err
	}
	var root *db.Comment
	if !snapshot.recipient.ID.Valid && snapshot.kind == "" {
		root, err = commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, workspaceID, snapshot.request.IssueCommentID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: snapshot.issue.ID}); err != nil {
		return false, err
	}
	if snapshot.task.ID.Valid {
		if _, err := queries.LockDecisionDeliveryTask(ctx, db.LockDecisionDeliveryTaskParams{WorkspaceID: workspaceID, ID: snapshot.task.ID}); errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	eligible := true
	var humans, sources []pgtype.UUID
	if snapshot.kind == "" {
		for _, request := range snapshot.requests {
			if request.Status != "cancelled" {
				humans = append(humans, request.HumanUserID)
			}
		}
	}
	for _, humanID := range decisionWakeIDs(humans) {
		if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: workspaceID, UserID: humanID}); errors.Is(err, pgx.ErrNoRows) {
			eligible = false
		} else if err != nil {
			return false, err
		}
	}
	if snapshot.kind == "" && eligible {
		for _, request := range snapshot.requests {
			if request.Status == "cancelled" {
				continue
			}
			var refusal decisionIntentRefusal
			if err := requireRequestAuthority(ctx, queries, snapshot.issue, request, http.StatusConflict); errors.As(err, &refusal) {
				eligible = false
			} else if err != nil {
				return false, err
			}
		}
	}
	if snapshot.kind == "" {
		for _, answer := range snapshot.answers {
			sources = append(sources, answer.ChatMessageIds...)
		}
	}
	sources = decisionWakeIDs(sources)
	locked, err := queries.LockDecisionSourceMessages(ctx, db.LockDecisionSourceMessagesParams{WorkspaceID: workspaceID, MessageIds: sources})
	if err != nil || len(locked) != len(sources) {
		return false, err
	}
	if snapshot.group {
		if _, err := queries.LockDecisionIntent(ctx, db.LockDecisionIntentParams{WorkspaceID: workspaceID, ID: snapshot.intent.ID}); err != nil {
			return false, err
		}
		if _, err := queries.LockDecisionDeliveryTargets(ctx, db.LockDecisionDeliveryTargetsParams{WorkspaceID: workspaceID, ID: snapshot.intent.ID}); err != nil {
			return false, err
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
	current, err := handler.loadDecisionItem(ctx, queries, workspaceID, snapshot.itemID, snapshot.kind)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(current, snapshot) {
		return true, nil
	}
	if snapshot.kind == "" {
		if refused, err := handler.refuseGatedAgentRecords(ctx, transaction, queries, snapshot); err != nil || refused {
			return false, err
		}
	}
	if snapshot.group && !snapshot.intent.SealedAt.Valid {
		if _, err := decisiontx.SealInvalidIntent(ctx, queries, snapshot.intent); err != nil {
			return false, err
		}
		snapshot.intent, err = queries.GetDecisionIntent(ctx, db.GetDecisionIntentParams{WorkspaceID: workspaceID, ID: snapshot.intent.ID})
		if err != nil {
			return false, err
		}
	}
	now, err := queries.DecisionDeliveryClock(ctx)
	if err != nil {
		return false, err
	}
	if !decisionWakeReady(snapshot, now.Time) {
		return false, nil
	}
	asking, err := queries.GetAgent(ctx, snapshot.request.AskingAgentID)
	if err != nil {
		return false, err
	}
	var unparked *db.Issue
	var unparkedActivity *db.ActivityLog
	if snapshot.kind == "" {
		if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: snapshot.issue.ID}); err != nil {
			return false, err
		}
		locked, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: snapshot.issue.ID})
		if err != nil {
			return false, err
		}
		if restored, recorded, err := unparkIssueAfterDecision(ctx, queries, locked, snapshot.answers); err != nil {
			return false, err
		} else if recorded != nil {
			unparked, unparkedActivity = &restored, recorded
		}
	}
	switch snapshot.issue.Status {
	case "done", "cancelled", "agent_accepted", "approved":
		eligible = false
	}
	if eligible && snapshot.kind == "" && len(snapshot.requests) > 0 {
		// An executed or declined action already did its effect; its platform
		// comment starts no run (CHE-5287). A failed action did not, so the
		// asker still hears of it. One plain request keeps the wake. An agent
		// proposal is the exception: the proposer hears every terminal state
		// (CHE-5411).
		settled := true
		for _, request := range snapshot.requests {
			if request.ProposalFingerprint.Valid {
				settled = false
				continue
			}
			action, err := queries.GetDecisionActionByRequest(ctx, db.GetDecisionActionByRequestParams{DecisionRequestID: request.ID, WorkspaceID: workspaceID})
			if errors.Is(err, pgx.ErrNoRows) {
				settled = false
			} else if err != nil {
				return false, err
			} else if action.State != "executed" && action.State != "rejected" {
				settled = false
			}
		}
		eligible = !settled
	}
	eligible = eligible && (!asking.ArchivedAt.Valid || (snapshot.kind == "" || snapshot.kind == "retraction") && snapshot.recipient.ID.Valid && !snapshot.recipient.ArchivedAt.Valid)
	if eligible && snapshot.recipient.ID.Valid && isServerDecision(snapshot.request) {
		// The platform picked this asker, not the person, so the answer runs the
		// agent only when that person could have invoked it themselves.
		eligible = handler.canInvokeAgent(ctx, snapshot.recipient, "member", uuidToString(snapshot.request.HumanUserID), "", uuidToString(workspaceID))
	}
	outcome := "skipped_ineligible"
	var queued db.AgentTaskQueue
	var comments, cleared []db.Comment
	if eligible {
		if snapshot.kind == "" {
			for _, answer := range snapshot.answers {
				if err := captureDecisionAnswerWakeup(ctx, transaction, snapshot.issue.ID, answer.ID, workspaceID); err != nil {
					return false, err
				}
			}
		}
		if snapshot.recipient.ID.Valid {
			if !snapshot.recipient.RuntimeID.Valid || snapshot.recipient.ArchivedAt.Valid {
				return false, nil
			}
			outcome, queued, err = handler.dispatchDecisionWake(ctx, queries, snapshot)
			if err != nil || outcome == "" {
				return false, err
			}
		} else {
			outcome = "comment_unassigned"
			if snapshot.issue.AssigneeType.String == "member" {
				outcome = "comment_member"
			}
			if snapshot.kind == "" {
				for _, answer := range snapshot.answers {
					created, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: dbid.NewV7(), WorkspaceID: workspaceID, IssueID: snapshot.issue.ID, ParentID: answer.IssueCommentID, AuthorType: "agent", AuthorID: snapshot.request.AskingAgentID, Type: "comment", Content: "Decision took effect.", DecisionAnswerID: answer.ID, DecisionCommentKind: pgtype.Text{String: "delivery", Valid: true}})
					if err != nil {
						return false, err
					}
					comments = append(comments, created.Comment())
				}
			}
			if root != nil {
				cleared, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{WorkspaceID: workspaceID, IssueID: snapshot.issue.ID, ThreadRootID: root.ID})
				if err != nil {
					return false, err
				}
			}
		}
	}
	if snapshot.kind == "reask" {
		rows, err := queries.AcknowledgeDecisionReask(ctx, db.AcknowledgeDecisionReaskParams{WorkspaceID: workspaceID, ID: snapshot.request.ID, Outcome: pgtype.Text{String: outcome, Valid: true}})
		if err != nil {
			return false, err
		}
		if rows != 1 {
			return false, errors.New("decision re-ask acknowledgement changed")
		}
	}
	for _, answer := range snapshot.answers {
		var rows int64
		var err error
		if snapshot.kind == "retraction" {
			rows, err = queries.AcknowledgeDecisionRetraction(ctx, db.AcknowledgeDecisionRetractionParams{WorkspaceID: workspaceID, ID: answer.ID, Outcome: pgtype.Text{String: outcome, Valid: true}})
		} else {
			rows, err = queries.AcknowledgeDecisionWake(ctx, db.AcknowledgeDecisionWakeParams{WorkspaceID: workspaceID, ID: answer.ID, Outcome: pgtype.Text{String: outcome, Valid: true}})
		}
		if err != nil {
			return false, err
		}
		if rows != 1 {
			return false, errors.New("decision wake acknowledgement changed")
		}
	}
	if snapshot.group {
		rows, err := queries.AcknowledgeDecisionGroupWake(ctx, db.AcknowledgeDecisionGroupWakeParams{WorkspaceID: workspaceID, ID: snapshot.intent.ID})
		if err != nil {
			return false, err
		}
		if rows != 1 {
			return false, errors.New("decision group acknowledgement changed")
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, err
	}
	if unparked != nil {
		handler.publishDecisionStatusChange(ctx, snapshot.issue, *unparked, unparkedActivity, "decision_answer")
	}
	if queued.ID.Valid {
		handler.TaskService.PublishTaskEnqueued(ctx, queued)
	}
	for _, comment := range comments {
		handler.publish(protocol.EventCommentCreated, uuidToString(workspaceID), "agent", uuidToString(snapshot.request.AskingAgentID), map[string]any{"comment": commentToResponse(comment, nil, nil)})
	}
	handler.TaskService.PublishThreadUnresolvedOnReply(cleared, uuidToString(workspaceID), "agent", uuidToString(snapshot.request.AskingAgentID))
	if snapshot.kind == "" {
		for _, answer := range snapshot.answers {
			handler.publishDecisionAnswerUpdated(uuidToString(workspaceID), "agent", uuidToString(snapshot.request.AskingAgentID), answer.ID, snapshot.issue.ID, decisionStateEffective)
		}
	}
	return false, nil
}

func (handler *Handler) dispatchDecisionWake(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot) (string, db.AgentTaskQueue, error) {
	var empty db.AgentTaskQueue
	var commentIDs []pgtype.UUID
	var summary strings.Builder
	switch snapshot.kind {
	case "retraction":
		summary.WriteString("Decision withdrawn. Do not rely on the withdrawn quote.\n")
	case "reask":
		summary.WriteString("Decision request cancelled. Ask again through a new decision-intent window and a fresh lane listing. No automatic re-ping or server-created DM.\n")
	default:
		summary.WriteString("Decision took effect. Cover every quote in this delivery:\n")
	}
	var trigger db.Comment
	if snapshot.kind == "reask" {
		var err error
		trigger, err = queries.GetDecisionCancelComment(ctx, db.GetDecisionCancelCommentParams{WorkspaceID: snapshot.issue.WorkspaceID, ID: snapshot.request.ID})
		if err != nil {
			return "", empty, err
		}
		commentIDs = append(commentIDs, trigger.ID)
	}
	for _, answer := range snapshot.answers {
		commentID := answer.IssueCommentID
		if snapshot.kind == "retraction" {
			commentID = answer.WithdrawalCommentID
		}
		quote, err := queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: snapshot.issue.WorkspaceID, ID: commentID})
		if err != nil {
			return "", empty, err
		}
		commentIDs = append(commentIDs, quote.ID)
		if snapshot.kind == "retraction" {
			fmt.Fprintf(&summary, "- Withdrawal comment %s (request %s, reason %s)\n", uuidToString(quote.ID), uuidToString(answer.DecisionRequestID), answer.DisclaimReason.String)
		} else {
			fmt.Fprintf(&summary, "- Quote comment %s (request %s)\n", uuidToString(quote.ID), uuidToString(answer.DecisionRequestID))
		}
		if !trigger.ID.Valid || quote.CreatedAt.Time.After(trigger.CreatedAt.Time) || (quote.CreatedAt.Time.Equal(trigger.CreatedAt.Time) && uuidToString(quote.ID) > uuidToString(trigger.ID)) {
			trigger = quote
		}
	}
	for _, request := range snapshot.requests {
		if request.Status == "cancelled" {
			fmt.Fprintf(&summary, "- Cancelled request %s: %s\n", uuidToString(request.ID), request.CancelReason.String)
		}
	}
	for _, target := range snapshot.targets {
		if !target.ConsumedAt.Valid {
			fmt.Fprintf(&summary, "- Target not asked: user %s\n", uuidToString(target.HumanUserID))
		}
	}
	return handler.dispatchDecisionInputs(ctx, queries, snapshot, trigger, commentIDs, summary.String())
}

// dispatchDecisionInputs is the one place a decision delivery reaches the
// recipient: a new task, a merge into its queued task, or planned input for its
// running task. An empty outcome is a refusal.
func (handler *Handler) dispatchDecisionInputs(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot, trigger db.Comment, commentIDs []pgtype.UUID, summary string) (string, db.AgentTaskQueue, error) {
	var empty db.AgentTaskQueue
	if !snapshot.task.ID.Valid {
		task, err := handler.TaskService.EnqueueDecisionTaskInTx(ctx, queries, snapshot.issue, snapshot.recipient.ID, snapshot.squadID, trigger.ID, commentIDs, summary)
		return "run_enqueued", task, err
	}
	var taskID pgtype.UUID
	outcome := ""
	if snapshot.task.SquadID != snapshot.squadID || snapshot.task.IsLeaderTask != snapshot.squadID.Valid {
		return "", empty, nil
	}
	if snapshot.task.Status == "queued" {
		params, _, err := handler.commentMergeParams(ctx, snapshot.issue, commentAgentTrigger{Agent: snapshot.recipient, Source: commentTriggerSourceIssueAssignee}, trigger.ID, snapshot.head)
		if err != nil {
			return "", empty, err
		}
		row, err := queries.MergeCommentIntoQueuedIssueTask(ctx, db.MergeCommentIntoQueuedIssueTaskParams{TargetTaskID: snapshot.task.ID, IssueID: snapshot.issue.ID, AgentID: snapshot.recipient.ID, SquadID: snapshot.squadID, HeadSha: snapshot.head, NewTriggerCommentID: params.NewTriggerCommentID, NewTriggerSummary: params.NewTriggerSummary, NewOriginatorUserID: params.NewOriginatorUserID, NewAccountableUserID: params.NewAccountableUserID, NewOriginatorSource: params.NewOriginatorSource, NewDelegatedFromTaskID: params.NewDelegatedFromTaskID, NewRuleVersionID: params.NewRuleVersionID, NewTriggerEvidenceKind: params.NewTriggerEvidenceKind, NewTriggerEvidenceRefID: params.NewTriggerEvidenceRefID, NewRuntimeMcpOverlay: params.NewRuntimeMcpOverlay, NewRuntimeConnectedApps: params.NewRuntimeConnectedApps})
		if errors.Is(err, pgx.ErrNoRows) {
			return "", empty, nil
		}
		if err != nil {
			return "", empty, err
		}
		taskID, outcome = row.ID, "merged_into_queued"
	} else {
		row, err := queries.RegisterPlannedCommentForActiveTask(ctx, db.RegisterPlannedCommentForActiveTaskParams{TargetTaskID: snapshot.task.ID, IssueID: snapshot.issue.ID, AgentID: snapshot.recipient.ID, CommentID: trigger.ID, HeadSha: snapshot.head})
		if errors.Is(err, pgx.ErrNoRows) {
			return "", empty, nil
		}
		if err != nil {
			return "", empty, err
		}
		taskID, outcome = row.ID, "planned_input"
	}
	if taskID != snapshot.task.ID {
		return "", empty, errors.New("decision dispatch target changed")
	}
	rows, err := queries.AppendDecisionDeliveryInputs(ctx, db.AppendDecisionDeliveryInputsParams{WorkspaceID: snapshot.issue.WorkspaceID, ID: taskID, CommentIds: commentIDs, Summary: summary})
	if err == nil && rows != 1 {
		err = errors.New("decision input target changed")
	}
	return outcome, empty, err
}

// captureDecisionAnswerWakeup records the answer comment as a comment.created
// wakeup of the issue, as an ordinary answer wake does.
func captureDecisionAnswerWakeup(ctx context.Context, transaction pgx.Tx, issueID, answerID, workspaceID pgtype.UUID) error {
	_, err := transaction.Exec(ctx, `SELECT capture_issue_wakeup($1, 'comment.created', c.id::text, c.author_id, c.source_task_id,
		jsonb_build_object('comment_id', c.id, 'author_type', c.author_type, 'author_id', c.author_id,
		'actor_type', 'member', 'actor_id', r.human_user_id, 'decision_answer_id', a.id))
		FROM decision_answer a JOIN decision_request r ON r.id = a.decision_request_id
		JOIN comment c ON c.id = a.issue_comment_id
		WHERE r.authority = 'human' AND a.id = $2 AND r.workspace_id = $3 AND r.issue_id = $1 AND c.issue_id = $1
		AND a.disclaimed_at IS NULL AND c.deleted_at IS NULL`, issueID, answerID, workspaceID)
	return err
}

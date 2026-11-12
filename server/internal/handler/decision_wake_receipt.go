package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type decisionWakeRecipient struct {
	agentID pgtype.UUID
	role    string
	squadID pgtype.UUID
}

// resolveDecisionWakeRecipients returns the requester, plus the issue
// assignee (an agent, or a squad's leader with the squad id) when that is a
// different agent. Eligibility is checked at delivery time, not here.
func (handler *Handler) resolveDecisionWakeRecipients(ctx context.Context, queries *db.Queries, decision db.DecisionRequest) ([]decisionWakeRecipient, error) {
	recipients := []decisionWakeRecipient{{agentID: decision.AskingAgentID, role: "requester"}}
	issue, err := queries.GetIssue(ctx, decision.IssueID)
	if err != nil {
		return nil, err
	}
	assignee := decisionWakeRecipient{role: "assignee"}
	switch issue.AssigneeType.String {
	case "agent":
		assignee.agentID = issue.AssigneeID
	case "squad":
		squad, err := queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: decision.WorkspaceID})
		if err != nil {
			return nil, err
		}
		assignee.agentID, assignee.squadID = squad.LeaderID, squad.ID
	}
	if assignee.agentID.Valid && assignee.agentID != decision.AskingAgentID {
		if issue.Status == "need_decision" {
			// The assignee owns the wake only when the person could invoke it.
			if invocable, err := handler.personMayInvoke(ctx, queries, assignee.agentID, decision.HumanUserID, decision.WorkspaceID); err != nil {
				return nil, err
			} else if invocable {
				return []decisionWakeRecipient{assignee}, nil
			}
			return recipients, nil
		}
		recipients = append(recipients, assignee)
	}
	return recipients, nil
}

// recordDecisionWakeEvent writes one wake event and a receipt per recipient.
// Call it after the writer has locked the request row (any UPDATE of the
// request does), so wake_seq order equals commit order.
func (handler *Handler) recordDecisionWakeEvent(ctx context.Context, queries *db.Queries, decision db.DecisionRequest, eventType string, answerID, sourceCommentID pgtype.UUID) error {
	recipients, err := handler.resolveDecisionWakeRecipients(ctx, queries, decision)
	if err != nil {
		return err
	}
	seq, err := queries.AllocateDecisionWakeSeq(ctx, decision.ID)
	if err != nil {
		return err
	}
	event, err := queries.InsertDecisionWakeEvent(ctx, db.InsertDecisionWakeEventParams{ID: dbid.NewV7(), RequestID: decision.ID, Seq: seq, Type: eventType, AnswerID: answerID, SourceCommentID: sourceCommentID})
	if err != nil {
		return err
	}
	for _, recipient := range recipients {
		if _, err := queries.InsertDecisionWakeReceipt(ctx, db.InsertDecisionWakeReceiptParams{ID: dbid.NewV7(), EventID: event.ID, EventSeq: seq, RecipientAgentID: recipient.agentID, Role: recipient.role, SquadID: recipient.squadID}); err != nil {
			return err
		}
	}
	return nil
}

// markDecisionRequestAnswered is the one place a request becomes answered.
// kind=action also gets its durable answered event; kind=decision keeps the
// legacy answer-row wake.
func (handler *Handler) markDecisionRequestAnswered(ctx context.Context, queries *db.Queries, decision db.DecisionRequest, answer db.DecisionAnswer) (db.DecisionRequest, error) {
	marked, err := queries.MarkDecisionRequestAnswered(ctx, db.MarkDecisionRequestAnsweredParams{WorkspaceID: decision.WorkspaceID, ID: decision.ID})
	if err != nil || marked.Kind != "action" {
		return marked, err
	}
	return marked, handler.recordDecisionWakeEvent(ctx, queries, marked, "answered", answer.ID, answer.IssueCommentID)
}

// decisionReceiptState is everything a receipt delivery reads before it locks;
// it is reloaded under the locks and the delivery restarts if it moved.
type decisionReceiptState struct {
	receipt   db.DecisionWakeReceipt
	event     db.DecisionWakeEvent
	request   db.DecisionRequest
	issue     db.Issue
	recipient db.Agent
	task      db.AgentTaskQueue
	head      pgtype.Text
}

func (handler *Handler) loadDecisionReceiptState(ctx context.Context, queries *db.Queries, receiptID pgtype.UUID) (decisionReceiptState, error) {
	var state decisionReceiptState
	var err error
	if state.receipt, err = queries.GetDecisionWakeReceipt(ctx, receiptID); err != nil {
		return state, err
	}
	if state.event, err = queries.GetDecisionWakeEvent(ctx, state.receipt.EventID); err != nil {
		return state, err
	}
	if state.request, err = queries.GetDecisionRequestForWake(ctx, state.event.RequestID); err != nil {
		return state, err
	}
	if state.issue, err = queries.GetIssue(ctx, state.request.IssueID); err != nil {
		return state, err
	}
	snapshot := decisionWakeSnapshot{issue: state.issue}
	err = handler.loadDecisionRecipient(ctx, queries, &snapshot, state.receipt.RecipientAgentID)
	state.recipient, state.task, state.head = snapshot.recipient, snapshot.task, snapshot.head
	return state, err
}

// deliverDecisionWakeReceipt delivers one pending receipt in one transaction:
// the dispatch and the receipt's terminal status commit together. Locks follow
// the order of deliverDecisionWake (runtime, recipient, asking agent, issue,
// task, request), then the answer, the event and the receipt.
func (handler *Handler) deliverDecisionWakeReceipt(ctx context.Context, receiptID pgtype.UUID) error {
	for attempt := 0; attempt < 3; attempt++ {
		restart, err := handler.tryDeliverDecisionWakeReceipt(ctx, receiptID)
		if err != nil || !restart {
			return err
		}
	}
	return errors.New("decision wake receipt state kept changing")
}

func (handler *Handler) tryDeliverDecisionWakeReceipt(ctx context.Context, receiptID pgtype.UUID) (restart bool, err error) {
	state, err := handler.loadDecisionReceiptState(ctx, handler.Queries, receiptID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && state.receipt.Status != "pending" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	workspaceID := state.request.WorkspaceID
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return false, err
	}
	if state.recipient.RuntimeID.Valid {
		if _, err := queries.LockDecisionRecipientRuntime(ctx, db.LockDecisionRecipientRuntimeParams{WorkspaceID: workspaceID, ID: state.recipient.RuntimeID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	if state.recipient.ID != state.request.AskingAgentID {
		if _, err := queries.LockDecisionReceiptRecipient(ctx, db.LockDecisionReceiptRecipientParams{WorkspaceID: workspaceID, ID: state.recipient.ID}); err != nil {
			return false, err
		}
	}
	if _, err := queries.LockDecisionAskingAgent(ctx, db.LockDecisionAskingAgentParams{WorkspaceID: workspaceID, ID: state.request.AskingAgentID}); err != nil {
		return false, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: state.issue.ID}); err != nil {
		return false, err
	}
	if state.task.ID.Valid {
		if _, err := queries.LockDecisionDeliveryTask(ctx, db.LockDecisionDeliveryTaskParams{WorkspaceID: workspaceID, ID: state.task.ID}); errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	if _, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: state.request.ID}); err != nil {
		return false, err
	}
	if state.event.AnswerID.Valid {
		if _, err := queries.LockDecisionAnswerForWake(ctx, state.event.AnswerID); err != nil {
			return false, err
		}
	}
	if _, err := queries.LockDecisionWakeEvent(ctx, state.event.ID); err != nil {
		return false, err
	}
	if _, err := queries.LockDecisionWakeReceipt(ctx, receiptID); err != nil {
		return false, err
	}
	current, err := handler.loadDecisionReceiptState(ctx, queries, receiptID)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(current, state) {
		return true, nil
	}
	// The request lock is held, so no event can be written while these checks run.
	blocked, err := queries.HasOlderPendingDecisionWakeReceipt(ctx, db.HasOlderPendingDecisionWakeReceiptParams{RequestID: state.request.ID, RecipientAgentID: state.recipient.ID, EventSeq: state.receipt.EventSeq})
	if err != nil || blocked {
		return false, err
	}
	verdict, held, err := decisionReceiptVerdict(ctx, queries, state)
	if err != nil || held {
		return false, err
	}
	status, outcome := "skipped_ineligible", pgtype.Text{}
	if verdict != "" {
		status = verdict
	}
	if verdict == "" && state.receipt.Role == "assignee" && state.issue.Status == "need_decision" {
		// Permission can change after the receipt was written: the person must
		// still be able to invoke the assignee when it dispatches.
		invocable, err := handler.personMayInvoke(ctx, queries, state.recipient.ID, state.request.HumanUserID, workspaceID)
		if err != nil {
			return false, err
		}
		if !invocable {
			verdict = "skipped_ineligible"
		}
	}
	var enqueued db.AgentTaskQueue
	if verdict == "" && !state.recipient.ArchivedAt.Valid && state.recipient.RuntimeID.Valid && state.recipient.Kind != "system" {
		trigger, err := queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: workspaceID, ID: state.event.SourceCommentID})
		if err != nil {
			return false, err
		}
		dispatched, queued, err := handler.dispatchDecisionInputs(ctx, queries,
			decisionWakeSnapshot{issue: state.issue, recipient: state.recipient, squadID: state.receipt.SquadID, task: state.task, head: state.head},
			trigger, []pgtype.UUID{trigger.ID},
			fmt.Sprintf("Human action request %s: %s.\n- Source comment %s\n", uuidToString(state.request.ID), state.event.Type, uuidToString(trigger.ID)))
		if err != nil {
			return false, err
		}
		if dispatched == "" {
			return false, errors.New("decision dispatch produced no outcome")
		}
		status, outcome = "delivered", pgtype.Text{String: dispatched, Valid: true}
		enqueued = queued
	}
	rows, err := queries.SettleDecisionWakeReceipt(ctx, db.SettleDecisionWakeReceiptParams{ID: receiptID, Status: status, Outcome: outcome})
	if err != nil {
		return false, err
	}
	if rows != 1 {
		return false, errors.New("decision wake receipt changed")
	}
	if err := acknowledgeDecisionWakeEvent(ctx, queries, state); err != nil {
		return false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, err
	}
	if enqueued.ID.Valid {
		handler.TaskService.PublishTaskEnqueued(ctx, enqueued)
	}
	return false, nil
}

// decisionEventCancelReasons are the cancel reasons that make a non-answer
// event valid: a cancelled request is the expected state for these events.
var decisionEventCancelReasons = map[string][]string{
	"expired":            {"expired"},
	"withdrawn_human":    {"agent_withdrawn"},
	"withdrawn_agent":    {"agent_withdrawn"},
	"reask":              {"human_removed", "lane_archived"},
	"requester_archived": {"agent_archived"},
}

// decisionReceiptVerdict runs delivery-time checks 1 to 3 on a state reloaded
// under the locks. A non-empty verdict is the receipt's terminal status and
// nothing dispatches; held means the answer's wake_at is still ahead, so the
// receipt stays pending without an attempt.
func decisionReceiptVerdict(ctx context.Context, queries *db.Queries, state decisionReceiptState) (verdict string, held bool, err error) {
	switch state.issue.Status {
	case "done", "cancelled", "agent_accepted", "approved":
		return "suppressed_issue_closed", false, nil
	}
	if state.event.SupersededAt.Valid {
		return "superseded", false, nil
	}
	if !state.event.AnswerID.Valid {
		if slices.Contains(decisionEventCancelReasons[state.event.Type], state.request.CancelReason.String) && state.request.Status == "cancelled" {
			return "", false, nil
		}
		return "superseded", false, nil
	}
	answer, err := queries.GetDecisionAnswerWakeState(ctx, state.event.AnswerID)
	if err != nil {
		return "", false, err
	}
	if state.event.Type == "retracted" {
		if answer.Disclaimed {
			return "", false, nil
		}
		return "superseded", false, nil
	}
	if state.request.Status != "answered" || answer.Disclaimed || !answer.LatestLive {
		return "superseded", false, nil
	}
	return "", answer.Held, nil
}

// acknowledgeDecisionWakeEvent sets the answer's woken_at and wake_outcome once
// the last receipt of its event has settled. The answer row lock taken before
// the settle makes the last settler see every other receipt as committed.
func acknowledgeDecisionWakeEvent(ctx context.Context, queries *db.Queries, state decisionReceiptState) error {
	if !state.event.AnswerID.Valid {
		return nil
	}
	summary, err := queries.SummarizeDecisionWakeEvent(ctx, state.event.ID)
	if err != nil || summary.Pending > 0 {
		return err
	}
	// Zero rows: the answer was already woken or has been withdrawn.
	_, err = queries.AcknowledgeDecisionWake(ctx, db.AcknowledgeDecisionWakeParams{WorkspaceID: state.request.WorkspaceID, ID: state.event.AnswerID, Outcome: pgtype.Text{String: summary.Outcome, Valid: true}})
	return err
}

// deliverDecisionWakeReceipts is the worker stage: every due pending receipt is
// delivered in its own transaction. A failure is recorded on that receipt with
// a backoff and does not stop the others.
func (handler *Handler) deliverDecisionWakeReceipts(ctx context.Context) error {
	ids, err := handler.Queries.ListDecisionWakeReceiptCandidates(ctx, decisionBatchSize)
	if err != nil {
		return err
	}
	var failures error
	for _, id := range ids {
		err := decisionAttempt(ctx, func() error { return handler.deliverDecisionWakeReceipt(ctx, id) })
		if err == nil || ctx.Err() != nil {
			continue
		}
		slog.Warn("decision wake receipt delivery failed", "receipt_id", uuidToString(id), "error", err)
		if _, recordErr := handler.Queries.RecordDecisionWakeReceiptFailure(ctx, db.RecordDecisionWakeReceiptFailureParams{ID: id, LastError: pgtype.Text{String: err.Error(), Valid: true}}); recordErr != nil {
			failures = errors.Join(failures, recordErr)
		}
	}
	return failures
}

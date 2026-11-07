package handler

import (
	"context"

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
func resolveDecisionWakeRecipients(ctx context.Context, queries *db.Queries, decision db.DecisionRequest) ([]decisionWakeRecipient, error) {
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
		recipients = append(recipients, assignee)
	}
	return recipients, nil
}

// recordDecisionWakeEvent writes one wake event and a receipt per recipient.
// Call it after the writer has locked the request row (any UPDATE of the
// request does), so wake_seq order equals commit order.
func recordDecisionWakeEvent(ctx context.Context, queries *db.Queries, decision db.DecisionRequest, eventType string, answerID, sourceCommentID pgtype.UUID) error {
	recipients, err := resolveDecisionWakeRecipients(ctx, queries, decision)
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
func markDecisionRequestAnswered(ctx context.Context, queries *db.Queries, decision db.DecisionRequest, answer db.DecisionAnswer) (db.DecisionRequest, error) {
	marked, err := queries.MarkDecisionRequestAnswered(ctx, db.MarkDecisionRequestAnsweredParams{WorkspaceID: decision.WorkspaceID, ID: decision.ID})
	if err != nil || marked.Kind != "action" {
		return marked, err
	}
	return marked, recordDecisionWakeEvent(ctx, queries, marked, "answered", answer.ID, answer.IssueCommentID)
}

package decisiontx

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// ClassNeedsOwner is the reason for a decision that only the workspace owner may
// make. It cancels the request and never reopens it.
const ClassNeedsOwner = "class_needs_owner"

// OwnerRequiredNotice tells the asking agent where a refused decision goes next.
const OwnerRequiredNotice = "This decision concerns the workspace owner. Post a fresh --decision-intent that names the owner, then ping in a new request."

func withdrawalContent(reason string) string {
	if reason == ClassNeedsOwner {
		return "Decision withdrawn: " + reason + ". " + OwnerRequiredNotice
	}
	return "Decision withdrawn: " + reason + "."
}

type Withdrawal struct {
	Request         db.DecisionRequest
	Answer          db.DecisionAnswer
	Comments        []db.Comment
	ClearedComments []db.Comment
}

func Withdraw(ctx context.Context, queries *db.Queries, request db.DecisionRequest, answer db.DecisionAnswer, sessionStatus, reason string, userID pgtype.UUID) (Withdrawal, error) {
	return withdraw(ctx, queries, request, answer, sessionStatus, reason, userID, nil, false)
}

func WithdrawByAgent(ctx context.Context, queries *db.Queries, request db.DecisionRequest, answer db.DecisionAnswer, sessionStatus, reason string, comment db.Comment, suppress bool) (Withdrawal, error) {
	return withdraw(ctx, queries, request, answer, sessionStatus, reason, pgtype.UUID{}, &comment, suppress)
}

func withdraw(ctx context.Context, queries *db.Queries, request db.DecisionRequest, answer db.DecisionAnswer, sessionStatus, reason string, userID pgtype.UUID, veto *db.Comment, suppress bool) (Withdrawal, error) {
	result := Withdrawal{Request: request, Answer: answer}
	if answer.DecisionRequestID != request.ID {
		return Withdrawal{}, fmt.Errorf("withdrawal answer belongs to another request")
	}
	if answer.DisclaimedAt.Valid {
		return result, nil
	}
	parentID := request.IssueCommentID
	if parentID.Valid {
		if _, err := queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: request.WorkspaceID, ID: parentID}); errors.Is(err, pgx.ErrNoRows) {
			parentID = pgtype.UUID{}
		} else if err != nil {
			return Withdrawal{}, err
		}
	}
	var comment db.Comment
	var err error
	params := db.WithdrawDecisionAnswerParams{WorkspaceID: request.WorkspaceID, ID: answer.ID, DisclaimReason: pgtype.Text{String: reason, Valid: true}, DisclaimedByUserID: userID}
	if veto == nil {
		created, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: dbid.NewV7(), WorkspaceID: request.WorkspaceID, IssueID: request.IssueID, ParentID: parentID, AuthorType: "agent", AuthorID: request.AskingAgentID, Type: "comment", Content: withdrawalContent(reason), DecisionAnswerID: answer.ID, DecisionCommentKind: pgtype.Text{String: "disclaim", Valid: true}})
		if err != nil {
			return Withdrawal{}, err
		}
		comment = created.Comment()
	} else {
		comment = *veto
		reasonRunes := []rune(comment.Content)
		if len(reasonRunes) > 600 {
			reasonRunes = reasonRunes[:600]
		}
		params.DisclaimedByAgentID, params.DisclaimedByTaskID = comment.AuthorID, comment.SourceTaskID
		params.VetoReason = pgtype.Text{String: string(reasonRunes), Valid: true}
		params.SuppressRetraction = suppress && answer.WokenAt.Valid && answer.WakeOutcome.Valid && answer.WakeOutcome.String != "skipped_ineligible"
	}
	result.Comments = append(result.Comments, comment)
	params.WithdrawalCommentID = comment.ID
	result.Answer, err = queries.WithdrawDecisionAnswer(ctx, params)
	if err != nil {
		return Withdrawal{}, err
	}
	if request.Status != "answered" {
		return result, nil
	}
	cancelReason := ""
	if reason == ClassNeedsOwner {
		cancelReason = ClassNeedsOwner
	} else if request.Origin == "unprompted" {
		cancelReason = "record_disclaimed"
		if veto != nil {
			cancelReason = "record_vetoed"
		}
	} else if sessionStatus == "archived" {
		cancelReason = "lane_archived"
	}
	if cancelReason == "" {
		result.Request, err = queries.ReopenDecisionRequest(ctx, db.ReopenDecisionRequestParams{WorkspaceID: request.WorkspaceID, ID: request.ID})
	} else {
		if cancelReason == "lane_archived" {
			cancel, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: dbid.NewV7(), WorkspaceID: request.WorkspaceID, IssueID: request.IssueID, ParentID: parentID, AuthorType: "agent", AuthorID: request.AskingAgentID, Type: "comment", Content: "The decision was withdrawn and its DM is archived. A new decision request is required.", DecisionRequestID: request.ID, DecisionCommentKind: pgtype.Text{String: "cancel", Valid: true}})
			if err != nil {
				return Withdrawal{}, err
			}
			result.Comments = append(result.Comments, cancel.Comment())
		}
		result.Request, err = queries.CancelWithdrawnDecisionRequest(ctx, db.CancelWithdrawnDecisionRequestParams{WorkspaceID: request.WorkspaceID, ID: request.ID, CancelReason: pgtype.Text{String: cancelReason, Valid: true}})
	}
	if err != nil {
		return Withdrawal{}, err
	}
	if cancelReason != "" {
		if _, err := EndAction(ctx, queries, result.Request, cancelReason); err != nil {
			return Withdrawal{}, err
		}
	}
	return result, nil
}

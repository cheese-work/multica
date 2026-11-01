package handler

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/weblink"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// refuseGatedAgentRecords is the delivery policy check: a non-reversible
// request answered by an agent record may not wake anyone. When any answer of
// the item is such a record, it withdraws each one, reopens its request with a
// card link and commits, with no wakeup capture, dispatch or effect comment.
// Permitted members of the same group stay unwoken until the card answers.
func (handler *Handler) refuseGatedAgentRecords(ctx context.Context, transaction pgx.Tx, queries *db.Queries, snapshot decisionWakeSnapshot) (bool, error) {
	var withdrawals []decisiontx.Withdrawal
	for _, answer := range snapshot.answers {
		for _, request := range snapshot.requests {
			if request.ID != answer.DecisionRequestID || request.EscalationClass == "reversible" || answer.AnswerSource != "agent_record" || answer.DisclaimedAt.Valid || answer.WokenAt.Valid {
				continue
			}
			session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: request.WorkspaceID, ID: request.ChatSessionID})
			if err != nil {
				return false, err
			}
			workspace, err := queries.GetWorkspace(ctx, request.WorkspaceID)
			if err != nil {
				return false, err
			}
			link := weblink.Chat(workspace.Slug, uuidToString(session.ID))
			if absolute := weblink.Build(handler.cfg.AppURL, link); absolute != "" {
				link = absolute
			}
			if len(withdrawals) == 0 {
				// The refusal commits, so the withdrawal comment must not capture a wakeup.
				if _, err := transaction.Exec(ctx, `SELECT set_config('multica.skip_comment_wakeup', 'on', true)`); err != nil {
					return false, err
				}
			}
			withdrawal, err := decisiontx.WithdrawForCard(ctx, queries, request, answer, session.Status, link)
			if err != nil {
				return false, err
			}
			withdrawals = append(withdrawals, withdrawal)
		}
	}
	if len(withdrawals) == 0 {
		return false, nil
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, err
	}
	for _, withdrawal := range withdrawals {
		workspaceID, agentID := uuidToString(withdrawal.Request.WorkspaceID), uuidToString(withdrawal.Request.AskingAgentID)
		for _, comment := range withdrawal.Comments {
			handler.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{"comment": commentToResponse(comment, nil, nil)})
		}
		handler.publishDecisionAnswerUpdated(workspaceID, "agent", agentID, withdrawal.Answer.ID, withdrawal.Request.IssueID, decisionStateDisclaimed)
	}
	return true, nil
}

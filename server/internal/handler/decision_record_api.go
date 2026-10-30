package handler

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (handler *Handler) createDecisionRecordComment(writer http.ResponseWriter, request *http.Request, issue db.Issue, input CreateCommentRequest) {
	if input.DecisionIntent || input.Type != "comment" || len(input.AttachmentIDs) > 0 || len(input.SuppressAgentIDs) > 0 || len(input.SteerTaskIDs) > 0 {
		writeError(writer, http.StatusBadRequest, "decision records cannot combine with intent, attachments, steering, suppression or another comment type")
		return
	}
	messageIDs, ok := parseUUIDSliceOrBadRequest(writer, input.MessageIDs, "message_ids")
	if !ok {
		return
	}
	var contextMessageID pgtype.UUID
	if input.ContextMessageID != "" {
		contextMessageID, ok = parseUUIDOrBadRequest(writer, input.ContextMessageID, "context_message_id")
		if !ok {
			return
		}
	}
	var parentID pgtype.UUID
	var sourceCommentID pgtype.UUID
	if input.SourceCommentID != "" {
		sourceCommentID, ok = parseUUIDOrBadRequest(writer, input.SourceCommentID, "source_comment_id")
		if !ok {
			return
		}
	}
	if input.ParentID != nil {
		parentID, ok = parseUUIDOrBadRequest(writer, *input.ParentID, "parent_id")
		if !ok {
			return
		}
	}
	if handler.rejectMissingRequiredSourceLink(writer, request, "agent", true, issue, input.Content, "content") {
		return
	}
	result, err := handler.createAskedDecisionRecord(request, issue, decisionRecordInput{ParentID: parentID, MessageIDs: messageIDs, ContextMessageID: contextMessageID, Excerpts: input.Excerpts, SourceCommentID: sourceCommentID, Content: input.Content, EscalationClass: input.EscalationClass})
	if err != nil {
		writeDecisionError(writer, err, "decision record")
		return
	}
	workspaceID, agentID := uuidToString(issue.WorkspaceID), request.Header.Get("X-Agent-ID")
	response := commentToResponse(result.Comment, nil, nil)
	if !result.Replayed {
		response.TriggerOutcomes = handler.routeDecisionRecordMentions(request, issue, result, input.Content)
	}
	response.DecisionCommentFields = handler.decisionCommentFields(request.Context(), issue.WorkspaceID, []db.Comment{result.Comment}, pgtype.UUID{})[response.ID]
	if result.Replayed {
		writeJSON(writer, http.StatusOK, response)
		return
	}
	handler.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{"comment": response, "issue_title": issue.Title, "issue_status": issue.Status})
	handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, "agent", agentID)
	handler.publishDecisionAnswerUpdated(workspaceID, "agent", agentID, result.Answer.ID, issue.ID, decisionAnswerState(result.Answer.WokenAt, result.Answer.DisclaimedAt))
	handler.publishChat(protocol.EventChatMessage, workspaceID, "agent", agentID, uuidToString(result.Message.ChatSessionID), protocol.ChatMessagePayload{
		MessageKind:   result.Message.MessageKind,
		ChatSessionID: uuidToString(result.Message.ChatSessionID), MessageID: uuidToString(result.Message.ID), Role: result.Message.Role, Content: result.Message.Content, CreatedAt: timestampToString(result.Message.CreatedAt),
	})
	writeJSON(writer, http.StatusCreated, response)
}

// routeDecisionRecordMentions sends the agent's own words in a record through the
// ordinary mention path, so authorization, project stop, de-duplication and the
// per-target outcomes match any other comment. The human's quote is not routed.
// A held class (escalation hold still running) reports its mention targets as
// deferred without enqueuing: the hold, not the record, decides when the action
// is released, and the asker is woken then.
func (handler *Handler) routeDecisionRecordMentions(request *http.Request, issue db.Issue, result decisionRecordResult, content string) []CommentTriggerOutcome {
	routable := false
	for _, mention := range util.ParseMentions(content) {
		routable = routable || mention.Type == "agent" || mention.Type == "squad"
	}
	if !routable {
		return nil
	}
	ctx := request.Context()
	agentID := request.Header.Get("X-Agent-ID")
	originatorUserID := handler.invokeOriginatorFromRequest(request, "agent", agentID)
	comment := result.Comment
	comment.Content = content
	if !result.Answer.EffectiveAt.Time.After(time.Now()) {
		return handler.triggerTasksForComment(ctx, issue, comment, nil, "agent", agentID, originatorUserID, nil, nil)
	}
	triggers, targets := handler.computeCommentAgentTriggers(ctx, issue, content, nil, "agent", agentID, commentTriggerComputeOptions{
		ExcludeTriggerCommentID: comment.ID,
		AuthoringTaskID:         comment.SourceTaskID,
		OriginatorUserID:        originatorUserID,
	})
	_, targets = filterCommentAuthorTriggers(triggers, targets, "agent", agentID)
	var outcomes []CommentTriggerOutcome
	for _, target := range targets {
		status, reason := target.Status, target.ReasonCode
		if target.ExecAgentID != "" {
			status, reason = DispatchDeferred, ReasonDeferred
		}
		outcomes = append(outcomes, CommentTriggerOutcome{TargetType: target.TargetType, TargetID: target.TargetID, Status: status, ReasonCode: reason})
	}
	return outcomes
}

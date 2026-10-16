package handler

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
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
	var parentID pgtype.UUID
	if input.ParentID != nil {
		parentID, ok = parseUUIDOrBadRequest(writer, *input.ParentID, "parent_id")
		if !ok {
			return
		}
	}
	if handler.rejectMissingRequiredSourceLink(writer, request, "agent", true, issue, input.Content, "content") {
		return
	}
	result, err := handler.createAskedDecisionRecord(request, issue, decisionRecordInput{ParentID: parentID, MessageIDs: messageIDs, Content: input.Content, EscalationClass: input.EscalationClass})
	if err != nil {
		writeDecisionError(writer, err, "decision record")
		return
	}
	workspaceID, agentID := uuidToString(issue.WorkspaceID), request.Header.Get("X-Agent-ID")
	response := commentToResponse(result.Comment, nil, nil)
	handler.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{"comment": response, "issue_title": issue.Title, "issue_status": issue.Status})
	handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, "agent", agentID)
	handler.publishChat(protocol.EventChatMessage, workspaceID, "agent", agentID, uuidToString(result.Message.ChatSessionID), protocol.ChatMessagePayload{
		ChatSessionID: uuidToString(result.Message.ChatSessionID), MessageID: uuidToString(result.Message.ID), Role: result.Message.Role, Content: result.Message.Content, CreatedAt: timestampToString(result.Message.CreatedAt),
	})
	writeJSON(writer, http.StatusCreated, response)
}

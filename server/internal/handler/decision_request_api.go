package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (handler *Handler) RequestDecision(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Actor-Source") != "task_token" {
		writeError(writer, http.StatusForbidden, "decision request requires this issue's running task")
		return
	}
	var input struct {
		IssueID         string              `json:"issue_id"`
		SessionID       string              `json:"session_id"`
		NewDM           bool                `json:"new_dm"`
		For             string              `json:"for"`
		Ask             json.RawMessage     `json:"ask"`
		Footer          string              `json:"footer"`
		ParentID        string              `json:"parent_id"`
		EscalationClass string              `json:"escalation_class"`
		Action          *decisionActionWire `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 8*maxCommentContentBytes)).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid decision request")
		return
	}
	issue, ok := handler.loadIssueForUser(writer, request, input.IssueID)
	if !ok {
		return
	}
	if _, ok := handler.requireWorkspaceMember(writer, request, uuidToString(issue.WorkspaceID), "workspace not found"); !ok {
		return
	}
	if len(input.Ask) == 0 || string(input.Ask) == "null" {
		writeError(writer, http.StatusBadRequest, "a structured ask is required")
		return
	}
	parsed, err := decisiontx.DecodeAsk(input.Ask)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	hints := decisiontx.LintAsk(parsed)
	sourceIssueID, ok := parseUUIDOrBadRequest(writer, parsed.SourceIssueID, "ask.source_issue_id")
	if !ok {
		return
	}
	sourceCommentID, ok := parseUUIDOrBadRequest(writer, parsed.SourceCommentID, "ask.source_comment_id")
	if !ok {
		return
	}
	if sourceIssueID != issue.ID {
		writeError(writer, http.StatusBadRequest, "ask.source_issue_id must match issue_id")
		return
	}
	workspace, err := handler.Queries.GetWorkspace(request.Context(), issue.WorkspaceID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to load workspace")
		return
	}
	ask := parsed.Content(workspace.Slug)
	if len(decisionAskContent(decisionRequestInput{Ask: ask, Footer: input.Footer})) > maxCommentContentBytes {
		writeError(writer, http.StatusBadRequest, "ask is too long")
		return
	}
	if input.NewDM && (input.SessionID != "" || input.For == "") {
		writeError(writer, http.StatusBadRequest, "new_dm takes for and no session_id")
		return
	}
	var sessionID, humanID, parentID pgtype.UUID
	for _, field := range []struct {
		value string
		name  string
		into  *pgtype.UUID
	}{{input.SessionID, "session_id", &sessionID}, {input.For, "for", &humanID}, {input.ParentID, "parent_id", &parentID}} {
		if field.value == "" {
			continue
		}
		if *field.into, ok = parseUUIDOrBadRequest(writer, field.value, field.name); !ok {
			return
		}
	}
	var draft *decisionActionDraft
	if input.Action != nil {
		if draft, err = draftDecisionAction(input.Action); err != nil {
			writeDecisionError(writer, err, "decision request")
			return
		}
		input.EscalationClass = actionRequestClass(input.EscalationClass, draft.Definition.MinClass)
	}
	result, err := handler.createDecisionRequest(request, issue, decisionRequestInput{SessionID: sessionID, NewDM: input.NewDM, HumanUserID: humanID, Ask: ask, Options: parsed.Options, Footer: input.Footer, ParentID: parentID, EscalationClass: input.EscalationClass, SourceCommentID: sourceCommentID, Action: draft})
	if err != nil {
		writeDecisionError(writer, err, "decision request")
		return
	}
	if result.Replayed {
		action, err := handler.replayedDecisionAction(request.Context(), issue, result.Request)
		if err != nil {
			writeDecisionError(writer, err, "decision request")
			return
		}
		writeJSON(writer, http.StatusOK, decisionRequestResponse{result.Request, hints, action})
		return
	}
	workspaceID, agentID := uuidToString(issue.WorkspaceID), uuidToString(result.Request.AskingAgentID)
	if result.CreatedSession != nil {
		session := result.CreatedSession
		handler.publishChat(protocol.EventChatSessionCreated, workspaceID, "agent", agentID, uuidToString(session.ID), protocol.ChatSessionCreatedPayload{
			WorkspaceID: workspaceID, ChatSessionID: uuidToString(session.ID), AgentID: agentID, CreatorID: uuidToString(session.CreatorID), Title: session.Title,
		})
	}
	item, pinged := result.PingItem, result.PingRecorded
	if !pinged {
		var err error
		if item, err = decisiontx.NotifyPing(request.Context(), handler.TxStarter, handler.Queries, issue.WorkspaceID, result.Request.ID); err != nil {
			slog.Warn("decision ping inbox pending retry", "decision_request_id", uuidToString(result.Request.ID), "error", err)
		} else {
			pinged = true
		}
	}
	if pinged {
		if stored, err := handler.Queries.GetDecisionRequest(request.Context(), db.GetDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: result.Request.ID}); err == nil {
			result.Request = stored
		}
		if item != nil {
			response := inboxToResponse(*item)
			response.IssueStatus = &issue.Status
			response.IssuePriority = &issue.Priority
			handler.publish(protocol.EventInboxNew, workspaceID, "agent", agentID, map[string]any{"item": response})
		}
	}
	if result.AttachedToIntent {
		// The intent comment was already announced as created; only its decision fields changed.
		handler.publish(protocol.EventCommentUpdated, workspaceID, "agent", agentID, map[string]any{"comment": commentToResponse(result.Comment, nil, nil)})
	} else {
		handler.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{
			"comment": commentToResponse(result.Comment, nil, nil), "issue_title": issue.Title, "issue_status": issue.Status,
			"decision_intent_targets": []string{uuidToString(result.Request.HumanUserID)},
		})
	}
	handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, "agent", agentID)
	handler.publishChat(protocol.EventChatMessage, workspaceID, "agent", agentID, uuidToString(result.Message.ChatSessionID), protocol.ChatMessagePayload{
		ChatSessionID: uuidToString(result.Message.ChatSessionID), MessageID: uuidToString(result.Message.ID),
		Role: result.Message.Role, Content: result.Message.Content, CreatedAt: timestampToString(result.Message.CreatedAt),
	})
	var action *decisionActionResponse
	if result.Action != nil {
		action = newDecisionActionResponse(*result.Action)
	}
	writeJSON(writer, http.StatusCreated, decisionRequestResponse{result.Request, hints, action})
}

// decisionRequestResponse adds advisory ask-lint hints to the stored request.
type decisionRequestResponse struct {
	db.DecisionRequest
	Hints  []string                `json:"hints,omitempty"`
	Action *decisionActionResponse `json:"action,omitempty"`
}

func (handler *Handler) GetDecision(writer http.ResponseWriter, request *http.Request) {
	workspaceID := handler.resolveWorkspaceID(request)
	if _, ok := handler.requireWorkspaceMember(writer, request, workspaceID, "workspace not found"); !ok {
		return
	}
	workspaceUUID, ok := parseUUIDOrBadRequest(writer, workspaceID, "workspace_id")
	if !ok {
		return
	}
	requestID, ok := parseUUIDOrBadRequest(writer, chi.URLParam(request, "id"), "id")
	if !ok {
		return
	}
	stored, err := handler.Queries.GetDecisionRequest(request.Context(), db.GetDecisionRequestParams{WorkspaceID: workspaceUUID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(writer, http.StatusNotFound, "decision request not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to read decision request")
		return
	}
	if _, ok := handler.loadIssueForUser(writer, request, uuidToString(stored.IssueID)); !ok {
		return
	}
	writeJSON(writer, http.StatusOK, stored)
}

func (handler *Handler) ListDecisions(writer http.ResponseWriter, request *http.Request) {
	issue, ok := handler.loadIssueForUser(writer, request, request.URL.Query().Get("issue"))
	if !ok {
		return
	}
	if _, ok := handler.requireWorkspaceMember(writer, request, uuidToString(issue.WorkspaceID), "workspace not found"); !ok {
		return
	}
	requests, err := handler.Queries.ListDecisionRequestsByIssue(request.Context(), db.ListDecisionRequestsByIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to list decision requests")
		return
	}
	if requests == nil {
		requests = []db.DecisionRequest{}
	}
	writeJSON(writer, http.StatusOK, requests)
}

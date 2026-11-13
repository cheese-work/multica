package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/weblink"
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
		Kind            string              `json:"kind"`
		ExpiresIn       string              `json:"expires_in"`
		// CoversRequestIDs names open cards this batch card answers for (CHE-4453).
		CoversRequestIDs []string `json:"covers_request_ids"`
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
	if input.Action == nil && handler.refuseBudgetQuestion(writer, request, issue) {
		return
	}
	if len(input.Ask) == 0 || string(input.Ask) == "null" {
		writeError(writer, http.StatusBadRequest, "a structured ask is required")
		return
	}
	kind, expiresInSeconds, ok := handler.actionRequestTerms(writer, input.Kind, input.ExpiresIn, input.Action != nil)
	if !ok {
		return
	}
	parsed, err := decisiontx.DecodeAskKind(input.Ask, kind)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	// The recommended option is shown first (CHE-3628); an ask with two or more options must identify it.
	parsed, err = parsed.RequireRecommendedFirst()
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
	if input.Action == nil && kind == decisiontx.KindDecision {
		hints = append(hints, plainCardConfigHints(parsed)...)
	}
	if err := handler.verifyAskArtifacts(request.Context(), issue, parsed); err != nil {
		writeDecisionError(writer, err, "decision request")
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
	var dmSession pgtype.UUID
	if taskID, err := util.ParseUUID(request.Header.Get("X-Task-ID")); err == nil {
		task, err := handler.Queries.GetAgentTask(request.Context(), taskID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(writer, http.StatusInternalServerError, "failed to load task")
			return
		}
		if err == nil && task.ChatSessionID.Valid {
			// A DM agent asks its own member in its own DM, never elsewhere.
			if input.NewDM || sessionID.Valid || parentID.Valid {
				writeError(writer, http.StatusBadRequest, "a request from a chat takes no new_dm, session_id or parent_id")
				return
			}
			if humanID, dmSession, ok = handler.proposalRecipient(writer, request, issue, taskID, input.For); !ok {
				return
			}
		}
	}
	covers, ok := parseBatchCovers(writer, input.CoversRequestIDs, input.Action != nil)
	if !ok {
		return
	}
	var draft *decisionActionDraft
	if input.Action != nil {
		if err = withIssueTarget(input.Action, issue.ID); err == nil {
			draft, err = draftDecisionAction(input.Action)
		}
		if err == nil {
			input.EscalationClass, err = handler.requestClassFor(request.Context(), issue, input.EscalationClass, draft)
		}
		if err != nil {
			writeDecisionError(writer, err, "decision request")
			return
		}
	}
	askJSON, err := json.Marshal(parsed)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to encode ask")
		return
	}
	result, err := handler.createDecisionRequest(request, issue, decisionRequestInput{AskJSON: askJSON, SessionID: sessionID, NewDM: input.NewDM, HumanUserID: humanID, Ask: ask, Options: parsed.Options, Footer: input.Footer, ParentID: parentID, EscalationClass: input.EscalationClass, SourceCommentID: sourceCommentID, Action: draft, DMSession: dmSession, Parsed: &parsed, Slug: workspace.Slug, Kind: kind, ExpiresInSeconds: expiresInSeconds, Covers: covers})
	if err != nil {
		writeDecisionError(writer, err, "decision request")
		return
	}
	handler.writeDecisionResult(writer, request, issue, result, hints)
}

// configChangeWords matches a decision or option that reads like a change to
// a workflow, agent, squad, workspace or budget in plain words.
var configChangeWords = regexp.MustCompile(`(?i)\b(?:workflow|squad|agent|workspace)\b.{0,80}\b(?:handler|instructions?|steps?|context|env(?:ironment)?|budget|prompt)\b|\bbudget\b.{0,40}\b(?:raise|top.?up|increase)|\b(?:raise|top.?up|increase)\b.{0,40}\bbudget\b`)

// plainCardConfigHints returns an advisory hint when an option card reads like
// a change an allowlisted action applies. Approve on an option card only
// records an answer, so the change would never apply and the person would be
// sent a second link (CHE-5229). The server cannot read intent, so this never
// refuses: it scans the decision and the options (not the agent_did detail)
// for an action type string or plain words naming such a change.
func plainCardConfigHints(ask decisiontx.Ask) []string {
	texts := []string{ask.Decision}
	for _, option := range ask.Options {
		texts = append(texts, option.Label, option.WhatHappens)
	}
	text := strings.Join(texts, "\n")
	matched := configChangeWords.MatchString(text)
	for _, actionType := range decisionaction.Types() {
		matched = matched || strings.Contains(text, actionType)
	}
	if !matched {
		return nil
	}
	return []string{"the decision or an option reads like a change to a workflow, agent, squad, workspace or budget; Approve on an option card only records the answer and applies nothing. If one approval should apply the change, send it with `multica decision propose` (or `multica decision request --action-file`)"}
}

// writeDecisionResult answers a replay with 200, or publishes the events of a
// new request and answers 201.
func (handler *Handler) writeDecisionResult(writer http.ResponseWriter, request *http.Request, issue db.Issue, result decisionRequestResult, hints []string) {
	if result.Replayed {
		action, err := handler.replayedDecisionAction(request.Context(), issue, result.Request)
		if err != nil {
			writeDecisionError(writer, err, "decision request")
			return
		}
		writeJSON(writer, http.StatusOK, decisionRequestResponse{result.Request, hints, action})
		return
	}
	handler.publishDecisionResult(request, issue, &result)
	var action *decisionActionResponse
	if result.Action != nil {
		action = newDecisionActionResponse(*result.Action)
	}
	writeJSON(writer, http.StatusCreated, decisionRequestResponse{result.Request, hints, action})
}

// publishDecisionResult announces the rows of a new request. It records a ping
// that the transaction did not, and refreshes result.Request. A batch request
// that does not lead its card has no message to announce.
func (handler *Handler) publishDecisionResult(request *http.Request, issue db.Issue, result *decisionRequestResult) {
	workspaceID, agentID := uuidToString(issue.WorkspaceID), uuidToString(result.Request.AskingAgentID)
	if result.CreatedSession != nil {
		session := result.CreatedSession
		handler.publishChatFor(protocol.EventChatSessionCreated, workspaceID, "agent", agentID, uuidToString(session.CreatorID), uuidToString(session.ID), protocol.ChatSessionCreatedPayload{
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
	if result.ParkedActivity != nil {
		handler.publishDecisionStatusChange(request.Context(), issue, result.ParkedIssue, result.ParkedActivity, "decision_request")
	}
	handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, "agent", agentID)
	if result.Message.ID.Valid {
		handler.publishChatFor(protocol.EventChatMessage, workspaceID, "agent", agentID, uuidToString(result.Request.HumanUserID), uuidToString(result.Message.ChatSessionID), protocol.ChatMessagePayload{
			MessageKind:   result.Message.MessageKind,
			ChatSessionID: uuidToString(result.Message.ChatSessionID), MessageID: uuidToString(result.Message.ID),
			Role: result.Message.Role, Content: result.Message.Content, CreatedAt: timestampToString(result.Message.CreatedAt),
		})
	}
}

// decisionRequestResponse adds advisory ask-lint hints to the stored request.
type decisionRequestResponse struct {
	db.DecisionRequest
	Hints  []string                `json:"hints,omitempty"`
	Action *decisionActionResponse `json:"action,omitempty"`
}

// decisionReadResponse is the stored request plus its newest answer, so an
// agent reads the same state the person saw: who answered, when, with what.
type decisionReadResponse struct {
	db.DecisionRequest
	Answer *decisionAnswerRead `json:"answer,omitempty"`
	// The outcome of the request's proposed action, once it is terminal (CHE-5411).
	AppliedAt     *string `json:"applied_at,omitempty"`
	Outcome       string  `json:"outcome,omitempty"`
	OutcomeReason *string `json:"outcome_reason,omitempty"`
	// The effect readback (B v9 S5, CHE-5415): what the approval did, who decided,
	// for whom. Revocable is false for every action until S6 adds revocation.
	Effect       string  `json:"effect,omitempty"`
	EffectReason *string `json:"effect_reason,omitempty"`
	DecidedBy    *string `json:"decided_by,omitempty"`
	OnBehalfOf   *string `json:"on_behalf_of,omitempty"`
	Revocable    bool    `json:"revocable"`
}

type decisionAnswerRead struct {
	ID                string  `json:"id"`
	AnsweredByUserID  *string `json:"answered_by_user_id"`
	AnswerSource      string  `json:"answer_source"`
	ChosenOptionIndex *int32  `json:"chosen_option_index"`
	ContentSnapshot   string  `json:"content_snapshot"`
	CreatedAt         *string `json:"created_at"`
	DisclaimedAt      *string `json:"disclaimed_at"`
	DisclaimReason    *string `json:"disclaim_reason"`
}

func (handler *Handler) GetDecision(writer http.ResponseWriter, request *http.Request) {
	stored, _, ok := handler.loadDecisionForRead(writer, request)
	if !ok {
		return
	}
	response := decisionReadResponse{DecisionRequest: stored}
	switch action, err := handler.Queries.GetDecisionActionByRequest(request.Context(), db.GetDecisionActionByRequestParams{DecisionRequestID: stored.ID, WorkspaceID: stored.WorkspaceID}); {
	case err == nil:
		response.Outcome, response.OutcomeReason = decisionActionOutcomeOf(action)
		response.AppliedAt = appliedAtOf(action)
		response.Effect, response.EffectReason = decisionEffectOf(action)
		response.OnBehalfOf = uuidToPtr(stored.HumanUserID)
	case !errors.Is(err, pgx.ErrNoRows):
		writeError(writer, http.StatusInternalServerError, "failed to read decision action")
		return
	}
	answer, err := handler.Queries.GetLatestDecisionAnswer(request.Context(), db.GetLatestDecisionAnswerParams{WorkspaceID: stored.WorkspaceID, ID: stored.ID})
	switch {
	case err == nil:
		if response.Effect == "applied" || response.Effect == "not_applied" {
			response.DecidedBy = uuidToPtr(answer.AnsweredByUserID)
		}
		response.Answer = &decisionAnswerRead{
			ID: uuidToString(answer.ID), AnsweredByUserID: uuidToPtr(answer.AnsweredByUserID), AnswerSource: answer.AnswerSource,
			ChosenOptionIndex: int4ToPtr(answer.ChosenOptionIndex), ContentSnapshot: answer.ContentSnapshot, CreatedAt: timestampToPtr(answer.CreatedAt),
			DisclaimedAt: timestampToPtr(answer.DisclaimedAt), DisclaimReason: textToPtr(answer.DisclaimReason),
		}
	case !errors.Is(err, pgx.ErrNoRows):
		writeError(writer, http.StatusInternalServerError, "failed to read decision answer")
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

// loadDecisionForRead resolves {id} for a workspace member who can read the
// request's issue; it writes the refusal itself when it returns false.
func (handler *Handler) loadDecisionForRead(writer http.ResponseWriter, request *http.Request) (db.DecisionRequest, db.Issue, bool) {
	workspaceID := handler.resolveWorkspaceID(request)
	if _, ok := handler.requireWorkspaceMember(writer, request, workspaceID, "workspace not found"); !ok {
		return db.DecisionRequest{}, db.Issue{}, false
	}
	workspaceUUID, ok := parseUUIDOrBadRequest(writer, workspaceID, "workspace_id")
	if !ok {
		return db.DecisionRequest{}, db.Issue{}, false
	}
	requestID, ok := parseUUIDOrBadRequest(writer, chi.URLParam(request, "id"), "id")
	if !ok {
		return db.DecisionRequest{}, db.Issue{}, false
	}
	stored, err := handler.Queries.GetDecisionRequest(request.Context(), db.GetDecisionRequestParams{WorkspaceID: workspaceUUID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(writer, http.StatusNotFound, "decision request not found")
		return stored, db.Issue{}, false
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to read decision request")
		return stored, db.Issue{}, false
	}
	issue, ok := handler.loadIssueForUser(writer, request, uuidToString(stored.IssueID))
	return stored, issue, ok
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

func (handler *Handler) ListOpenDecisions(writer http.ResponseWriter, request *http.Request) {
	if isMachineCredentialActor(request) {
		writeError(writer, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	member, ok := handler.requireWorkspaceMember(writer, request, ctxWorkspaceID(request.Context()), "workspace not found")
	if !ok {
		return
	}
	requests, err := handler.Queries.ListOpenDecisionRequestsForHuman(request.Context(), db.ListOpenDecisionRequestsForHumanParams{
		WorkspaceID: member.WorkspaceID, HumanUserID: member.UserID,
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to list open decision requests")
		return
	}
	writeJSON(writer, http.StatusOK, handler.openDecisionResponses(request.Context(), member.WorkspaceID, requests))
}

// openDecisionResponse sends options as JSON, not as the row's base64 bytes,
// so the issue-page banner can render the answer buttons. A batch card also
// carries the decisions it covers: the issue page shows them next to the
// answer form, so a human never answers for a card they cannot see.
type openDecisionResponse struct {
	db.ListOpenDecisionRequestsForHumanRow
	Options          json.RawMessage              `json:"options"`
	CoversRequestIDs []string                     `json:"covers_request_ids,omitempty"`
	CoveredCards     []DecisionRequestCoveredCard `json:"covered_cards,omitempty"`
	// Action is the executable action behind the card, so a page can tell which
	// card is a run-budget or workflow-budget ask (CHE-5285).
	Action *DecisionRequestCardAction `json:"action,omitempty"`
}

func (handler *Handler) openDecisionResponses(ctx context.Context, workspaceUUID pgtype.UUID, rows []db.ListOpenDecisionRequestsForHumanRow) []openDecisionResponse {
	out := make([]openDecisionResponse, len(rows))
	for i, row := range rows {
		out[i] = openDecisionResponse{ListOpenDecisionRequestsForHumanRow: row, Options: row.Options}
		if len(out[i].Options) == 0 {
			out[i].Options = json.RawMessage("[]")
		}
		for _, covered := range row.CoversRequestIds {
			out[i].CoversRequestIDs = append(out[i].CoversRequestIDs, uuidToString(covered))
		}
		// On a read failure the card keeps covers_request_ids but lists no covered
		// cards; the client then refuses the answer form (fail closed).
		covered, err := handler.coveredCardSummaries(ctx, workspaceUUID, row.CoversRequestIds)
		if err != nil {
			slog.ErrorContext(ctx, "list covered decision cards", "request_id", uuidToString(row.ID), "error", err)
		}
		out[i].CoveredCards = covered
		out[i].Action = handler.decisionCardAction(ctx, workspaceUUID, row.ID)
	}
	return out
}

// openAskResponse is the open-asks projection. It deliberately has no field
// for options, answers, or the full ask, so a task token cannot read them here.
type openAskResponse struct {
	ID               string `json:"id"`
	IssueID          string `json:"issue_id"`
	IssueIdentifier  string `json:"issue_identifier"`
	Ask              string `json:"ask"`
	Class            string `json:"escalation_class"`
	CreatedAt        string `json:"created_at"`
	CardLink         string `json:"card_link"`
	AskingAgentID    string `json:"asking_agent_id"`
	DefaultIfNoReply string `json:"default_if_no_reply"`
}

// ListOpenDecisionAsks returns one member's open requests, oldest first. A
// member (JWT or PAT, including a cloud PAT) reads only their own; only a task
// token reads any member's, because the evening brief runs as an agent.
func (handler *Handler) ListOpenDecisionAsks(writer http.ResponseWriter, request *http.Request) {
	member, ok := handler.requireWorkspaceMember(writer, request, handler.resolveWorkspaceID(request), "workspace not found")
	if !ok {
		return
	}
	humanID, ok := parseUUIDOrBadRequest(writer, request.URL.Query().Get("for"), "for")
	if !ok {
		return
	}
	if humanID != member.UserID && request.Header.Get("X-Actor-Source") != "task_token" {
		writeError(writer, http.StatusForbidden, "members read only their own open asks")
		return
	}
	rows, err := handler.Queries.ListOpenDecisionAsksForHuman(request.Context(), db.ListOpenDecisionAsksForHumanParams{
		WorkspaceID: member.WorkspaceID, HumanUserID: humanID,
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to list open asks")
		return
	}
	asks := make([]openAskResponse, 0, len(rows))
	for _, row := range rows {
		link := weblink.Chat(row.WorkspaceSlug, uuidToString(row.ChatSessionID))
		if absolute := weblink.Build(handler.cfg.AppURL, link); absolute != "" {
			link = absolute
		}
		asks = append(asks, openAskResponse{
			ID: uuidToString(row.ID), IssueID: uuidToString(row.IssueID), IssueIdentifier: row.IssueIdentifier,
			Ask: row.AskHeadline, Class: row.EscalationClass, CreatedAt: timestampToString(row.CreatedAt),
			CardLink: link, AskingAgentID: uuidToString(row.AskingAgentID), DefaultIfNoReply: row.DefaultIfNoReply,
		})
	}
	writeJSON(writer, http.StatusOK, asks)
}

const (
	defaultActionExpiry = 72 * time.Hour
	maxActionExpiry     = 168 * time.Hour
)

// actionRequestTerms checks the request kind and its expiry, and answers 400 for
// anything the kind does not allow. The returned kind is "decision" or "action";
// the seconds are 0 for a decision, which keeps its fixed lifetime.
func (handler *Handler) actionRequestTerms(writer http.ResponseWriter, kind, expiresIn string, hasAction bool) (string, float64, bool) {
	switch kind {
	case "", decisiontx.KindDecision:
		if expiresIn != "" {
			writeError(writer, http.StatusBadRequest, "expires_in applies to action requests only")
			return "", 0, false
		}
		return decisiontx.KindDecision, 0, true
	case decisiontx.KindAction:
	default:
		writeError(writer, http.StatusBadRequest, "kind must be decision or action")
		return "", 0, false
	}
	if !handler.ActionRequestsEnabled {
		writeErrorCode(writer, http.StatusBadRequest, "action_requests_unavailable", "action requests are not available yet")
		return "", 0, false
	}
	if hasAction {
		writeError(writer, http.StatusBadRequest, "an action request cannot carry an action")
		return "", 0, false
	}
	expiry := defaultActionExpiry
	if expiresIn != "" {
		parsed, err := time.ParseDuration(expiresIn)
		if err != nil || parsed <= 0 || parsed > maxActionExpiry {
			writeError(writer, http.StatusBadRequest, "expires_in must be a duration above 0 and at most 168h")
			return "", 0, false
		}
		expiry = parsed
	}
	return decisiontx.KindAction, expiry.Seconds(), true
}

// refuseBudgetQuestion refuses a question about a budget block while the
// platform's top-up card is open: the card is the only way to reopen the
// budget, and a second card for the same block makes two requests (CHE-5303).
func (handler *Handler) refuseBudgetQuestion(writer http.ResponseWriter, request *http.Request, issue db.Issue) bool {
	requests, err := handler.Queries.ListDecisionRequestsByIssue(request.Context(), db.ListDecisionRequestsByIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to check the run budget card")
		return true
	}
	for _, open := range requests {
		holds, isCard, err := runBudgetCardHolds(request.Context(), handler.Queries, open)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "failed to check the run budget card")
			return true
		}
		if !isCard || !holds {
			continue
		}
		action, err := handler.Queries.GetDecisionActionByRequest(request.Context(), db.GetDecisionActionByRequestParams{DecisionRequestID: open.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "failed to read the run budget card")
			return true
		}
		workspace, err := handler.Queries.GetWorkspace(request.Context(), issue.WorkspaceID)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "failed to read the workspace")
			return true
		}
		url := "/" + workspace.Slug + "/issues/" + uuidToString(issue.ID) + "?approve=" + uuidToString(action.ID)
		writeErrorCode(writer, http.StatusConflict, "budget_card_open", "this issue's run budget top-up card is already open; the person approves it at "+url+" - do not ask a second question")
		return true
	}
	return false
}

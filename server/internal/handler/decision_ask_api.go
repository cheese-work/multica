package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AskDecision is the one-step decision_ask: the running task names the issue and
// the agent, and the issue names the person, so the body carries only the ask.
// It then runs the single-call RequestDecision path unchanged.
func (handler *Handler) AskDecision(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Actor-Source") != "task_token" {
		writeError(writer, http.StatusForbidden, "decision ask requires this issue's running task")
		return
	}
	var input struct {
		Ask             json.RawMessage `json:"ask"`
		Footer          string          `json:"footer"`
		ParentID        string          `json:"parent_id"`
		EscalationClass string          `json:"escalation_class"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 8*maxCommentContentBytes))
	decoder.DisallowUnknownFields() // issue, agent and person come from the task, never the body
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid decision ask")
		return
	}
	taskID, err := util.ParseUUID(request.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(writer, http.StatusForbidden, "decision ask requires this issue's running task")
		return
	}
	task, err := handler.Queries.GetAgentTask(request.Context(), taskID)
	if err == nil && !task.IssueID.Valid {
		err = pgx.ErrNoRows
	}
	var issue db.Issue
	if err == nil {
		issue, err = handler.Queries.GetIssue(request.Context(), task.IssueID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(writer, http.StatusForbidden, "decision ask requires this issue's running task")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to load task issue")
		return
	}
	// The requester decides; an issue an agent created for itself has no person
	// to pick, and guessing a recipient is worse than refusing.
	human := issue.RequestedByID
	if !human.Valid && issue.CreatorType == "member" {
		human = issue.CreatorID
	}
	if !human.Valid {
		writeError(writer, http.StatusConflict, "decision ask found no person to ask on this issue; use decision request with --for")
		return
	}
	body, err := json.Marshal(map[string]any{
		"issue_id": uuidToString(issue.ID), "for": uuidToString(human), "ask": input.Ask,
		"footer": input.Footer, "parent_id": input.ParentID, "escalation_class": input.EscalationClass,
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to encode decision ask")
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	handler.RequestDecision(writer, request)
}

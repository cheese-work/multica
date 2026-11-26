<<<<<<< HEAD
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ChatWakeupResponse struct {
	ID          string `json:"id"`
	AgentID     string `json:"agent_id"`
	FireAt      string `json:"fire_at"`
	Title       string `json:"title"`
	Instruction string `json:"instruction"`
	Status      string `json:"status"`
	// Issue watch fields; empty for a plain timed wakeup. FireAt is the watch's expiry.
	IssueID     string `json:"issue_id,omitempty"`
	Event       string `json:"event,omitempty"`
	ActorType   string `json:"filter_actor_type,omitempty"`
	ActorID     string `json:"filter_actor_id,omitempty"`
	UntilStatus string `json:"until_status,omitempty"`
	OnTimeout   string `json:"on_timeout,omitempty"`
}

func chatWakeupToResponse(w db.ChatWakeup) ChatWakeupResponse {
	r := ChatWakeupResponse{ID: uuidToString(w.ID), AgentID: uuidToString(w.AgentID), FireAt: timestampToString(w.FireAt), Title: w.Title, Instruction: w.Instruction, Status: w.Status}
	if w.IssueID.Valid {
		r.IssueID, r.Event, r.ActorType, r.ActorID = uuidToString(w.IssueID), w.EventType, w.FilterActorType, uuidToString(w.FilterActorID)
		r.UntilStatus, r.OnTimeout = w.UntilStatus, w.OnTimeout
	}
	return r
}

// chatWakeupTaskID returns the chat task the request runs as. Wakeups are an
// agent-facing surface: the task token names the DM, so no session id is passed.
func chatWakeupTaskID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "only a running chat task can manage DM wakeups")
		return pgtype.UUID{}, false
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(w, http.StatusForbidden, "a running chat task is required")
		return pgtype.UUID{}, false
	}
	return taskID, true
}

func (h *Handler) chatWakeupService() *service.ChatWakeupService {
	return &service.ChatWakeupService{Tasks: h.TaskService}
}

func writeChatWakeupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrChatWakeupNotOwner), errors.Is(err, service.ErrChatWatchIssueAccess):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, service.ErrChatWakeupNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrChatWakeupLimit):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrChatWakeupTooFar), errors.Is(err, service.ErrChatWakeupPast),
		errors.Is(err, service.ErrChatWakeupEmpty), errors.Is(err, service.ErrChatWakeupTooLong),
		errors.Is(err, service.ErrChatWakeupTitleTooLong), errors.Is(err, service.ErrChatWatchInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "chat wakeup failed")
	}
}

func (h *Handler) CreateChatWakeup(w http.ResponseWriter, r *http.Request) {
	taskID, ok := chatWakeupTaskID(w, r)
	if !ok {
		return
	}
	var req struct {
		AfterSeconds int64  `json:"after_seconds"`
		At           string `json:"at"`
		Title        string `json:"title"`
		Instruction  string `json:"instruction"`
		// Issue watch: Issue is a UUID or identifier; the expiry is ExpiresInSeconds (default 24h).
		Issue            string `json:"issue"`
		Event            string `json:"event"`
		ActorType        string `json:"filter_actor_type"`
		ActorID          string `json:"filter_actor_id"`
		UntilStatus      string `json:"until_status"`
		ExpiresInSeconds int64  `json:"expires_in_seconds"`
		OnTimeout        string `json:"on_timeout"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var at time.Time
	watch := service.ChatWakeupInput{}
	switch {
	case req.Issue != "":
		if req.AfterSeconds != 0 || req.At != "" {
			writeError(w, http.StatusBadRequest, "an issue watch takes expires_in_seconds, not after_seconds or at")
			return
		}
		issue, ok := h.chatWatchIssue(w, r, req.Issue)
		if !ok {
			return
		}
		if req.ActorID != "" {
			actorID, ok := parseUUIDOrBadRequest(w, req.ActorID, "filter_actor_id")
			if !ok {
				return
			}
			watch.ActorID = actorID
		}
		if req.ExpiresInSeconds < 0 || req.ExpiresInSeconds > int64(service.MaxChatWakeupDelay/time.Second) || req.OnTimeout != "" && req.OnTimeout != "wake" && req.OnTimeout != "drop" {
			writeError(w, http.StatusBadRequest, "expires_in_seconds must be within 24h and on_timeout wake or drop")
			return
		}
		expires := time.Duration(req.ExpiresInSeconds) * time.Second
		if expires == 0 {
			expires = service.MaxChatWakeupDelay - time.Minute
		}
		at = time.Now().Add(expires)
		watch.IssueID, watch.Event, watch.ActorType, watch.UntilStatus, watch.WakeOnTimeout = issue.ID, req.Event, req.ActorType, req.UntilStatus, req.OnTimeout == "wake"
	case (req.AfterSeconds > 0) == (req.At != ""):
		writeError(w, http.StatusBadRequest, "exactly one of after_seconds or at is required")
		return
	case req.AfterSeconds > int64(service.MaxChatWakeupDelay/time.Second):
		writeError(w, http.StatusBadRequest, service.ErrChatWakeupTooFar.Error())
		return
	case req.At != "":
		parsed, err := time.Parse(time.RFC3339, req.At)
		if err != nil {
			writeError(w, http.StatusBadRequest, "at must be an RFC3339 time")
			return
		}
		at = parsed
	default:
		at = time.Now().Add(time.Duration(req.AfterSeconds) * time.Second)
	}
	wakeup, err := h.chatWakeupService().Create(r.Context(), withBase(watch, taskID, at, req.Title, req.Instruction))
	if err != nil {
		writeChatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, chatWakeupToResponse(wakeup))
}

func (h *Handler) ListChatWakeups(w http.ResponseWriter, r *http.Request) {
	taskID, ok := chatWakeupTaskID(w, r)
	if !ok {
		return
	}
	wakeups, err := h.chatWakeupService().List(r.Context(), taskID)
	if err != nil {
		writeChatWakeupError(w, err)
		return
	}
	out := make([]ChatWakeupResponse, 0, len(wakeups))
	for _, wakeup := range wakeups {
		out = append(out, chatWakeupToResponse(wakeup))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) CancelChatWakeup(w http.ResponseWriter, r *http.Request) {
	taskID, ok := chatWakeupTaskID(w, r)
	if !ok {
		return
	}
	wakeupID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "wakeupId"), "wakeupId")
	if !ok {
		return
	}
	if err := h.chatWakeupService().Cancel(r.Context(), taskID, wakeupID); err != nil {
		writeChatWakeupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) memberWakeupSession(w http.ResponseWriter, r *http.Request) (db.ChatSession, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return db.ChatSession{}, false
	}
	if r.Header.Get("X-Agent-ID") != "" {
		writeError(w, http.StatusForbidden, "only the DM's human can manage its wakeups here")
		return db.ChatSession{}, false
	}
	return h.gateChatSessionForUser(w, r, userID, h.resolveWorkspaceID(r), chi.URLParam(r, "sessionId"))
}

// Member-facing DM wakeups: the DM creator reads and cancels the schedule the
// agent set. Creating one stays agent-only.
func (h *Handler) ListChatSessionWakeups(w http.ResponseWriter, r *http.Request) {
	session, ok := h.memberWakeupSession(w, r)
	if !ok {
		return
	}
	wakeups, err := h.Queries.ListPendingChatWakeups(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list wakeups")
		return
	}
	out := make([]ChatWakeupResponse, 0, len(wakeups))
	for _, wakeup := range wakeups {
		out = append(out, chatWakeupToResponse(wakeup))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) CancelChatSessionWakeup(w http.ResponseWriter, r *http.Request) {
	session, ok := h.memberWakeupSession(w, r)
	if !ok {
		return
	}
	wakeupID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "wakeupId"), "wakeupId")
	if !ok {
		return
	}
	if err := h.chatWakeupService().CancelInSession(r.Context(), session.ID, wakeupID); err != nil {
		writeChatWakeupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func withBase(in service.ChatWakeupInput, taskID pgtype.UUID, at time.Time, title, instruction string) service.ChatWakeupInput {
	in.TaskID, in.At, in.Title, in.Instruction = taskID, at, title, instruction
	return in
}

// chatWatchIssue resolves an issue by UUID or identifier inside the caller's workspace.
func (h *Handler) chatWatchIssue(w http.ResponseWriter, r *http.Request, ref string) (db.Issue, bool) {
	wsID := h.resolveWorkspaceID(r)
	if issue, ok, _ := h.resolveIssueByIdentifier(r.Context(), ref, wsID); ok {
		return issue, true
	}
	issueID, err1 := util.ParseUUID(ref)
	wsUUID, err2 := util.ParseUUID(wsID)
	if err1 == nil && err2 == nil {
		if issue, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: wsUUID}); err == nil {
			return issue, true
		}
	}
	writeError(w, http.StatusNotFound, "issue not found")
	return db.Issue{}, false
}
||||||| 2fcb424b72
=======
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ChatWakeupResponse is one chat wakeup as clients and the CLI see it.
type ChatWakeupResponse struct {
	ID               string          `json:"id"`
	ChatSessionID    string          `json:"chat_session_id"`
	ChatSessionTitle string          `json:"chat_session_title,omitempty"`
	AgentID          string          `json:"agent_id"`
	Title            string          `json:"title"`
	Instruction      string          `json:"instruction"`
	Kind             string          `json:"kind"`
	Condition        json.RawMessage `json:"condition,omitempty"`
	IntervalSeconds  *int64          `json:"interval_seconds"`
	FireAt           *string         `json:"fire_at"`
	NextEvalAt       string          `json:"next_eval_at"`
	ExpiresAt        string          `json:"expires_at"`
	Status           string          `json:"status"`
	EndReason        *string         `json:"end_reason"`
	EndedAt          *string         `json:"ended_at"`
	CheckCount       int32           `json:"check_count"`
	LastCheckNote    *string         `json:"last_check_note"`
	LastCheckedAt    *string         `json:"last_checked_at"`
	LastTaskID       *string         `json:"last_task_id"`
	LastTaskStatus   string          `json:"last_task_status,omitempty"`
	LastFiredAt      *string         `json:"last_fired_at"`
	LastError        *string         `json:"last_error"`
	SourceMessageID  *string         `json:"source_message_id"`
	SourceTaskID     *string         `json:"source_task_id"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
}

func chatWakeupToResponse(w db.ChatWakeup) ChatWakeupResponse {
	resp := ChatWakeupResponse{
		ID:              uuidToString(w.ID),
		ChatSessionID:   uuidToString(w.ChatSessionID),
		AgentID:         uuidToString(w.AgentID),
		Title:           w.Title,
		Instruction:     w.Instruction,
		Kind:            w.Kind,
		IntervalSeconds: int8ToPtr(w.IntervalSeconds),
		FireAt:          timestampToPtr(w.FireAt),
		NextEvalAt:      timestampToString(w.NextEvalAt),
		ExpiresAt:       timestampToString(w.ExpiresAt),
		Status:          w.Status,
		EndReason:       textToPtr(w.EndReason),
		EndedAt:         timestampToPtr(w.EndedAt),
		CheckCount:      w.CheckCount,
		LastCheckNote:   textToPtr(w.LastCheckNote),
		LastCheckedAt:   timestampToPtr(w.LastCheckedAt),
		LastTaskID:      uuidToPtr(w.LastTaskID),
		LastFiredAt:     timestampToPtr(w.LastFiredAt),
		LastError:       textToPtr(w.LastError),
		SourceMessageID: uuidToPtr(w.SourceMessageID),
		SourceTaskID:    uuidToPtr(w.SourceTaskID),
		CreatedAt:       timestampToString(w.CreatedAt),
		UpdatedAt:       timestampToString(w.UpdatedAt),
	}
	if len(w.Condition) > 0 {
		resp.Condition = w.Condition
	}
	return resp
}

func chatWakeupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrChatWakeupInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrChatWakeupLimit):
		writeErrorCode(w, http.StatusBadRequest, "chat_wakeup_limit", err.Error())
	case errors.Is(err, service.ErrChatWakeupNotFound):
		writeError(w, http.StatusNotFound, "chat wakeup not found")
	case errors.Is(err, service.ErrChatWakeupEnded):
		writeError(w, http.StatusConflict, "this wakeup has already ended")
	default:
		writeError(w, http.StatusInternalServerError, "could not save chat wakeup")
	}
}

func (h *Handler) chatWakeups() *service.ChatWakeupService {
	return &service.ChatWakeupService{Tasks: h.TaskService}
}

// chatTurnTask resolves the chat turn behind an agent's task-scoped token. Like
// `multica chat history`, the agent never names a session: it can only manage
// the wakeups of the conversation it is running in.
func (h *Handler) chatTurnTask(w http.ResponseWriter, r *http.Request) (db.AgentTaskQueue, bool) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "chat wakeups are only managed from within a chat turn")
		return db.AgentTaskQueue{}, false
	}
	taskID, ok := parseUUIDOrBadRequest(w, r.Header.Get("X-Task-ID"), "task id")
	if !ok {
		return db.AgentTaskQueue{}, false
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return db.AgentTaskQueue{}, false
	}
	if !task.ChatSessionID.Valid {
		writeError(w, http.StatusBadRequest, "this task is not a chat turn; use `multica issue wakeup` for issue work")
		return db.AgentTaskQueue{}, false
	}
	session, err := h.Queries.GetChatSession(r.Context(), task.ChatSessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "chat session not found")
		return db.AgentTaskQueue{}, false
	}
	if ws := ctxWorkspaceID(r.Context()); ws != "" && uuidToString(session.WorkspaceID) != ws {
		writeError(w, http.StatusForbidden, "chat session does not belong to this workspace")
		return db.AgentTaskQueue{}, false
	}
	return task, true
}

func (h *Handler) listChatWakeups(w http.ResponseWriter, r *http.Request, sessionID pgtype.UUID) {
	rows, err := h.Queries.ListChatWakeupsForSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chat wakeups")
		return
	}
	resp := make([]ChatWakeupResponse, 0, len(rows))
	for _, row := range rows {
		item := chatWakeupToResponse(db.ChatWakeup{
			ID: row.ID, WorkspaceID: row.WorkspaceID, ChatSessionID: row.ChatSessionID, AgentID: row.AgentID,
			RequesterID: row.RequesterID, SourceTaskID: row.SourceTaskID, SourceMessageID: row.SourceMessageID,
			Title: row.Title, Instruction: row.Instruction, Kind: row.Kind, Condition: row.Condition,
			ConditionState: row.ConditionState, IntervalSeconds: row.IntervalSeconds, FireAt: row.FireAt,
			NextEvalAt: row.NextEvalAt, ExpiresAt: row.ExpiresAt, Status: row.Status, EndReason: row.EndReason,
			EndedAt: row.EndedAt, CheckCount: row.CheckCount, LastCheckNote: row.LastCheckNote,
			LastCheckedAt: row.LastCheckedAt, LastCheckinTaskID: row.LastCheckinTaskID, LastTaskID: row.LastTaskID,
			LastFiredAt: row.LastFiredAt, LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
		item.LastTaskStatus = row.LastTaskStatus
		resp = append(resp, item)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- Agent endpoints: the calling chat turn's own conversation ----

func (h *Handler) ListChatTurnWakeups(w http.ResponseWriter, r *http.Request) {
	task, ok := h.chatTurnTask(w, r)
	if !ok {
		return
	}
	h.listChatWakeups(w, r, task.ChatSessionID)
}

func decodeChatWakeupInput(w http.ResponseWriter, r *http.Request) (service.ChatWakeupInput, bool) {
	var in service.ChatWakeupInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return in, false
	}
	return in, true
}

func (h *Handler) CreateChatTurnWakeup(w http.ResponseWriter, r *http.Request) {
	task, ok := h.chatTurnTask(w, r)
	if !ok {
		return
	}
	in, ok := decodeChatWakeupInput(w, r)
	if !ok {
		return
	}
	created, err := h.chatWakeups().Create(r.Context(), task, in)
	if err != nil {
		chatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, chatWakeupToResponse(created))
}

func (h *Handler) chatTurnWakeupID(w http.ResponseWriter, r *http.Request) (db.AgentTaskQueue, pgtype.UUID, bool) {
	task, ok := h.chatTurnTask(w, r)
	if !ok {
		return task, pgtype.UUID{}, false
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "wakeupId"), "wakeup id")
	return task, id, ok
}

func (h *Handler) UpdateChatTurnWakeup(w http.ResponseWriter, r *http.Request) {
	task, id, ok := h.chatTurnWakeupID(w, r)
	if !ok {
		return
	}
	in, ok := decodeChatWakeupInput(w, r)
	if !ok {
		return
	}
	updated, err := h.chatWakeups().Update(r.Context(), task, id, in)
	if err != nil {
		chatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatWakeupToResponse(updated))
}

func (h *Handler) CancelChatTurnWakeup(w http.ResponseWriter, r *http.Request) {
	task, id, ok := h.chatTurnWakeupID(w, r)
	if !ok {
		return
	}
	ended, err := h.chatWakeups().Cancel(r.Context(), task.ChatSessionID, id, "agent")
	if err != nil {
		chatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatWakeupToResponse(ended))
}

func (h *Handler) FinishChatTurnWakeup(w http.ResponseWriter, r *http.Request) {
	task, id, ok := h.chatTurnWakeupID(w, r)
	if !ok {
		return
	}
	ended, err := h.chatWakeups().Finish(r.Context(), task, id)
	if err != nil {
		chatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatWakeupToResponse(ended))
}

func (h *Handler) CheckInChatTurnWakeup(w http.ResponseWriter, r *http.Request) {
	task, id, ok := h.chatTurnWakeupID(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.chatWakeups().CheckIn(r.Context(), task, id, body.Note)
	if err != nil {
		chatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatWakeupToResponse(updated))
}

// ---- Member endpoints: the session creator's own conversations ----

func (h *Handler) ListChatSessionWakeups(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, ctxWorkspaceID(r.Context()), chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	h.listChatWakeups(w, r, session.ID)
}

// chatSessionWakeupForUser resolves a member-facing wakeup action. The
// conversation's owner may act on any of its wakeups, and the person who asked
// for one (in a group chat, not necessarily the owner) may act on theirs.
func (h *Handler) chatSessionWakeupForUser(w http.ResponseWriter, r *http.Request) (pgtype.UUID, pgtype.UUID, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	sessionID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sessionId"), "session id")
	if !ok {
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "wakeupId"), "wakeup id")
	if !ok {
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	workspaceID := ctxWorkspaceID(r.Context())
	wakeup, err := h.Queries.GetChatWakeupInSession(r.Context(), db.GetChatWakeupInSessionParams{ID: id, ChatSessionID: sessionID})
	if err == nil && uuidToString(wakeup.WorkspaceID) == workspaceID && uuidToString(wakeup.RequesterID) == userID {
		return sessionID, id, true
	}
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	return session.ID, id, true
}

func (h *Handler) CancelChatSessionWakeup(w http.ResponseWriter, r *http.Request) {
	sessionID, id, ok := h.chatSessionWakeupForUser(w, r)
	if !ok {
		return
	}
	ended, err := h.chatWakeups().Cancel(r.Context(), sessionID, id, "user")
	if err != nil {
		chatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatWakeupToResponse(ended))
}

// RescheduleChatSessionWakeup changes when an active wakeup runs: a reminder's
// time, a check's interval or deadline, or a condition's deadline.
func (h *Handler) RescheduleChatSessionWakeup(w http.ResponseWriter, r *http.Request) {
	sessionID, id, ok := h.chatSessionWakeupForUser(w, r)
	if !ok {
		return
	}
	var timing service.ChatWakeupTiming
	if err := json.NewDecoder(r.Body).Decode(&timing); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.chatWakeups().Reschedule(r.Context(), sessionID, id, timing)
	if err != nil {
		chatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatWakeupToResponse(updated))
}

// RetryChatSessionWakeup re-runs a wakeup whose report turn failed.
func (h *Handler) RetryChatSessionWakeup(w http.ResponseWriter, r *http.Request) {
	sessionID, id, ok := h.chatSessionWakeupForUser(w, r)
	if !ok {
		return
	}
	retried, err := h.chatWakeups().Retry(r.Context(), sessionID, id)
	if err != nil {
		chatWakeupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chatWakeupToResponse(retried))
}

// ListMyChatWakeups lists the wakeups across the caller's conversations in
// this workspace.
func (h *Handler) ListMyChatWakeups(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	if !h.requireChatOwnerAccess(w, r, userID, workspaceID) {
		return
	}
	workspaceUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	owner, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	rows, err := h.Queries.ListChatWakeupsForSessionOwner(r.Context(), db.ListChatWakeupsForSessionOwnerParams{WorkspaceID: workspaceUUID, OwnerID: owner})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chat wakeups")
		return
	}
	resp := make([]ChatWakeupResponse, 0, len(rows))
	for _, row := range rows {
		item := chatWakeupToResponse(db.ChatWakeup{
			ID: row.ID, WorkspaceID: row.WorkspaceID, ChatSessionID: row.ChatSessionID, AgentID: row.AgentID,
			RequesterID: row.RequesterID, SourceTaskID: row.SourceTaskID, SourceMessageID: row.SourceMessageID,
			Title: row.Title, Instruction: row.Instruction, Kind: row.Kind, Condition: row.Condition,
			ConditionState: row.ConditionState, IntervalSeconds: row.IntervalSeconds, FireAt: row.FireAt,
			NextEvalAt: row.NextEvalAt, ExpiresAt: row.ExpiresAt, Status: row.Status, EndReason: row.EndReason,
			EndedAt: row.EndedAt, CheckCount: row.CheckCount, LastCheckNote: row.LastCheckNote,
			LastCheckedAt: row.LastCheckedAt, LastCheckinTaskID: row.LastCheckinTaskID, LastTaskID: row.LastTaskID,
			LastFiredAt: row.LastFiredAt, LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
		item.LastTaskStatus = row.LastTaskStatus
		item.ChatSessionTitle = row.ChatSessionTitle
		resp = append(resp, item)
	}
	writeJSON(w, http.StatusOK, resp)
}
>>>>>>> 35b54a3742bb

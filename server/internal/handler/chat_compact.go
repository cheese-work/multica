package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

type ChatCompactResponse struct {
	Session          ChatSessionResponse `json:"session"`
	SummaryMessageID string              `json:"summary_message_id"`
}

// CompactChatSession compacts a DM in place: the running chat task stores its
// summary in the same DM and opens a new compaction epoch. The DM is neither
// replaced nor archived. Only the DM's own running task may call it; the DM's
// human compacts through ContinueChatSession. Both share compactChatInPlace.
func (h *Handler) CompactChatSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "only the DM's running chat task can compact")
		return
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(w, http.StatusForbidden, "a session chat task is required")
		return
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil || !task.ChatSessionID.Valid {
		writeErrorCode(w, http.StatusForbidden, codeChatTaskNotActive, "a running session chat task is required")
		return
	}
	session, err := h.Queries.GetChatSession(r.Context(), task.ChatSessionID)
	if err != nil {
		writeErrorCode(w, http.StatusForbidden, codeChatTaskSessionMismatch, "task does not belong to this chat session")
		return
	}
	if !h.authorizeChatTask(r.Context(), w, h.Queries, chatOpCompact, task, session, ctxWorkspaceID(r.Context())) {
		return
	}
	summaryText, ok := decodeCompactSummary(w, r)
	if !ok {
		return
	}
	session, summary, fail := h.compactChatInPlace(r.Context(), session.ID, task.ID, summaryText)
	if fail != nil {
		fail.write(w)
		return
	}
	writeJSON(w, http.StatusOK, ChatCompactResponse{Session: chatSessionToResponse(session), SummaryMessageID: uuidToString(summary.ID)})
}

// decodeCompactSummary reads the request's summary, bounded in size.
func decodeCompactSummary(w http.ResponseWriter, r *http.Request) (string, bool) {
	var request struct {
		Summary string `json:"summary"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || strings.TrimSpace(request.Summary) == "" || len(request.Summary) > 64<<10 {
		writeError(w, http.StatusBadRequest, "a non-empty summary of at most 65536 bytes is required")
		return "", false
	}
	return request.Summary, true
}

type compactFailure struct {
	status  int
	code    string // stable error code (DX-4); empty for an uncoded failure
	message string
}

func (f *compactFailure) write(w http.ResponseWriter) {
	if f.code == "" {
		writeError(w, f.status, f.message)
		return
	}
	writeErrorCode(w, f.status, f.code, f.message)
}

// compactChatInPlace is the one compaction path: it stores the summary as a
// divider in the DM and opens the next epoch, all in one transaction under the
// DM lock. taskID is the compacting chat task, or invalid when the DM's human
// compacts. A task is authorized again under the lock (SEC-1 rule 5). The DM stays active and keeps its pins, decision lane and queued
// turns. Rule E: the epoch cannot move while another chat task of the DM is
// claimed or running; a queued turn is restamped when it is claimed.
func (h *Handler) compactChatInPlace(ctx context.Context, sessionID, taskID pgtype.UUID, summaryText string) (db.ChatSession, db.ChatMessage, *compactFailure) {
	fail := func(status int, message string) (db.ChatSession, db.ChatMessage, *compactFailure) {
		return db.ChatSession{}, db.ChatMessage{}, &compactFailure{status: status, message: message}
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return fail(http.StatusInternalServerError, "failed to start compaction")
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	session, err := qtx.LockChatSessionForEnqueue(ctx, sessionID)
	if err != nil {
		return fail(http.StatusNotFound, "chat session not found")
	}
	if session.Status != "active" {
		return fail(http.StatusConflict, "only an active DM can be compacted")
	}
	// The caller's task status read is stale by now: the task may have been
	// cancelled or completed while the body decoded and the DM lock was awaited,
	// and a successor may already hold the old epoch. Lock the live task (DM
	// first, then task) and read its status again.
	if taskID.Valid {
		task, err := qtx.LockAgentTaskForCompaction(ctx, taskID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" { // lock_not_available
				return fail(http.StatusConflict, "the task is changing state; retry if it is still running")
			}
			return fail(http.StatusInternalServerError, "failed to lock the compacting task")
		}
		if refusal := h.decideChatTask(ctx, qtx, chatOpCompact, task, session, uuidToString(session.WorkspaceID)); refusal != nil {
			status := http.StatusForbidden
			if refusal.Code == codeChatTaskNotActive {
				status = http.StatusConflict // lost the race with a terminal transition
			}
			return db.ChatSession{}, db.ChatMessage{}, &compactFailure{status: status, code: refusal.Code, message: refusal.Message}
		}
	}
	// A human request has no task to exclude: the zero UUID matches none.
	otherActive, err := qtx.CountOtherActiveChatTasks(ctx, db.CountOtherActiveChatTasksParams{ChatSessionID: session.ID, ID: pgtype.UUID{Bytes: taskID.Bytes, Valid: true}})
	if err != nil {
		return fail(http.StatusInternalServerError, "failed to check active chat tasks")
	}
	if otherActive > 0 {
		return fail(http.StatusConflict, "another reply is in progress; compact after it finishes")
	}
	if _, err := qtx.GetChannelChatSessionBindingBySessionAny(ctx, session.ID); !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			return fail(http.StatusInternalServerError, "failed to check DM source")
		}
		return fail(http.StatusConflict, "compact is only available for web DMs")
	}
	// A retry from the task that already wrote the summary changes nothing.
	if taskID.Valid {
		existing, err := qtx.GetTaskCompactionSummary(ctx, db.GetTaskCompactionSummaryParams{ChatSessionID: session.ID, TaskID: taskID})
		if err == nil {
			return session, existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fail(http.StatusInternalServerError, "failed to check existing summary")
		}
	}
	// The server's decision records lead the stored summary: the seed cuts a
	// too-long summary from its tail, never the decisions.
	decisions, err := compactionDecisionsBlock(ctx, qtx, session)
	if err != nil {
		return fail(http.StatusInternalServerError, "failed to load decision records")
	}
	if decisions != "" {
		summaryText = decisions + "\n" + summaryText
	}
	summary, err := qtx.CreateChatCompactionDivider(ctx, db.CreateChatCompactionDividerParams{
		ID: dbid.NewV7(), ChatSessionID: session.ID,
		Content: strings.TrimSpace(redact.Prose(summaryText)),
		TaskID:  taskID,
	})
	if err == nil {
		session, err = qtx.CompactChatSessionInPlace(ctx, db.CompactChatSessionInPlaceParams{ID: session.ID, DividerID: summary.ID})
	}
	if err != nil {
		return fail(http.StatusInternalServerError, "failed to compact chat session")
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(http.StatusInternalServerError, "failed to commit compaction")
	}
	id := uuidToString(session.ID)
	h.publishChatFor(protocol.EventChatMessage, uuidToString(session.WorkspaceID), "agent", uuidToString(session.AgentID), uuidToString(session.CreatorID), id, compactionSummaryPayload(id, summary, uuidToString(taskID)))
	return session, summary, nil
}

// compactionSummaryPayload is the live event for a new compaction summary.
func compactionSummaryPayload(sessionID string, summary db.ChatMessage, taskID string) protocol.ChatMessagePayload {
	return protocol.ChatMessagePayload{
		ChatSessionID: sessionID, MessageID: uuidToString(summary.ID), Role: summary.Role, Content: summary.Content,
		TaskID: taskID, CreatedAt: timestampToString(summary.CreatedAt),
		MessageKind: protocol.ChatMessageKindCompactionSummary,
	}
}

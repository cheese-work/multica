package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Head-task steer readiness reported on PendingChatTaskResponse.
const (
	HeadSupplementCapable     = "capable"
	HeadSupplementStarting    = "starting"
	HeadSupplementUnsupported = "unsupported"
)

// ChatSupplementResponse is the delivery receipt of a steered DM follow-up.
type ChatSupplementResponse struct {
	MessageID          string  `json:"message_id"`
	ChatSessionID      string  `json:"chat_session_id"`
	TaskID             string  `json:"task_id"`
	Status             string  `json:"status"`
	FailureReason      *string `json:"failure_reason"`
	DeliveredAt        *string `json:"delivered_at"`
	AckedAfterResendAt *string `json:"acked_after_resend_at"`
	ResentTaskID       *string `json:"resent_task_id"`
}

// ResendChatSupplementResponse names the queued task that now owns the message.
type ResendChatSupplementResponse struct {
	TaskID     string                 `json:"task_id"`
	Supplement ChatSupplementResponse `json:"supplement"`
}

func chatSupplementToResponse(s db.ChatTaskSupplement) ChatSupplementResponse {
	return ChatSupplementResponse{
		MessageID:          uuidToString(s.MessageID),
		ChatSessionID:      uuidToString(s.ChatSessionID),
		TaskID:             uuidToString(s.TaskID),
		Status:             s.Status,
		FailureReason:      textToPtr(s.FailureReason),
		DeliveredAt:        timestampToPtr(s.DeliveredAt),
		AckedAfterResendAt: timestampToPtr(s.AckedAfterResendAt),
		ResentTaskID:       uuidToPtr(s.ResentTaskID),
	}
}

// SteerChatTask adds the message of a queued follow-up to the running task.
func (h *Handler) SteerChatTask(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	taskID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "taskId"), "task id")
	if !ok {
		return
	}
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	if _, _, _, ok := h.preflightChatSend(w, r, session, userID, workspaceID); !ok {
		return
	}
	result, err := h.TaskService.SteerChatTask(r.Context(), session, taskID, parseUUID(userID))
	if err != nil {
		h.writeChatSupplementError(w, r, err, "steer chat message")
		return
	}
	if result.Cancelled != nil {
		h.publishChatSupplement(result.Receipt, session.CreatorID, "member", userID)
		if h.DaemonTaskSupplement != nil && result.RunningTask.RuntimeID.Valid {
			h.DaemonTaskSupplement.NotifyTaskSupplementAvailable(uuidToString(result.RunningTask.RuntimeID), uuidToString(result.RunningTask.ID))
		}
	}
	writeJSON(w, http.StatusOK, chatSupplementToResponse(result.Receipt))
}

// ResendChatSupplement queues a steered message as a new turn.
func (h *Handler) ResendChatSupplement(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	messageID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "messageId"), "message id")
	if !ok {
		return
	}
	var req struct {
		ConfirmPossibleDuplicate bool `json:"confirm_possible_duplicate"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	agent, _, _, ok := h.preflightChatSend(w, r, session, userID, workspaceID)
	if !ok {
		return
	}
	result, err := h.TaskService.ResendChatSupplement(r.Context(), session, agent, messageID, parseUUID(userID), req.ConfirmPossibleDuplicate)
	if err != nil {
		h.writeChatSupplementError(w, r, err, "resend chat message")
		return
	}
	if result.Created {
		h.publishChatSupplement(result.Receipt, session.CreatorID, "member", userID)
	}
	writeJSON(w, http.StatusOK, ResendChatSupplementResponse{
		TaskID:     uuidToString(result.Task.ID),
		Supplement: chatSupplementToResponse(result.Receipt),
	})
}

func (h *Handler) writeChatSupplementError(w http.ResponseWriter, r *http.Request, err error, action string) {
	if code, ok := service.ChatSupplementConflictCode(err); ok {
		writeErrorCode(w, http.StatusConflict, code, "cannot "+action+": "+strings.ReplaceAll(code, "_", " "))
		return
	}
	switch {
	case errors.Is(err, service.ErrChatSupplementNotFound):
		writeError(w, http.StatusNotFound, "chat message was not added to a run")
	case errors.Is(err, service.ErrChatSessionArchived):
		writeError(w, http.StatusConflict, "chat session is archived")
	case errors.Is(err, service.ErrChatTaskAgentArchived):
		writeError(w, http.StatusConflict, "chat agent is archived")
	case errors.Is(err, service.ErrChatTaskAgentNoRuntime):
		writeError(w, http.StatusConflict, "chat agent has no runtime")
	case errors.Is(err, service.ErrChatCompacting):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "chat is compacting", "code": "chat_compacting", "reason_code": "chat_compacting"})
	default:
		slog.Warn(action+" failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to "+action)
	}
}

// publishChatSupplement tells the session creator a receipt changed.
func (h *Handler) publishChatSupplement(receipt db.ChatTaskSupplement, ownerID pgtype.UUID, actorType, actorID string) {
	h.publishChatFor(protocol.EventChatSupplementUpdated, uuidToString(receipt.WorkspaceID), actorType, actorID,
		uuidToString(ownerID), uuidToString(receipt.ChatSessionID),
		map[string]any{"supplement": chatSupplementToResponse(receipt)})
}

// chatSupplementsForMessages maps a user message id to its latest receipt.
func (h *Handler) chatSupplementsForMessages(ctx context.Context, workspaceID string, messages []db.ChatMessage) map[string]*ChatSupplementResponse {
	var ids []pgtype.UUID
	for _, m := range messages {
		if m.Role == "user" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := h.Queries.ListChatTaskSupplementsByMessageIDs(ctx, db.ListChatTaskSupplementsByMessageIDsParams{
		WorkspaceID: parseUUID(workspaceID), MessageIds: ids,
	})
	if err != nil {
		slog.Warn("list chat supplements failed", "error", err)
		return nil
	}
	out := make(map[string]*ChatSupplementResponse, len(rows))
	for _, row := range rows {
		resp := chatSupplementToResponse(row)
		out[resp.MessageID] = &resp
	}
	return out
}

// headSupplementState reports whether the head task can take a steer now.
func (h *Handler) headSupplementState(ctx context.Context, headID pgtype.UUID, status string) string {
	if status != "running" {
		return HeadSupplementStarting
	}
	capability, err := h.Queries.GetTaskSupplementCapability(ctx, headID)
	if err == nil && capability.Capability == protocol.DaemonCapabilityTaskSupplementChatV1 {
		return HeadSupplementCapable
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// Unknown is "wait", never a false "unsupported".
		return HeadSupplementStarting
	}
	return HeadSupplementUnsupported
}

// resentUncertainChatMessages returns the input messages of taskID that were
// resent after a delivery to an earlier run could not be confirmed.
func (h *Handler) resentUncertainChatMessages(ctx context.Context, taskID pgtype.UUID, messages []db.ChatMessage) (map[string]bool, error) {
	if len(messages) == 0 {
		return nil, nil
	}
	ids := make([]pgtype.UUID, len(messages))
	for i, m := range messages {
		ids[i] = m.ID
	}
	rows, err := h.Queries.ListResentUncertainChatMessageIDs(ctx, db.ListResentUncertainChatMessageIDsParams{TaskID: taskID, MessageIds: ids})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, id := range rows {
		out[uuidToString(id)] = true
	}
	return out, nil
}

func withResentUncertainNotice(content string, resent bool) string {
	if !resent {
		return content
	}
	if strings.TrimSpace(content) == "" {
		return service.ChatSupplementResentUncertainNotes
	}
	return content + "\n\n" + service.ChatSupplementResentUncertainNotes
}

// chatSupplementAttachmentBlock matches the first-turn prompt's attachment
// block, so a steered attachment reads the same as a sent one.
func chatSupplementAttachmentBlock(atts []db.Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nAttachments on this message:\n")
	for _, a := range atts {
		if a.ContentType != "" {
			fmt.Fprintf(&b, "- id=%s filename=%q content_type=%s\n", uuidToString(a.ID), a.Filename, a.ContentType)
		} else {
			fmt.Fprintf(&b, "- id=%s filename=%q\n", uuidToString(a.ID), a.Filename)
		}
	}
	b.WriteString("Use `multica attachment download <id>` to fetch each file locally before referring to it.\n")
	b.WriteString("When creating an issue that should preserve one of these attachments, pass `--attachment-id <id>` to `multica issue create` in addition to keeping the attachment markdown inline.\n")
	return b.String()
}

func (h *Handler) claimChatTaskSupplement(w http.ResponseWriter, r *http.Request, task db.AgentTaskQueue) {
	row, err := h.Queries.ClaimNextChatTaskSupplement(r.Context(), task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to claim additional message")
		return
	}
	// The claimed text is chat content. Take the chat's taint after the claim so
	// a delegate this run starts later cannot be elevated (CHE-3702).
	if err := h.recordChatTaint(r.Context(), &task, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record chat taint")
		return
	}
	atts, err := h.Queries.ListAttachmentsByChatMessageIDs(r.Context(), db.ListAttachmentsByChatMessageIDsParams{
		Column1: []pgtype.UUID{row.MessageID}, WorkspaceID: row.WorkspaceID,
	})
	if err != nil {
		// The row is already delivering; settlement or a late ack resolves it.
		writeError(w, http.StatusInternalServerError, "failed to load additional message attachments")
		return
	}
	content := row.Content
	if block := chatSupplementAttachmentBlock(atts); block != "" {
		if strings.TrimSpace(content) == "" {
			content = strings.TrimPrefix(block, "\n")
		} else {
			content += "\n" + block
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"comment_id": uuidToString(row.MessageID), "author_type": "member", "author_name": row.AuthorName,
		"content": content,
	})
}

func (h *Handler) ackChatTaskSupplement(w http.ResponseWriter, r *http.Request, task db.AgentTaskQueue, messageID pgtype.UUID, req ackTaskSupplementRequest) {
	var row db.ChatTaskSupplement
	var err error
	if req.Delivered {
		row, err = h.Queries.AckChatTaskSupplementDelivered(r.Context(), db.AckChatTaskSupplementDeliveredParams{
			TaskID: task.ID, MessageID: messageID,
		})
	} else {
		row, err = h.Queries.AckChatTaskSupplementNotDelivered(r.Context(), db.AckChatTaskSupplementNotDeliveredParams{
			TaskID: task.ID, MessageID: messageID, FailureReason: stableTaskSupplementFailureReason(req.Error),
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "additional message is no longer deliverable")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to acknowledge additional message")
		return
	}
	if session, err := h.Queries.GetChatSession(r.Context(), row.ChatSessionID); err == nil {
		h.publishChatSupplement(row, session.CreatorID, "system", "")
	}
	writeJSON(w, http.StatusOK, chatSupplementToResponse(row))
}

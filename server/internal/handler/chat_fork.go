package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/weblink"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// forkRequestMaxBytes bounds the fork body. A normal chat send has no message
// size limit of its own, so this body cap is the bound on the fork message.
const forkRequestMaxBytes = 1 << 20

// forkTitleMessageRunes is how much of the fork message the child title shows.
const forkTitleMessageRunes = 40

type ChatForkResponse struct {
	Session           ChatSessionResponse  `json:"session"`
	PreviousSessionID string               `json:"previous_session_id"`
	URL               string               `json:"url"`
	WebURL            string               `json:"web_url,omitempty"`
	Message           *ChatMessageResponse `json:"message,omitempty"`
	// AttachmentIDs are the draft attachments bound to the first message.
	AttachmentIDs []string `json:"attachment_ids,omitempty"`
}

// ForkChatSession copies a web DM into a new child DM: every member-visible
// message and attachment, as history. The source stays active and unchanged.
// With a message, the same transaction sends it as the child's first message
// and enqueues the first turn. The operation key makes a retry return the same
// child without copying or sending again.
func (h *Handler) ForkChatSession(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	session, operationKey, taskScoped, ok := h.resolveForkSource(w, r, userID, workspaceID)
	if !ok {
		return
	}
	var request struct {
		IdempotencyKey string          `json:"idempotency_key"`
		Message        string          `json:"message"`
		Summary        json.RawMessage `json:"summary"`
		AttachmentIDs  []string        `json:"attachment_ids"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, forkRequestMaxBytes)
	decoder := json.NewDecoder(r.Body)
	// The second Decode reads to the end of the body, so the cap covers bytes
	// after the first JSON value too, and trailing data is rejected.
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "a JSON body of at most 1 MiB is required")
		return
	}
	if request.Summary != nil {
		writeError(w, http.StatusBadRequest, "a fork copies the whole DM; summary is not accepted")
		return
	}
	message := strings.TrimSpace(request.Message)
	if taskScoped && (message != "" || len(request.AttachmentIDs) > 0) {
		writeError(w, http.StatusBadRequest, "a chat task cannot send a fork message")
		return
	}
	attachmentIDs, ok := parseUUIDSliceOrBadRequest(w, request.AttachmentIDs, "attachment_ids")
	if !ok {
		return
	}
	if !utf8.ValidString(message) || strings.ContainsRune(message, '\x00') {
		writeError(w, http.StatusBadRequest, "message must be UTF-8 without NUL characters")
		return
	}
	if request.IdempotencyKey != "" {
		operationKey = request.IdempotencyKey
	}
	if strings.TrimSpace(operationKey) == "" || len(operationKey) > 128 || !utf8.ValidString(operationKey) || strings.ContainsRune(operationKey, '\x00') {
		writeError(w, http.StatusBadRequest, "an idempotency_key of at most 128 bytes is required")
		return
	}
	key := pgtype.Text{String: operationKey, Valid: true}
	operation := db.GetChatForkByOperationParams{ForkedFromSessionID: session.ID, ForkIdempotencyKey: key}

	// A replay returns before the send preflight, so a stored fork stays
	// reachable after its agent changes.
	if child, err := h.Queries.GetChatForkByOperation(r.Context(), operation); err == nil {
		h.writeForkReplay(w, r, session, child)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to check fork operation")
		return
	}
	agent, err := h.Queries.GetAgent(r.Context(), session.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load chat agent")
		return
	}
	if isAgentBuilderCarrier(agent) {
		writeError(w, http.StatusConflict, "agent-builder DMs cannot be forked")
		return
	}
	childID := dbid.NewV7()
	var plan service.DirectChatSendPlan
	if message != "" || len(attachmentIDs) > 0 {
		if agent, _, _, ok = h.preflightChatSend(w, r, session, userID, workspaceID); !ok {
			return
		}
		if plan, err = h.TaskService.PlanDirectChatSend(r.Context(), childID, agent, parseUUID(userID)); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to prepare the fork message")
			return
		}
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start fork")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := qtx.LockWorkspaceForChatSessionCreate(r.Context(), session.WorkspaceID); err != nil {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if session.ProjectID.Valid {
		if _, err := qtx.LockProjectForChatSessionCreate(r.Context(), db.LockProjectForChatSessionCreateParams{ID: session.ProjectID, WorkspaceID: session.WorkspaceID}); err != nil {
			writeError(w, http.StatusConflict, "the source project is no longer available")
			return
		}
	}
	// The source row lock serializes forks of one source, so the operation
	// lookup below sees any fork that committed first.
	session, err = qtx.LockChatSessionForEnqueue(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "chat session not found")
		return
	}
	if child, err := qtx.GetChatForkByOperation(r.Context(), operation); err == nil {
		h.writeForkReplay(w, r, session, child)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to check fork operation")
		return
	}
	if session.Status != "active" {
		writeError(w, http.StatusConflict, "only an active DM can be forked")
		return
	}
	if session.ChannelOriginAt.Valid {
		writeError(w, http.StatusConflict, "fork is only available for web DMs")
		return
	}
	if _, err := qtx.GetChannelChatSessionBindingBySessionAny(r.Context(), session.ID); !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check DM source")
		} else {
			writeError(w, http.StatusConflict, "fork is only available for web DMs")
		}
		return
	}
	sources, err := qtx.ListChatMessagesForFork(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the DM")
		return
	}
	if len(sources) == 0 {
		writeError(w, http.StatusConflict, "nothing to fork yet")
		return
	}
	workspace, err := qtx.GetWorkspace(r.Context(), session.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workspace")
		return
	}
	copied := copyForkRows(sources)
	attachments, err := qtx.LockChatAttachmentsForFork(r.Context(), db.LockChatAttachmentsForForkParams{WorkspaceID: session.WorkspaceID, ChatMessageIds: copied.sourceIDs})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the DM attachments")
		return
	}
	attachmentCopy := copied.copyAttachments(attachments)

	child, err := qtx.CreateChatSession(r.Context(), db.CreateChatSessionParams{
		ID: childID, WorkspaceID: session.WorkspaceID, AgentID: session.AgentID,
		CreatorID: session.CreatorID, Title: forkTitle(session.Title, message), ProjectID: session.ProjectID,
	})
	if err == nil {
		err = qtx.InsertForkChatMessages(r.Context(), db.InsertForkChatMessagesParams{
			Ids: copied.ids, ChatSessionID: child.ID, Contents: copied.contents, MessageKinds: copied.kinds, SourceIds: copied.sourceIDs,
		})
	}
	if err == nil && len(attachmentCopy.ids) > 0 {
		err = qtx.InsertForkChatAttachments(r.Context(), db.InsertForkChatAttachmentsParams{
			Ids: attachmentCopy.ids, ChatSessionID: child.ID, ChatMessageIds: attachmentCopy.messageIDs, SourceIds: attachmentCopy.sourceIDs,
		})
	}
	if err == nil {
		child, err = qtx.SetChatForkSource(r.Context(), db.SetChatForkSourceParams{
			ID: child.ID, ForkedFromSessionID: session.ID, ForkIdempotencyKey: key,
			ForkLastMessageID: copied.ids[len(copied.ids)-1], ForkSourceMessageID: copied.sourceIDs[len(copied.sourceIDs)-1], ForkMessageCount: pgtype.Int4{Int32: int32(len(copied.ids)), Valid: true},
		})
	}
	// Drafts uploaded in the source DM move to the child so the first send can
	// bind them. A fork whose only attachments cannot move sends nothing.
	var movable []pgtype.UUID
	if err == nil && len(attachmentIDs) > 0 {
		movable, err = qtx.MoveDraftAttachmentsToForkChild(r.Context(), db.MoveDraftAttachmentsToForkChildParams{
			ChildSessionID: child.ID, WorkspaceID: session.WorkspaceID, SourceSessionID: session.ID,
			UploaderID: parseUUID(userID), AttachmentIds: attachmentIDs,
		})
	}
	sendFirst := message != "" || len(movable) > 0
	var sent service.DirectChatSendResult
	if err == nil && sendFirst {
		sent, err = h.TaskService.SendDirectChatMessageTx(r.Context(), qtx, plan, child, parseUUID(userID), message, movable, "member", parseUUID(userID), r.Header.Get("X-Actor-Source"))
		if err == nil {
			sent.Message, err = qtx.PlaceForkFirstMessage(r.Context(), sent.Message.ID)
		}
	}
	if err != nil {
		slog.Error("chat fork failed", "source_session_id", uuidToString(session.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to fork chat session")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit fork")
		return
	}

	slog.Info("chat fork created",
		"source_session_id", uuidToString(session.ID), "child_session_id", uuidToString(child.ID),
		"copied_messages", len(copied.ids), "copied_attachments", len(attachmentCopy.ids),
		"copy_duration_ms", time.Since(started).Milliseconds(), "replay", false, "first_message", sendFirst)
	creatorID := uuidToString(session.CreatorID)
	h.publishChat(protocol.EventChatSessionCreated, workspaceID, "member", creatorID, uuidToString(child.ID), protocol.ChatSessionCreatedPayload{
		WorkspaceID: workspaceID, ChatSessionID: uuidToString(child.ID), AgentID: uuidToString(child.AgentID), CreatorID: creatorID, Title: child.Title,
	})
	response := ChatForkResponse{Session: chatSessionToResponse(child), PreviousSessionID: uuidToString(session.ID), URL: forkChatURL(workspace.Slug, child.ID), WebURL: weblink.Build(h.cfg.AppURL, forkChatURL(workspace.Slug, child.ID))}
	if sendFirst {
		h.TaskService.RecordHumanDelegation(r.Context(), plan.Attribution)
		h.publishChat(protocol.EventChatMessage, workspaceID, "member", userID, uuidToString(child.ID), protocol.ChatMessagePayload{
			ChatSessionID: uuidToString(child.ID), MessageID: uuidToString(sent.Message.ID), Role: "user",
			Content: message, TaskID: uuidToString(sent.Task.ID), CreatedAt: timestampToString(sent.Message.CreatedAt),
		})
		h.TaskService.NotifyDirectChatSent(r.Context(), sent.Task)
		first := chatMessageToResponse(sent.Message, nil)
		response.Message = &first
		response.AttachmentIDs = uuidsToStrings(sent.BoundAttachmentIDs)
	}
	writeJSON(w, http.StatusCreated, response)
}

// resolveForkSource authorizes the caller. A chat task forks the session that
// owns it, keyed by its input batch owner so an auto-retry of the same turn
// returns the same child; a person forks a DM they own and supplies the key.
func (h *Handler) resolveForkSource(w http.ResponseWriter, r *http.Request, userID, workspaceID string) (db.ChatSession, string, bool, bool) {
	sessionID := chi.URLParam(r, "sessionId")
	if r.Header.Get("X-Actor-Source") != "task_token" {
		if r.Header.Get("X-Agent-ID") != "" {
			writeError(w, http.StatusForbidden, "only the session human or its chat task can fork")
			return db.ChatSession{}, "", false, false
		}
		session, ok := h.gateChatSessionForUser(w, r, userID, workspaceID, sessionID)
		return session, "", false, ok
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(w, http.StatusForbidden, "a session chat task is required")
		return db.ChatSession{}, "", true, false
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil || !task.ChatSessionID.Valid || (task.Status != "running" && task.Status != "dispatched") {
		writeError(w, http.StatusForbidden, "a running session chat task is required")
		return db.ChatSession{}, "", true, false
	}
	if sessionID != "" && sessionID != uuidToString(task.ChatSessionID) {
		writeError(w, http.StatusForbidden, "task does not belong to this chat session")
		return db.ChatSession{}, "", true, false
	}
	session, err := h.Queries.GetChatSession(r.Context(), task.ChatSessionID)
	if err != nil || uuidToString(session.WorkspaceID) != workspaceID || session.AgentID != task.AgentID {
		writeError(w, http.StatusForbidden, "task does not belong to this chat session")
		return db.ChatSession{}, "", true, false
	}
	key := task.ID
	if task.ChatInputTaskID.Valid {
		key = task.ChatInputTaskID
	}
	return session, uuidToString(key), true, true
}

func (h *Handler) writeForkReplay(w http.ResponseWriter, r *http.Request, source, child db.ChatSession) {
	if child.CreatorID != source.CreatorID || child.WorkspaceID != source.WorkspaceID {
		writeError(w, http.StatusConflict, "the fork is no longer available")
		return
	}
	workspace, err := h.Queries.GetWorkspace(r.Context(), child.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workspace")
		return
	}
	response := ChatForkResponse{Session: chatSessionToResponse(child), PreviousSessionID: uuidToString(source.ID), URL: forkChatURL(workspace.Slug, child.ID), WebURL: weblink.Build(h.cfg.AppURL, forkChatURL(workspace.Slug, child.ID))}
	if first, err := h.Queries.GetChatForkFirstMessage(r.Context(), child.ID); err == nil {
		message := chatMessageToResponse(first, nil)
		response.Message = &message
		bound, err := h.Queries.ListAttachmentsByChatMessage(r.Context(), db.ListAttachmentsByChatMessageParams{ChatMessageID: first.ID, WorkspaceID: child.WorkspaceID})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load the fork message attachments")
			return
		}
		for _, attachment := range bound {
			response.AttachmentIDs = append(response.AttachmentIDs, uuidToString(attachment.ID))
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to load the fork message")
		return
	}
	slog.Info("chat fork replayed", "source_session_id", uuidToString(source.ID), "child_session_id", uuidToString(child.ID), "replay", true)
	writeJSON(w, http.StatusOK, response)
}

func forkChatURL(slug string, id pgtype.UUID) string {
	return "/" + url.PathEscape(slug) + "/chat?session=" + uuidToString(id)
}

var (
	markdownLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	markdownPrefix = regexp.MustCompile(`^(?:>\s*|#{1,6}\s+|[-*+]\s+|\d+[.)]\s+)+`)
)

// plainText reduces markdown to what a reader sees in a title: link and slash
// chips become their label; fence lines and leading quote, heading and list
// markers are dropped.
func plainText(markdown string) string {
	var lines []string
	for _, line := range strings.Split(markdown, "\n") {
		line = markdownPrefix.ReplaceAllString(strings.TrimSpace(line), "")
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			continue
		}
		lines = append(lines, markdownLink.ReplaceAllString(line, "$1"))
	}
	return strings.Join(lines, " ")
}

// forkTitle is "<source> (fork: <message>)", the message as plain text,
// whitespace-collapsed and cut at forkTitleMessageRunes with "…", or "<source> (fork)" without one.
func forkTitle(source, message string) string {
	if message == "" {
		return strings.TrimSpace(source + " (fork)")
	}
	label := strings.Join(strings.Fields(plainText(message)), " ")
	if runes := []rune(label); len(runes) > forkTitleMessageRunes {
		label = string(runes[:forkTitleMessageRunes]) + "…"
	}
	return strings.TrimSpace(source + " (fork: " + label + ")")
}

type forkCopy struct {
	ids, sourceIDs  []pgtype.UUID
	contents, kinds []string
	newID           map[pgtype.UUID]pgtype.UUID
}

// copyForkRows assigns new ids in source order, so rows with equal times keep
// their order, and maps the onboarding opening to an ordinary message.
func copyForkRows(sources []db.ChatMessage) forkCopy {
	copied := forkCopy{newID: make(map[pgtype.UUID]pgtype.UUID, len(sources))}
	for _, source := range sources {
		id := dbid.NewV7()
		kind := source.MessageKind
		if kind == protocol.ChatMessageKindOnboardingOpening {
			kind = protocol.ChatMessageKindMessage
		}
		copied.ids = append(copied.ids, id)
		copied.sourceIDs = append(copied.sourceIDs, source.ID)
		copied.contents = append(copied.contents, source.Content)
		copied.kinds = append(copied.kinds, kind)
		copied.newID[source.ID] = id
	}
	return copied
}

type forkAttachmentCopy struct {
	ids, sourceIDs, messageIDs []pgtype.UUID
}

// copyAttachments gives each source attachment a new id on its copied message
// and rewrites download links in copied text to the new ids.
func (copied *forkCopy) copyAttachments(attachments []db.Attachment) forkAttachmentCopy {
	var result forkAttachmentCopy
	var pairs []string
	for _, attachment := range attachments {
		id := dbid.NewV7()
		result.ids = append(result.ids, id)
		result.sourceIDs = append(result.sourceIDs, attachment.ID)
		result.messageIDs = append(result.messageIDs, copied.newID[attachment.ChatMessageID])
		pairs = append(pairs, "/api/attachments/"+uuidToString(attachment.ID)+"/", "/api/attachments/"+uuidToString(id)+"/")
	}
	if len(pairs) == 0 {
		return result
	}
	links := strings.NewReplacer(pairs...)
	for index, content := range copied.contents {
		if strings.Contains(content, "/api/attachments/") {
			copied.contents[index] = links.Replace(content)
		}
	}
	return result
}

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Human actions on DM attention (CHE-4147, plan r4 T3-T5): Xong, Để sau, Xong tất cả + undo.
// The client sends the exact subject keys it displayed; the server acts on those keys only,
// so an obligation that arrived after the snapshot is never touched.

const (
	attentionMaxKeys     = 500
	attentionLaterMax    = 168 * time.Hour
	attentionLaterTarget = 2 * time.Hour
	attentionMaxBody     = 256 << 10 // bytes; 500 keys need about 30 KB
)

type chatAttentionResponse struct {
	RootID          string         `json:"root_id"`
	CanonicalID     string         `json:"canonical_id"`
	StaleLevel      string         `json:"stale_level"`
	AttentionSource *string        `json:"attention_source"`
	AttentionSince  *time.Time     `json:"attention_since"`
	Counts          map[string]int `json:"counts"`
	Keys            []string       `json:"keys"`
	NextChangeAt    *time.Time     `json:"next_change_at"`
}

func attentionResponse(a service.ChatAttention) chatAttentionResponse {
	keys := a.Keys
	if keys == nil {
		keys = []string{}
	}
	return chatAttentionResponse{
		RootID: a.RootID, CanonicalID: a.CanonicalID, StaleLevel: a.StaleLevel,
		AttentionSource: a.AttentionSource, AttentionSince: a.AttentionSince, Keys: keys, NextChangeAt: a.NextChangeAt,
		Counts: map[string]int{
			"ask": a.Counts.Ask, "later": a.Counts.Later, "unseen": a.Counts.Unseen,
			"decision": a.Counts.Decision, "waiting": a.Counts.Waiting, "unclassified": a.Counts.Unclassified,
		},
	}
}

// attentionSubjects is the parsed, ownership-checked set of keys of one request.
type attentionSubjects struct {
	items, decisions, turns []pgtype.UUID
	keys, roots             []string // parallel: every non-item key and its chain root, for marks
	itemOpen                map[string]bool
	itemRoots               map[string]pgtype.UUID
}

type attentionFailure struct {
	status int
	msg    string
}

func (f *attentionFailure) write(w http.ResponseWriter) { writeError(w, f.status, f.msg) }

// attentionVisibility is the private-agent boundary for attention: the chains whose DM agent the
// caller can still open (`visible`) and those they cannot (`hidden`, for SQL). A chain the caller
// lost access to is neither listed nor actable, the same as the Chat session endpoints.
type attentionVisibility struct {
	visible map[string]bool
	hidden  []pgtype.UUID
}

func (h *Handler) attentionVisibility(r *http.Request, userID, workspaceID string) (*attentionVisibility, *attentionFailure) {
	rows, err := h.Queries.ListAttentionChainAgents(r.Context(), db.ListAttentionChainAgentsParams{WorkspaceID: parseUUID(workspaceID), UserID: parseUUID(userID)})
	if err != nil {
		return nil, &attentionFailure{http.StatusInternalServerError, "failed to resolve agent access"}
	}
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	v := &attentionVisibility{visible: map[string]bool{}, hidden: []pgtype.UUID{}}
	access := map[string]bool{} // by agent
	for _, row := range rows {
		agentID := uuidToString(row.AgentID)
		ok, seen := access[agentID]
		if !seen {
			agent, err := h.Queries.GetAgent(r.Context(), row.AgentID)
			ok = err == nil && h.canAccessPrivateAgent(r.Context(), agent, actorType, actorID, workspaceID)
			access[agentID] = ok
		}
		if ok {
			v.visible[uuidToString(row.RootID)] = true
		} else {
			v.hidden = append(v.hidden, row.RootID)
		}
	}
	return v, nil
}

// decodeAttentionBody reads a bounded JSON body; false means the error response is written.
func decodeAttentionBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, attentionMaxBody)).Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return false
	}
	return true
}

// loadAttentionSubjects parses `item:`, `decision:` and `turn:` keys and proves the user owns each.
// A malformed key is 400. An id that is not the user's, does not exist, or belongs to a DM whose
// agent the user lost access to is 403.
func (h *Handler) loadAttentionSubjects(ctx context.Context, q *db.Queries, workspaceID, userID pgtype.UUID, vis *attentionVisibility, rawKeys []string) (*attentionSubjects, *attentionFailure) {
	if len(rawKeys) > attentionMaxKeys {
		return nil, &attentionFailure{http.StatusUnprocessableEntity, fmt.Sprintf("at most %d subject keys per request", attentionMaxKeys)}
	}
	s := &attentionSubjects{itemOpen: map[string]bool{}, itemRoots: map[string]pgtype.UUID{}}
	seen := map[string]bool{}
	var itemIDs, decisionIDs, turnIDs []pgtype.UUID
	var decisionKeys, turnKeys []string
	for _, rawKey := range rawKeys {
		kind, rest, ok := strings.Cut(rawKey, ":")
		if !ok {
			return nil, &attentionFailure{http.StatusBadRequest, "invalid subject key: " + rawKey}
		}
		idPart, revision := rest, ""
		if kind == "decision" {
			var rev string
			idPart, rev, ok = strings.Cut(rest, "@")
			n, err := strconv.Atoi(rev)
			if !ok || err != nil {
				return nil, &attentionFailure{http.StatusBadRequest, "invalid subject key: " + rawKey}
			}
			revision = "@" + strconv.Itoa(n)
		}
		id, err := util.ParseUUID(idPart)
		if err != nil {
			return nil, &attentionFailure{http.StatusBadRequest, "invalid subject key: " + rawKey}
		}
		// Canonical form: every later lookup and stored mark uses it, so an uppercase or
		// undashed id names the same subject as the lowercase one.
		key := kind + ":" + uuidToString(id) + revision
		if seen[key] {
			continue
		}
		seen[key] = true
		switch kind {
		case "item":
			itemIDs = append(itemIDs, id)
		case "decision":
			decisionIDs, decisionKeys = append(decisionIDs, id), append(decisionKeys, key)
		case "turn":
			turnIDs, turnKeys = append(turnIDs, id), append(turnKeys, key)
		default:
			return nil, &attentionFailure{http.StatusBadRequest, "invalid subject key: " + key}
		}
	}
	fail := &attentionFailure{http.StatusInternalServerError, "failed to load subjects"}
	forbidden := &attentionFailure{http.StatusForbidden, "subject key is not yours"}

	if len(itemIDs) > 0 {
		rows, err := q.ListOwnedAttentionItems(ctx, db.ListOwnedAttentionItemsParams{Ids: itemIDs, UserID: userID, WorkspaceID: workspaceID})
		if err != nil {
			return nil, fail
		}
		if len(rows) != len(itemIDs) {
			return nil, forbidden
		}
		for _, r := range rows {
			if !vis.visible[uuidToString(r.ChainRootSessionID)] {
				return nil, forbidden
			}
			id := uuidToString(r.ID)
			s.itemOpen[id], s.itemRoots[id] = !r.ResolvedAt.Valid, r.ChainRootSessionID
		}
		s.items = itemIDs
	}
	if len(decisionIDs) > 0 {
		rows, err := q.ListOwnedAttentionDecisions(ctx, db.ListOwnedAttentionDecisionsParams{Ids: decisionIDs, UserID: userID, WorkspaceID: workspaceID})
		if err != nil {
			return nil, fail
		}
		if len(rows) != len(decisionIDs) {
			return nil, forbidden
		}
		s.decisions = decisionIDs
		roots := map[string]string{}
		for _, r := range rows {
			if !vis.visible[uuidToString(r.ChainRootSessionID)] {
				return nil, forbidden
			}
			roots[uuidToString(r.ID)] = uuidToString(r.ChainRootSessionID)
		}
		for _, key := range decisionKeys {
			id, _, _ := strings.Cut(strings.TrimPrefix(key, "decision:"), "@")
			s.keys, s.roots = append(s.keys, key), append(s.roots, roots[id])
		}
	}
	if len(turnIDs) > 0 {
		rows, err := q.ListOwnedAttentionTurns(ctx, db.ListOwnedAttentionTurnsParams{Ids: turnIDs, UserID: userID, WorkspaceID: workspaceID})
		if err != nil {
			return nil, fail
		}
		if len(rows) != len(turnIDs) {
			return nil, forbidden
		}
		s.turns = turnIDs
		roots := map[string]string{}
		for _, r := range rows {
			if !vis.visible[uuidToString(r.ChainRootSessionID)] {
				return nil, forbidden
			}
			roots[uuidToString(r.ID)] = uuidToString(r.ChainRootSessionID)
		}
		for _, key := range turnKeys {
			s.keys, s.roots = append(s.keys, key), append(s.roots, roots[strings.TrimPrefix(key, "turn:")])
		}
	}
	return s, nil
}

// markArgs returns the keys and chain roots to mark: turns always, items only when asked.
func (s *attentionSubjects) markArgs(kinds ...string) (keys []string, roots []pgtype.UUID) {
	for i, key := range s.keys {
		for _, kind := range kinds {
			if strings.HasPrefix(key, kind+":") {
				keys, roots = append(keys, key), append(roots, parseUUID(s.roots[i]))
			}
		}
	}
	return keys, roots
}

func (s *attentionSubjects) openItems() []pgtype.UUID {
	var out []pgtype.UUID
	for _, id := range s.items {
		if s.itemOpen[uuidToString(id)] {
			out = append(out, id)
		}
	}
	return out
}

// xong resolves the open items and acknowledges the turns, in one transaction.
func (h *Handler) xong(ctx context.Context, userID pgtype.UUID, s *attentionSubjects, batch pgtype.UUID) *attentionFailure {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return &attentionFailure{http.StatusInternalServerError, "failed to start transaction"}
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	if open := s.openItems(); len(open) > 0 {
		if _, err := qtx.ResolveAttentionItemsXong(ctx, db.ResolveAttentionItemsXongParams{BatchID: batch, Ids: open, UserID: userID}); err != nil {
			return &attentionFailure{http.StatusInternalServerError, "failed to resolve items"}
		}
	}
	if keys, roots := s.markArgs("turn"); len(keys) > 0 {
		if err := qtx.AckAttentionTurns(ctx, db.AckAttentionTurnsParams{UserID: userID, BatchID: batch, SubjectKeys: keys, ChainRoots: roots}); err != nil {
			return &attentionFailure{http.StatusInternalServerError, "failed to acknowledge turns"}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return &attentionFailure{http.StatusInternalServerError, "failed to commit"}
	}
	return nil
}

type attentionKeysRequest struct {
	SubjectKeys []string `json:"subject_keys"`
}

// readChain returns the projection of one chain now; a chain with nothing to show reads as quiet.
func (h *Handler) readChain(ctx context.Context, workspaceID, userID, root pgtype.UUID) (chatAttentionResponse, error) {
	rows, err := service.LoadChatAttention(ctx, h.Queries, workspaceID, userID, time.Now())
	if err != nil {
		return chatAttentionResponse{}, err
	}
	rootID := uuidToString(root)
	for _, r := range rows {
		if r.RootID == rootID {
			return attentionResponse(r), nil
		}
	}
	return attentionResponse(service.ChatAttention{RootID: rootID, CanonicalID: rootID, StaleLevel: service.AttentionNone}), nil
}

// ChatAttentionXong is Xong on one DM: items resolve, turns acknowledge. A decision key is 422:
// a decision is answered, never dismissed.
func (h *Handler) ChatAttentionXong(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	var req attentionKeysRequest
	if !decodeAttentionBody(w, r, &req) {
		return
	}
	user, ws := parseUUID(userID), parseUUID(workspaceID)
	vis, fail := h.attentionVisibility(r, userID, workspaceID)
	if fail != nil {
		fail.write(w)
		return
	}
	subjects, fail := h.loadAttentionSubjects(r.Context(), h.Queries, ws, user, vis, req.SubjectKeys)
	if fail != nil {
		fail.write(w)
		return
	}
	if len(subjects.decisions) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "a decision cannot be dismissed with Xong; answer it")
		return
	}
	if fail := h.xong(r.Context(), user, subjects, parseUUID(uuid.NewString())); fail != nil {
		fail.write(w)
		return
	}
	view, err := h.readChain(r.Context(), ws, user, session.ChainRootSessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read attention")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attention": view})
}

// ChatAttentionXongAll is Xong on every displayed key across chains, under one batch id.
// Decision keys are skipped, not rejected: the list always carries them.
func (h *Handler) ChatAttentionXongAll(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	var req attentionKeysRequest
	if !decodeAttentionBody(w, r, &req) {
		return
	}
	user, ws := parseUUID(userID), parseUUID(workspaceID)
	vis, fail := h.attentionVisibility(r, userID, workspaceID)
	if fail != nil {
		fail.write(w)
		return
	}
	subjects, fail := h.loadAttentionSubjects(r.Context(), h.Queries, ws, user, vis, req.SubjectKeys)
	if fail != nil {
		fail.write(w)
		return
	}
	batch := uuid.NewString()
	if fail := h.xong(r.Context(), user, subjects, parseUUID(batch)); fail != nil {
		fail.write(w)
		return
	}
	h.writeAttentionList(r.Context(), w, ws, user, vis, batch)
}

// ChatAttentionUndoXongAll reopens what the batch closed. A row changed since (answered, sent to,
// replaced) stays as it is. Repeating the call changes nothing.
func (h *Handler) ChatAttentionUndoXongAll(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	batch, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "batchId"), "batch id")
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	user, ws := parseUUID(userID), parseUUID(workspaceID)
	vis, fail := h.attentionVisibility(r, userID, workspaceID)
	if fail != nil {
		fail.write(w)
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := qtx.ReopenXongBatchItems(r.Context(), db.ReopenXongBatchItemsParams{BatchID: batch, UserID: user, WorkspaceID: ws, HiddenRoots: vis.hidden}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reopen items")
		return
	}
	if _, err := qtx.ClearAttentionAckBatch(r.Context(), db.ClearAttentionAckBatchParams{BatchID: batch, UserID: user, WorkspaceID: ws, HiddenRoots: vis.hidden}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear acknowledgements")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit")
		return
	}
	h.writeAttentionList(r.Context(), w, ws, user, vis, uuidToString(batch))
}

func (h *Handler) writeAttentionList(ctx context.Context, w http.ResponseWriter, ws, user pgtype.UUID, vis *attentionVisibility, batch string) {
	rows, err := service.LoadChatAttention(ctx, h.Queries, ws, user, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read attention")
		return
	}
	out := make([]chatAttentionResponse, 0, len(rows))
	for _, r := range rows {
		if vis.visible[r.RootID] {
			out = append(out, attentionResponse(r))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": batch, "attention": out})
}

type chatAttentionLaterRequest struct {
	SubjectKeys     []string `json:"subject_keys"`
	DurationSeconds *int64   `json:"duration_seconds"`
	Until           *string  `json:"until"`
	RequestID       string   `json:"request_id"`
}

var errLaterRange = errors.New("later must be between 1s and 168h from now")

// laterDue computes the server-time due date. Out-of-range is an error, never a silent cap.
func laterDue(now time.Time, req chatAttentionLaterRequest) (time.Time, error) {
	d := attentionLaterTarget
	switch {
	case req.DurationSeconds != nil && req.Until != nil:
		return time.Time{}, errors.New("send duration_seconds or until, not both")
	case req.DurationSeconds != nil:
		if *req.DurationSeconds > int64(attentionLaterMax/time.Second) || *req.DurationSeconds < 1 {
			return time.Time{}, errLaterRange
		}
		d = time.Duration(*req.DurationSeconds) * time.Second
	case req.Until != nil:
		until, err := time.Parse(time.RFC3339, *req.Until)
		if err != nil {
			return time.Time{}, errors.New("until must be an RFC3339 time")
		}
		d = until.Sub(now)
	}
	if d < time.Second || d > attentionLaterMax {
		return time.Time{}, errLaterRange
	}
	return now.Add(d), nil
}

// ChatAttentionLater is Để sau: the sent keys go quiet until due and the chain gets one `later`
// item that returns it. The request id makes a retry change nothing; a new request replaces the
// chain's open later (and an open unseen).
func (h *Handler) ChatAttentionLater(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	var req chatAttentionLaterRequest
	if !decodeAttentionBody(w, r, &req) {
		return
	}
	requestID, err := util.ParseUUID(req.RequestID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "request_id must be a UUID")
		return
	}
	user, ws := parseUUID(userID), parseUUID(workspaceID)
	vis, fail := h.attentionVisibility(r, userID, workspaceID)
	if fail != nil {
		fail.write(w)
		return
	}
	subjects, fail := h.loadAttentionSubjects(r.Context(), h.Queries, ws, user, vis, req.SubjectKeys)
	if fail != nil {
		fail.write(w)
		return
	}
	due, err := laterDue(time.Now(), req)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	itemID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("chat-attention-later:"+userID+":"+uuidToString(requestID)))
	if fail := h.setLater(r.Context(), ws, user, session, parseUUID(itemID.String()), due, subjects); fail != nil {
		fail.write(w)
		return
	}
	view, err := h.readChain(r.Context(), ws, user, session.ChainRootSessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read attention")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attention": view})
}

func (h *Handler) setLater(ctx context.Context, ws, user pgtype.UUID, session db.ChatSession, itemID pgtype.UUID, due time.Time, s *attentionSubjects) *attentionFailure {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return &attentionFailure{http.StatusInternalServerError, "failed to start transaction"}
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	internal := &attentionFailure{http.StatusInternalServerError, "failed to set later"}
	if err := qtx.LockAttentionChain(ctx, uuidToString(session.ChainRootSessionID)); err != nil {
		return internal
	}
	seen, err := qtx.AttentionItemExists(ctx, itemID)
	if err != nil {
		return internal
	}
	if !seen {
		if err := qtx.ReplaceOpenLaterAndUnseen(ctx, db.ReplaceOpenLaterAndUnseenParams{ChainRoot: session.ChainRootSessionID, UserID: user, SentItemIds: s.items}); err != nil {
			return internal
		}
		if err := qtx.InsertLaterAttentionItem(ctx, db.InsertLaterAttentionItemParams{
			ID: itemID, WorkspaceID: ws, ChainRoot: session.ChainRootSessionID, ChatSessionID: session.ID,
			UserID: user, DueAt: pgtype.Timestamptz{Time: due, Valid: true},
		}); err != nil {
			return internal
		}
		keys, roots := s.markArgs("decision", "turn")
		for id, open := range s.itemOpen {
			if open {
				keys, roots = append(keys, service.ItemKey(id)), append(roots, s.itemRoots[id])
			}
		}
		if len(keys) > 0 {
			if err := qtx.SnoozeAttentionSubjects(ctx, db.SnoozeAttentionSubjectsParams{
				UserID: user, SnoozedUntil: pgtype.Timestamptz{Time: due, Valid: true}, SubjectKeys: keys, ChainRoots: roots,
			}); err != nil {
				return internal
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return internal
	}
	return nil
}

// openUnseenAttention records an explicit mark-unseen as an obligation of the chain (plan r4 T6).
// Same no-op as the read cursor: nothing happens without a counted assistant message, and a
// second mark-unseen on the chain adds nothing.
func (h *Handler) openUnseenAttention(ctx context.Context, session db.ChatSession, userID string) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	if err := qtx.LockAttentionChain(ctx, uuidToString(session.ChainRootSessionID)); err != nil {
		return err
	}
	if err := qtx.OpenUnseenAttentionItem(ctx, db.OpenUnseenAttentionItemParams{
		WorkspaceID: session.WorkspaceID, ChainRoot: session.ChainRootSessionID, ChatSessionID: session.ID, UserID: parseUUID(userID),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetChatSessionAttention returns the chain's current projection, including the exact keys the
// client shows, so `multica chat later` can snooze what the user sees (CHE-4151).
func (h *Handler) GetChatSessionAttention(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	session, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	view, err := h.readChain(r.Context(), parseUUID(workspaceID), parseUUID(userID), session.ChainRootSessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read attention")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attention": view})
}

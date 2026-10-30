package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DecisionFeedScope is the PAT scope that opens the answer change feed. The feed
// carries verbatim human quotes for the Hindsight sweep, so no other credential
// reads it: not a task token, a chat task, a cloud PAT, a JWT or an unscoped PAT.
const DecisionFeedScope = "decisions:feed"

const decisionFeedPageSize = 200

type DecisionFeedItem struct {
	AnswerID       string  `json:"answer_id"`
	StateVersion   int64   `json:"state_version"`
	Issue          string  `json:"issue"`
	Ask            string  `json:"ask"`
	Class          string  `json:"class"`
	Quote          string  `json:"quote"`
	AnsweredAt     string  `json:"answered_at"`
	State          string  `json:"state"`
	DisclaimReason *string `json:"disclaim_reason"`
	ProjectTitle   *string `json:"project_title"`
}

// decisionFeedState names what the sweep does with an answer: only "effective"
// and "withdrawn" become Hindsight documents. "withdrawn" means a delivered
// answer was disclaimed; one withdrawn before delivery was never effective.
func decisionFeedState(row db.ListDecisionAnswerFeedRow) string {
	delivered := row.WokenAt.Valid && row.WakeOutcome.String != "skipped_ineligible"
	switch {
	case row.DisclaimedAt.Valid && delivered:
		return "withdrawn"
	case row.DisclaimedAt.Valid, row.WakeOutcome.String == "skipped_ineligible":
		return "not_delivered"
	case delivered:
		return "effective"
	default:
		return "pending"
	}
}

// requireDecisionFeedCaller admits one credential: a PAT carrying DecisionFeedScope
// whose user owns the workspace. The scope is read from the token row on every call,
// not from the auth cache, so a revoked or re-scoped token stops at once.
func (h *Handler) requireDecisionFeedCaller(w http.ResponseWriter, r *http.Request) (string, bool) {
	deny := func() (string, bool) {
		writeError(w, http.StatusForbidden, "the decision feed needs a decisions:feed PAT")
		return "", false
	}
	if r.Header.Get("X-Actor-Source") != "pat" {
		return deny()
	}
	bearer, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		return deny()
	}
	pat, err := h.Queries.GetPersonalAccessTokenByHash(r.Context(), auth.HashToken(bearer))
	if err != nil || !slices.Contains(pat.Scopes, DecisionFeedScope) || uuidToString(pat.UserID) != r.Header.Get("X-User-ID") {
		return deny()
	}
	workspaceID := ctxWorkspaceID(r.Context())
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner"); !ok {
		return "", false
	}
	return workspaceID, true
}

func (h *Handler) ListDecisionFeed(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.requireDecisionFeedCaller(w, r)
	if !ok {
		return
	}
	rows, err := h.Queries.ListDecisionAnswerFeed(r.Context(), db.ListDecisionAnswerFeedParams{WorkspaceID: parseUUID(workspaceID), RowLimit: decisionFeedPageSize})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list decision feed")
		return
	}
	items := make([]DecisionFeedItem, 0, len(rows))
	for _, row := range rows {
		ask, _, _ := strings.Cut(strings.TrimSpace(row.Ask), "\n")
		items = append(items, DecisionFeedItem{
			AnswerID: uuidToString(row.ID), StateVersion: row.StateVersion, Issue: row.IssuePrefix + "-" + strconv.Itoa(int(row.IssueNumber)),
			Ask: ask, Class: row.EscalationClass, Quote: row.Quote, AnsweredAt: timestampToString(row.EffectiveAt),
			State: decisionFeedState(row), DisclaimReason: textToPtr(row.DisclaimReason), ProjectTitle: textToPtr(row.ProjectTitle),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"answers": items})
}

type decisionFeedAckRequest struct {
	Version int64 `json:"version"`
}

// AckDecisionFeed moves the stored acknowledged version up, never down. A repeated
// or stale ack answers applied=false with 200: the sweep's retry must stay safe.
func (h *Handler) AckDecisionFeed(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.requireDecisionFeedCaller(w, r)
	if !ok {
		return
	}
	answerID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "answer id")
	if !ok {
		return
	}
	var body decisionFeedAckRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Version < 1 {
		writeError(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	acked, err := h.Queries.AckDecisionAnswerFeed(r.Context(), db.AckDecisionAnswerFeedParams{WorkspaceID: parseUUID(workspaceID), ID: answerID, Version: body.Version})
	if errors.Is(err, pgx.ErrNoRows) {
		if _, lookupErr := h.Queries.GetDecisionAnswer(r.Context(), db.GetDecisionAnswerParams{WorkspaceID: parseUUID(workspaceID), ID: answerID}); lookupErr != nil {
			writeError(w, http.StatusNotFound, "decision answer not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"applied": false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to ack decision feed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": true, "acked_version": acked.HindsightAckedVersion})
}

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DecisionCIRequestScope is the PAT scope that lets a CI job open one deploy
// decision card. It opens POST /api/decisions/ci and no other decision route.
const DecisionCIRequestScope = "decisions:ci_request"

var ciCandidateSHA = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

const ciFingerprintPrefix = serverDecisionFingerprint + "ci:"

// ciCardCandidate reads the candidate SHA and commit time a CI card carries in
// its fingerprint, "server:ci:<sha>:<RFC 3339 time>". A card from before the
// candidate was named does not parse and takes no part in ordering.
func ciCardCandidate(card db.DecisionRequest) (sha string, at time.Time, ok bool) {
	rest, found := strings.CutPrefix(card.RequestFingerprint, ciFingerprintPrefix)
	if !found {
		return "", time.Time{}, false
	}
	sha, stamp, found := strings.Cut(rest, ":")
	at, err := time.Parse(time.RFC3339, stamp)
	return sha, at, found && err == nil
}

// CreateCIDecision opens a server-origin card of class deploy for a CI job
// (CHE-5019). The job has no agent task, so the card is asked the way a budget
// card is (openRunBudgetDecision): zero task id, `server:` fingerprint. The body
// names the issue, the owner and the ask; the class is fixed, and the body
// carries no action and no covered cards. The candidate SHA and its commit time
// order the cards on one issue: one open ask per SHA, the same SHA again returns
// that card (200), a newer SHA withdraws the older open ask, and an older SHA
// after a newer one is refused (409). The check runs under the workspace lock,
// in the write's transaction.
func (h *Handler) CreateCIDecision(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireScopedPATOwner(w, r, DecisionCIRequestScope); !ok {
		return
	}
	var input struct {
		IssueID string          `json:"issue_id"`
		For     string          `json:"for"`
		Ask     json.RawMessage `json:"ask"`

		CandidateSHA string `json:"candidate_sha"`
		CandidateAt  string `json:"candidate_at"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*maxCommentContentBytes)).Decode(&input); err != nil || len(input.Ask) == 0 || string(input.Ask) == "null" {
		writeError(w, http.StatusBadRequest, "issue_id, for and a structured ask are required")
		return
	}
	candidateSHA := strings.ToLower(input.CandidateSHA)
	candidateAt, err := time.Parse(time.RFC3339, input.CandidateAt)
	if !ciCandidateSHA.MatchString(candidateSHA) || err != nil {
		writeError(w, http.StatusBadRequest, "candidate_sha (7 to 64 hex digits) and candidate_at (RFC 3339 commit time) are required")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, input.IssueID)
	if !ok {
		return
	}
	human, ok := parseUUIDOrBadRequest(w, input.For, "for")
	if !ok {
		return
	}
	parsed, err := decisiontx.DecodeAskKind(input.Ask, decisiontx.KindDecision)
	if err == nil {
		parsed, err = parsed.RequireRecommendedFirst()
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if parsed.SourceIssueID != uuidToString(issue.ID) {
		writeError(w, http.StatusBadRequest, "ask.source_issue_id must match issue_id")
		return
	}
	sourceCommentID, ok := parseUUIDOrBadRequest(w, parsed.SourceCommentID, "ask.source_comment_id")
	if !ok {
		return
	}
	workspace, err := h.Queries.GetWorkspace(r.Context(), issue.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load workspace")
		return
	}
	ask := parsed.Content(workspace.Slug)
	if len(decisionAskContent(decisionRequestInput{Ask: ask})) > maxCommentContentBytes {
		writeError(w, http.StatusBadRequest, "ask is too long")
		return
	}
	if err := h.verifyAskArtifacts(r.Context(), issue, parsed); err != nil {
		writeDecisionError(w, err, "ci decision request")
		return
	}
	askJSON, err := json.Marshal(parsed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode ask")
		return
	}
	card := decisionRequestInput{
		HumanUserID: human, Ask: ask, Options: parsed.Options, AskJSON: askJSON, EscalationClass: "deploy",
		SourceCommentID: sourceCommentID, Parsed: &parsed, Slug: workspace.Slug, Kind: decisiontx.KindDecision,
		Fingerprint: ciFingerprintPrefix + candidateSHA + ":" + candidateAt.UTC().Format(time.RFC3339),
	}
	actor := parseUUID(r.Header.Get("X-User-ID"))
	var result decisionRequestResult
	var withdrawn []decisionRequestResult
	replayed := false
	err = h.withPgxTx(r.Context(), func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		plan, err := h.planRunBudgetDecisions(r.Context(), q, issue, []pgtype.UUID{human})
		if err != nil {
			return err
		}
		if len(plan.lanes) == 0 {
			return decisionIntentRefusal{http.StatusConflict, "for is not a member of this workspace"}
		}
		cards, err := q.ListCIDecisionRequests(r.Context(), db.ListCIDecisionRequestsParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
		if err != nil {
			return err
		}
		var newest *db.DecisionRequest
		var newestAt time.Time
		seen := false
		for i, old := range cards {
			sha, at, ok := ciCardCandidate(old)
			if !ok {
				continue
			}
			seen = seen || sha == candidateSHA
			if newest == nil || !at.Before(newestAt) {
				newest, newestAt = &cards[i], at
			}
		}
		if newest != nil {
			if sha, _, _ := ciCardCandidate(*newest); sha == candidateSHA {
				result, replayed = decisionRequestResult{Request: *newest}, true
				return nil
			}
			if seen || !candidateAt.After(newestAt) {
				return decisionIntentRefusal{http.StatusConflict, "a newer candidate already has a decision card on this issue"}
			}
		}
		for _, old := range cards {
			if _, _, ok := ciCardCandidate(old); !ok || old.Status != "open" {
				continue
			}
			done, err := withdrawSupersededCICard(r.Context(), q, old, candidateSHA, actor)
			if err != nil {
				return err
			}
			withdrawn = append(withdrawn, done)
		}
		result, err = h.openRunBudgetDecision(r.Context(), q, issue, plan, plan.lanes[0], card)
		return err
	})
	if err != nil {
		writeDecisionError(w, err, "ci decision request")
		return
	}
	if replayed {
		writeJSON(w, http.StatusOK, decisionRequestResponse{DecisionRequest: result.Request})
		return
	}
	for _, done := range withdrawn {
		h.publishDecisionWithdrawal(uuidToString(issue.WorkspaceID), done)
	}
	h.publishRunBudgetDecisions(issue, []decisionRequestResult{result})
	writeJSON(w, http.StatusCreated, decisionRequestResponse{DecisionRequest: result.Request})
}

// withdrawSupersededCICard cancels an older candidate's open ask as the PAT's
// owner, with the locks the member withdrawal takes.
func withdrawSupersededCICard(ctx context.Context, q *db.Queries, card db.DecisionRequest, candidateSHA string, actor pgtype.UUID) (decisionRequestResult, error) {
	empty := decisionRequestResult{}
	if _, err := q.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: card.WorkspaceID, ID: card.IssueID}); err != nil {
		return empty, err
	}
	if _, err := q.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: card.WorkspaceID, ID: card.ChatSessionID}); err != nil {
		return empty, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, q, card.WorkspaceID, card.IssueCommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	current, err := q.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: card.WorkspaceID, ID: card.ID})
	if err != nil {
		return empty, err
	}
	reason := "Superseded by candidate " + candidateSHA[:min(12, len(candidateSHA))] + "."
	return withdrawOpenDecision(ctx, q, current, root, reason, "member", actor, pgtype.UUID{}, actor)
}

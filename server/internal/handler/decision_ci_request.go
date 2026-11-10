package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
)

// DecisionCIRequestScope is the PAT scope that lets a CI job open one deploy
// decision card. It opens POST /api/decisions/ci and no other decision route.
const DecisionCIRequestScope = "decisions:ci_request"

// CreateCIDecision opens a server-origin card of class deploy for a CI job
// (CHE-5019). The job has no agent task, so the card is asked the way a budget
// card is (openRunBudgetDecision): zero task id, `server:` fingerprint. The body
// names the issue, the owner and the ask; the class is fixed, and the body
// carries no action and no covered cards.
func (h *Handler) CreateCIDecision(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireScopedPATOwner(w, r, DecisionCIRequestScope); !ok {
		return
	}
	var input struct {
		IssueID string          `json:"issue_id"`
		For     string          `json:"for"`
		Ask     json.RawMessage `json:"ask"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*maxCommentContentBytes)).Decode(&input); err != nil || len(input.Ask) == 0 || string(input.Ask) == "null" {
		writeError(w, http.StatusBadRequest, "issue_id, for and a structured ask are required")
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
	sum := sha256.Sum256(append([]byte(uuidToString(human)+"\n"), askJSON...))
	card := decisionRequestInput{
		HumanUserID: human, Ask: ask, Options: parsed.Options, AskJSON: askJSON, EscalationClass: "deploy",
		SourceCommentID: sourceCommentID, Parsed: &parsed, Slug: workspace.Slug, Kind: decisiontx.KindDecision,
		Fingerprint: serverDecisionFingerprint + "ci:" + hex.EncodeToString(sum[:]),
	}
	var result decisionRequestResult
	err = h.withPgxTx(r.Context(), func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		plan, err := h.planRunBudgetDecisions(r.Context(), q, issue, []pgtype.UUID{human})
		if err != nil {
			return err
		}
		if len(plan.lanes) == 0 {
			return decisionIntentRefusal{http.StatusConflict, "for is not a member of this workspace"}
		}
		result, err = h.openRunBudgetDecision(r.Context(), q, issue, plan, plan.lanes[0], card)
		return err
	})
	if err != nil {
		writeDecisionError(w, err, "ci decision request")
		return
	}
	h.publishRunBudgetDecisions(issue, []decisionRequestResult{result})
	writeJSON(w, http.StatusCreated, decisionRequestResponse{DecisionRequest: result.Request})
}

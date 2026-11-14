package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/cibroker"
	"github.com/multica-ai/multica/server/internal/service"
)

// PutMergeReceipt records the receipt facts a merge_eligibility rule reads
// (CHE-5594). Routing limits it to a workspace owner or admin acting as a
// person; a run's task token and a cloud-node credential are refused, so an
// agent cannot assert its own PASS. The server stamps the observation time and
// stores the facts only for the PR's current head and base.
func (h *Handler) PutMergeReceipt(w http.ResponseWriter, r *http.Request) {
	h.recordMergeReceipt(w, r, parseUUID(h.resolveWorkspaceID(r)), "", "")
}

// recordMergeReceipt decodes and stores one receipt. A non-empty pinOwner and
// pinRepo fix the repository: a body naming another one is refused.
func (h *Handler) recordMergeReceipt(w http.ResponseWriter, r *http.Request, workspaceID pgtype.UUID, pinOwner, pinRepo string) {
	var req struct {
		Owner             string     `json:"repo_owner"`
		Repo              string     `json:"repo_name"`
		Number            int32      `json:"pr_number"`
		HeadSHA           string     `json:"head_sha"`
		BaseRef           string     `json:"base_ref"`
		Tier              *int       `json:"tier"`
		ClassifierTrusted *bool      `json:"classifier_trusted"`
		CoverageComplete  *bool      `json:"coverage_complete"`
		PRBound           *bool      `json:"pr_bound"`
		SecurityGate      string     `json:"security_gate"`
		CrossLabPass      *bool      `json:"cross_lab_pass"`
		ScopeClassified   *bool      `json:"scope_classified"`
		GateVetoed        *bool      `json:"gate_vetoed"`
		Paths             []string   `json:"paths"`
		Labels            []string   `json:"labels"`
		LastPassAt        *time.Time `json:"last_pass_at"`
		Evidence          string     `json:"evidence"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if pinOwner != "" {
		if (req.Owner != "" && !strings.EqualFold(req.Owner, pinOwner)) || (req.Repo != "" && !strings.EqualFold(req.Repo, pinRepo)) {
			writeError(w, http.StatusForbidden, "this identity writes receipts for "+pinOwner+"/"+pinRepo+" only")
			return
		}
		req.Owner, req.Repo = pinOwner, pinRepo
	}
	err := service.RecordMergeReceipt(r.Context(), h.TxStarter, workspaceID, service.MergeReceiptInput{
		Owner: req.Owner, Repo: req.Repo, Number: req.Number, HeadSHA: req.HeadSHA, BaseRef: req.BaseRef,
		Tier: req.Tier, ClassifierTrusted: req.ClassifierTrusted, CoverageComplete: req.CoverageComplete, PRBound: req.PRBound,
		SecurityGate: req.SecurityGate, CrossLabPass: req.CrossLabPass, ScopeClassified: req.ScopeClassified, GateVetoed: req.GateVetoed,
		Paths: req.Paths, Labels: req.Labels, LastPassAt: req.LastPassAt, Evidence: req.Evidence,
	}, time.Now())
	switch {
	case service.IsMergeReceiptInvalid(err):
		writeError(w, http.StatusBadRequest, err.Error())
	case service.IsMergeReceiptConflict(err):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to record the receipt")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

type mergeVetoRequest struct {
	Owner  string `json:"repo_owner"`
	Repo   string `json:"repo_name"`
	Number int32  `json:"pr_number"`
	Reason string `json:"reason"`
}

func decodeMergeVeto(w http.ResponseWriter, r *http.Request) (mergeVetoRequest, bool) {
	var req mergeVetoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Owner == "" || req.Repo == "" || req.Number <= 0 {
		writeError(w, http.StatusBadRequest, "repo_owner, repo_name and pr_number are required")
		return req, false
	}
	return req, true
}

// PutMergeVeto records a veto made in Multica only (CHE-5594). It holds the PR
// and drops its receipt; nothing a producer records under it survives. Routing
// limits it to a workspace owner or admin acting as a person.
func (h *Handler) PutMergeVeto(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeMergeVeto(w, r)
	if !ok {
		return
	}
	if err := service.SetMergeVeto(r.Context(), h.TxStarter, parseUUID(h.resolveWorkspaceID(r)), req.Owner, req.Repo, req.Number, parseUUID(requestUserID(r)), req.Reason, time.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record the veto")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteMergeVeto lifts a veto. The PR stays held until a producer records a
// new receipt.
func (h *Handler) DeleteMergeVeto(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeMergeVeto(w, r)
	if !ok {
		return
	}
	if err := service.ClearMergeVeto(r.Context(), h.TxStarter, parseUUID(h.resolveWorkspaceID(r)), req.Owner, req.Repo, req.Number); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lift the veto")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// MergeReceiptAudience is the OIDC audience a workflow asks for when it writes
// a receipt. It differs from the CI broker audience, so a token minted for one
// surface is refused by the other.
const MergeReceiptAudience = "multica-merge-receipt"

// PutMergeReceiptCI records a receipt for a workflow identity (CHE-5594). The
// bearer is a GitHub Actions OIDC token. The policy entry that admits it fixes
// the repository, so the body cannot name another one, and the route writes to
// the one workspace the server is configured with. Replayed, wrong-audience,
// wrong-workflow and pull_request tokens are refused by the broker.
func (h *Handler) PutMergeReceiptCI(w http.ResponseWriter, r *http.Request) {
	if h.MergeReceiptBroker == nil || h.MergeReceiptWorkspaceID == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	entry, _, err := h.MergeReceiptBroker.Authorize(r.Context(), token)
	if err != nil {
		var refusal *cibroker.Refusal
		if errors.As(err, &refusal) {
			writeError(w, refusal.Status, refusal.Reason)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify the token")
		return
	}
	owner, repo, _ := strings.Cut(entry.Repository, "/")
	h.recordMergeReceipt(w, r, parseUUID(h.MergeReceiptWorkspaceID), owner, repo)
}

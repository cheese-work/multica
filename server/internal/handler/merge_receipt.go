package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

// PutMergeReceipt records the receipt facts a merge_eligibility rule reads
// (CHE-5594). Routing limits it to a workspace owner or admin acting as a
// person; a run's task token and a cloud-node credential are refused, so an
// agent cannot assert its own PASS. The server stamps the observation time and
// stores the facts only for the PR's current head and base.
func (h *Handler) PutMergeReceipt(w http.ResponseWriter, r *http.Request) {
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
	err := service.RecordMergeReceipt(r.Context(), h.TxStarter, parseUUID(h.resolveWorkspaceID(r)), service.MergeReceiptInput{
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

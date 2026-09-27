package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const governanceProposalResultMaxBytes = 16 * 1024

type governanceProposalToken struct {
	workspaceID   pgtype.UUID
	userID        pgtype.UUID
	agentID       pgtype.UUID
	taskID        pgtype.UUID
	caseID        pgtype.UUID
	attemptID     pgtype.UUID
	attemptFence  pgtype.UUID
	evidenceEpoch int32
}

type governanceProposalEvidenceResponse struct {
	CaseID         string          `json:"case_id"`
	AttemptID      string          `json:"attempt_id"`
	InputDigest    string          `json:"input_digest"`
	EvidenceEpoch  int32           `json:"evidence_epoch"`
	SnapshotDigest string          `json:"snapshot_digest"`
	Snapshot       json.RawMessage `json:"snapshot"`
	CandidateMap   json.RawMessage `json:"candidate_map"`
	CitationMap    json.RawMessage `json:"citation_map"`
}

func (h *Handler) governanceProposalToken(w http.ResponseWriter, r *http.Request) (governanceProposalToken, bool) {
	var token governanceProposalToken
	if r.Header.Get("X-Actor-Source") != "task_token" || r.Header.Get("X-Task-Token-Purpose") != middleware.GovernanceProposalTaskPurpose {
		writeGovernanceProblem(w, r, http.StatusForbidden, "proposal_token_scope_denied")
		return token, false
	}
	caseID, caseErr := util.ParseUUID(chi.URLParam(r, "caseId"))
	attemptID, attemptErr := util.ParseUUID(chi.URLParam(r, "attemptId"))
	workspaceID, workspaceErr := util.ParseUUID(r.Header.Get("X-Workspace-ID"))
	userID, userErr := util.ParseUUID(r.Header.Get("X-User-ID"))
	agentID, agentErr := util.ParseUUID(r.Header.Get("X-Agent-ID"))
	taskID, taskErr := util.ParseUUID(r.Header.Get("X-Task-ID"))
	attemptFence, fenceErr := util.ParseUUID(r.Header.Get("X-Governance-Attempt-Fence"))
	evidenceEpoch, epochErr := strconv.ParseInt(r.Header.Get("X-Governance-Evidence-Epoch"), 10, 32)
	if caseErr != nil || attemptErr != nil || workspaceErr != nil || userErr != nil || agentErr != nil ||
		taskErr != nil || fenceErr != nil || epochErr != nil || evidenceEpoch < 0 ||
		r.Header.Get("X-Governance-Case-ID") != util.UUIDToString(caseID) ||
		r.Header.Get("X-Governance-Attempt-ID") != util.UUIDToString(attemptID) {
		writeGovernanceProblem(w, r, http.StatusForbidden, "proposal_token_scope_denied")
		return token, false
	}
	token = governanceProposalToken{
		workspaceID: workspaceID, userID: userID, agentID: agentID, taskID: taskID,
		caseID: caseID, attemptID: attemptID, attemptFence: attemptFence,
		evidenceEpoch: int32(evidenceEpoch),
	}
	if _, err := h.getWorkspaceMember(r.Context(), util.UUIDToString(token.userID), util.UUIDToString(token.workspaceID)); err != nil {
		writeGovernanceProblem(w, r, http.StatusForbidden, "proposal_access_denied")
		return token, false
	}
	agent, err := h.Queries.GetAgent(r.Context(), token.agentID)
	if err != nil || !h.canInvokeAgent(
		r.Context(), agent, "member", util.UUIDToString(token.userID),
		util.UUIDToString(token.userID), util.UUIDToString(token.workspaceID),
	) {
		writeGovernanceProblem(w, r, http.StatusForbidden, "proposal_access_denied")
		return token, false
	}
	return token, true
}

func (h *Handler) governanceProposalEvidence(w http.ResponseWriter, r *http.Request, token governanceProposalToken) (db.GetGovernanceProposalEvidenceRow, bool) {
	evidence, err := h.Queries.GetGovernanceProposalEvidence(r.Context(), db.GetGovernanceProposalEvidenceParams{
		WorkspaceID: token.workspaceID, CaseID: token.caseID, AttemptID: token.attemptID,
		EvidenceEpoch: token.evidenceEpoch, TaskID: token.taskID, AgentID: token.agentID,
		AttemptFence: token.attemptFence,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
			return evidence, false
		}
		writeGovernanceProblem(w, r, http.StatusConflict, "proposal_unavailable")
		return evidence, false
	}
	return evidence, true
}

func (h *Handler) GetGovernanceProposalEvidence(w http.ResponseWriter, r *http.Request) {
	token, ok := h.governanceProposalToken(w, r)
	if !ok {
		return
	}
	evidence, ok := h.governanceProposalEvidence(w, r, token)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, governanceProposalEvidenceResponse{
		CaseID: util.UUIDToString(evidence.CaseID), AttemptID: util.UUIDToString(evidence.AttemptID),
		InputDigest: evidence.InputDigest, EvidenceEpoch: evidence.EvidenceEpoch,
		SnapshotDigest: evidence.SnapshotDigest, Snapshot: json.RawMessage(evidence.Snapshot),
		CandidateMap: json.RawMessage(evidence.CandidateMap), CitationMap: json.RawMessage(evidence.CitationMap),
	})
}

func (h *Handler) HeartbeatGovernanceProposalAttempt(w http.ResponseWriter, r *http.Request) {
	token, ok := h.governanceProposalToken(w, r)
	if !ok {
		return
	}
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) != 0 {
			writeGovernanceProblem(w, r, http.StatusBadRequest, "proposal_invalid_request")
			return
		}
	}
	rows, err := h.Queries.HeartbeatGovernanceProposalAttempt(r.Context(), db.HeartbeatGovernanceProposalAttemptParams{
		WorkspaceID: token.workspaceID, CaseID: token.caseID, AttemptID: token.attemptID,
		TaskID: token.taskID, AgentID: token.agentID, AttemptFence: token.attemptFence,
		EvidenceEpoch: token.evidenceEpoch,
	})
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	if rows != 1 {
		writeGovernanceProblem(w, r, http.StatusConflict, "proposal_unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) SubmitGovernanceProposalResult(w http.ResponseWriter, r *http.Request) {
	token, ok := h.governanceProposalToken(w, r)
	if !ok {
		return
	}
	evidence, ok := h.governanceProposalEvidence(w, r, token)
	if !ok {
		return
	}
	result, err := decodeGovernanceProposalResult(
		http.MaxBytesReader(w, r.Body, governanceProposalResultMaxBytes),
		evidence.CandidateMap, evidence.CitationMap,
	)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusBadRequest, "proposal_invalid_request")
		return
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusInternalServerError, "configuration_unavailable")
		return
	}
	confidenceJSON, err := json.Marshal(map[string]float64{"proposal_confidence": result.ProposalConfidence})
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusInternalServerError, "configuration_unavailable")
		return
	}
	rows, err := h.Queries.SubmitGovernanceProposalResult(r.Context(), db.SubmitGovernanceProposalResultParams{
		Result: resultJSON, Confidence: confidenceJSON,
		WorkspaceID: token.workspaceID, CaseID: token.caseID, AttemptID: token.attemptID,
		TaskID: token.taskID, AgentID: token.agentID, AttemptFence: token.attemptFence,
		EvidenceEpoch: token.evidenceEpoch, CandidateMap: evidence.CandidateMap,
		CitationMap: evidence.CitationMap,
	})
	if err != nil {
		writeGovernanceProblem(w, r, http.StatusServiceUnavailable, "configuration_unavailable")
		return
	}
	if rows != 1 {
		writeGovernanceProblem(w, r, http.StatusConflict, "proposal_unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

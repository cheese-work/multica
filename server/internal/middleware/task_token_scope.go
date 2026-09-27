package middleware

import (
	"encoding/json"
	"net/http"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/governance"
)

const GovernanceProposalTaskPurpose = "governance_proposal"

func governanceProposalCapability(method, path, rawPath, rawQuery, caseID, attemptID string) string {
	if rawQuery != "" || (rawPath != "" && rawPath != path) || caseID == "" || attemptID == "" {
		return ""
	}
	pathParts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(pathParts) != 7 || pathParts[0] != "api" || pathParts[1] != "governance" ||
		pathParts[2] != "proposals" || pathParts[4] != "attempts" ||
		pathParts[3] != caseID || pathParts[5] != attemptID ||
		canonicalUUID(pathParts[3]) != caseID || canonicalUUID(pathParts[5]) != attemptID {
		return ""
	}
	switch {
	case method == http.MethodGet && pathParts[6] == "evidence":
		return "evidence"
	case method == http.MethodPost && pathParts[6] == "heartbeat":
		return "heartbeat"
	case method == http.MethodPost && pathParts[6] == "result":
		return "result"
	default:
		return ""
	}
}

func canonicalUUID(value string) string {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return ""
	}
	return parsed.String()
}

func writeGovernanceProposalProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	problem, ok := governance.SafeProblemFor(code)
	if !ok {
		problem, _ = governance.SafeProblemFor("configuration_unavailable")
	}
	correlationID := strings.TrimSpace(chimw.GetReqID(r.Context()))
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	problem.CorrelationID = correlationID
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set(chimw.RequestIDHeader, correlationID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

func writeInvalidTaskToken(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/governance/proposals/") {
		writeGovernanceProposalProblem(w, r, http.StatusUnauthorized, "proposal_unavailable")
		return
	}
	http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
}

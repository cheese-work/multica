package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// run_capability_grant (CHE-2216 plan 3.1): a person's time-boxed widening of
// one issue's runs. This slice only records the grant; the profile resolver
// that reads it lands separately. Scopes are an enum, never free text.
const (
	maxGrantTTL       = 24 * time.Hour
	maxGrantScopes    = 16
	maxGrantReasonLen = 1000
)

var (
	grantNamedScope = regexp.MustCompile(`^(protected_paths|device):[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	grantRepoScope  = regexp.MustCompile(`^repo_write:[1-9][0-9]{0,17}$`)
	grantSHA        = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func validGrantScope(s string) bool {
	return s == "multica_cross_issue" || s == "docker" || grantNamedScope.MatchString(s) || grantRepoScope.MatchString(s)
}

type createRunCapabilityGrantRequest struct {
	Scopes        []string `json:"scopes"`
	ExpiresAt     *string  `json:"expires_at"`
	Reason        string   `json:"reason"`
	PinnedBaseSHA *string  `json:"pinned_base_sha"`
	PinnedHeadSHA *string  `json:"pinned_head_sha"`
}

type runCapabilityGrantResponse struct {
	ID                string   `json:"id"`
	WorkspaceID       string   `json:"workspace_id"`
	IssueID           string   `json:"issue_id"`
	Scopes            []string `json:"scopes"`
	GrantedByMemberID string   `json:"granted_by_member_id"`
	CreatedAt         string   `json:"created_at"`
	ExpiresAt         string   `json:"expires_at"`
	Reason            string   `json:"reason"`
	PinnedBaseSHA     *string  `json:"pinned_base_sha"`
	PinnedHeadSHA     *string  `json:"pinned_head_sha"`
}

func runCapabilityGrantToResponse(g db.RunCapabilityGrant) runCapabilityGrantResponse {
	return runCapabilityGrantResponse{
		ID: uuidToString(g.ID), WorkspaceID: uuidToString(g.WorkspaceID), IssueID: uuidToString(g.IssueID),
		Scopes: g.Scopes, GrantedByMemberID: uuidToString(g.GrantedByMemberID),
		CreatedAt: g.CreatedAt.Time.UTC().Format(time.RFC3339), ExpiresAt: g.ExpiresAt.Time.UTC().Format(time.RFC3339),
		Reason: g.Reason, PinnedBaseSHA: textPtr(g.PinnedBaseSha), PinnedHeadSHA: textPtr(g.PinnedHeadSha),
	}
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// pinnedSHA returns the stored form of an optional pinned commit, or false when
// it is not a lowercase 40-hex SHA.
func pinnedSHA(p *string) (pgtype.Text, bool) {
	if p == nil {
		return pgtype.Text{}, true
	}
	return pgtype.Text{String: *p, Valid: true}, grantSHA.MatchString(*p)
}

// CreateRunCapabilityGrant records a grant for one issue. Human owner/admin
// only: any agent credential, including the task token of an agent a member
// owns, gets 403 before the issue is read.
func (h *Handler) CreateRunCapabilityGrant(w http.ResponseWriter, r *http.Request) {
	actorType, _ := h.resolveActor(r, requestUserID(r), h.resolveWorkspaceID(r))
	if isGovernedFieldMachineActor(r, actorType) {
		writeError(w, http.StatusForbidden, "only a person can grant run capabilities")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	member, ok := h.requireWorkspaceRole(w, r, uuidToString(issue.WorkspaceID), "workspace not found", "owner", "admin")
	if !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req createRunCapabilityGrantRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	scopes := make([]string, 0, len(req.Scopes))
	for _, s := range req.Scopes {
		if !validGrantScope(s) {
			writeError(w, http.StatusBadRequest, "unknown scope: "+s)
			return
		}
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	if len(scopes) == 0 || len(scopes) > maxGrantScopes {
		writeError(w, http.StatusBadRequest, "scopes must hold 1 to 16 entries")
		return
	}
	if req.ExpiresAt == nil {
		writeError(w, http.StatusBadRequest, "expires_at is required")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, *req.ExpiresAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "expires_at must be an RFC 3339 time")
		return
	}
	if now := time.Now(); !expiresAt.After(now) || expiresAt.After(now.Add(maxGrantTTL)) {
		writeError(w, http.StatusBadRequest, "expires_at must be in the future and within 24 hours")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > maxGrantReasonLen {
		writeError(w, http.StatusBadRequest, "reason is required and at most 1000 characters")
		return
	}
	baseSHA, baseOK := pinnedSHA(req.PinnedBaseSHA)
	headSHA, headOK := pinnedSHA(req.PinnedHeadSHA)
	if !baseOK || !headOK {
		writeError(w, http.StatusBadRequest, "pinned SHAs must be 40 lowercase hex characters")
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create the grant")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	grant, err := qtx.CreateRunCapabilityGrant(r.Context(), db.CreateRunCapabilityGrantParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, Scopes: scopes, GrantedByMemberID: member.ID,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true}, Reason: reason,
		PinnedBaseSha: baseSHA, PinnedHeadSha: headSHA,
	})
	if err != nil {
		slog.Warn("create run capability grant failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to create the grant")
		return
	}
	details, _ := json.Marshal(map[string]any{"grant_id": uuidToString(grant.ID), "scopes": scopes, "expires_at": expiresAt.UTC().Format(time.RFC3339)})
	if _, err := qtx.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "member", Valid: true},
		ActorID: member.UserID, Action: "run_capability_granted", Details: details,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record the grant")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create the grant")
		return
	}
	writeJSON(w, http.StatusCreated, runCapabilityGrantToResponse(grant))
}

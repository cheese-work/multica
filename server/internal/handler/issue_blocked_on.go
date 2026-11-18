package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Structured blocker (CHE-6972). `multica issue status <id> blocked
// --blocked-on <ref>` names what the issue waits for. Each ref form is the
// stored target of an existing wakeup condition flag. The ref is additive
// data: it is stored in the status_changed details and in metadata.blocked_on,
// and no behaviour reads it yet.
const (
	blockedOnMetadataKey = "blocked_on"
	blockedOnForms       = "issue:<id>[@done|ended|in_review|blocked], pr:<checks|merged|conflicts|dequeued>, " +
		"children[@<stage>], gh-run:<owner>/<repo>:<run-id>, decision:<request-id>"
	blockedOnUUID = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
)

var (
	blockedOnIssueRE    = regexp.MustCompile(`^issue:(` + blockedOnUUID + `)(?:@(?:done|ended|in_review|blocked))?$`)
	blockedOnPRRE       = regexp.MustCompile(`^pr:(?:checks|merged|conflicts|dequeued)$`)
	blockedOnChildrenRE = regexp.MustCompile(`^children(?:@[1-9][0-9]{0,8})?$`)
	blockedOnGHRunRE    = regexp.MustCompile(`^gh-run:[A-Za-z0-9._-]+/[A-Za-z0-9._-]+:[1-9][0-9]{0,17}$`)
	blockedOnDecisionRE = regexp.MustCompile(`^decision:(` + blockedOnUUID + `)$`)
)

// parseBlockedOn validates the request's blocked_on. It returns "" when the
// field is absent. The ref is refused on any move that does not end in
// blocked, when its form is unknown, and when an issue or decision it names is
// not in this workspace.
func (h *Handler) parseBlockedOn(ctx context.Context, w http.ResponseWriter, req *UpdateIssueRequest, rawFields map[string]json.RawMessage, workspaceID pgtype.UUID, statusKey string) (string, bool) {
	if _, present := rawFields["blocked_on"]; !present {
		return "", true
	}
	if statusKey != "blocked" {
		writeError(w, http.StatusBadRequest, "blocked_on is only accepted on a move to blocked")
		return "", false
	}
	if req.BlockedOn != nil {
		ref := *req.BlockedOn
		switch {
		case blockedOnPRRE.MatchString(ref), blockedOnChildrenRE.MatchString(ref), blockedOnGHRunRE.MatchString(ref):
			return ref, true
		case blockedOnIssueRE.MatchString(ref):
			_, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: parseUUID(blockedOnIssueRE.FindStringSubmatch(ref)[1]), WorkspaceID: workspaceID})
			return blockedOnTarget(w, ref, "issue", err)
		case blockedOnDecisionRE.MatchString(ref):
			_, err := h.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{ID: parseUUID(blockedOnDecisionRE.FindStringSubmatch(ref)[1]), WorkspaceID: workspaceID})
			return blockedOnTarget(w, ref, "decision request", err)
		}
	}
	writeError(w, http.StatusBadRequest, "blocked_on must be one of: "+blockedOnForms)
	return "", false
}

func blockedOnTarget(w http.ResponseWriter, ref, what string, err error) (string, bool) {
	switch {
	case err == nil:
		return ref, true
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusBadRequest, "blocked_on: "+what+" not found in this workspace")
	default:
		writeError(w, http.StatusInternalServerError, "failed to validate blocked_on")
	}
	return "", false
}

// recordBlockedOn stores ref in metadata.blocked_on on a move to blocked, in
// the status write's transaction. The key is dropped by the UpdateIssue and
// UpdateIssueStatus queries themselves, so any writer that leaves blocked
// clears it. It returns the issue row to publish.
func recordBlockedOn(ctx context.Context, q *db.Queries, issue db.Issue, ref string) (db.Issue, error) {
	if issue.Status != "blocked" || ref == "" {
		return issue, nil
	}
	if _, err := setIssueMetadataValue(ctx, q, issue.WorkspaceID, issue.ID, blockedOnMetadataKey, ref); err != nil {
		return issue, err
	}
	return q.GetIssue(ctx, issue.ID)
}

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	runProfileRestricted = "restricted"
	runProfileElevated   = "elevated"

	maxPolicyPatterns   = 64
	maxPolicyPatternLen = 256
)

// runProfile is the claim-time answer of CHE-2216 plan 3.1. It is a launch
// hint only: every elevated action is authorized again against live state.
type runProfile struct {
	Profile  string
	Scopes   []string
	Epoch    int64
	TaintIDs []string
}

// resolveRunProfile elevates only when an active grant exists and nothing
// removes elevation: a tainted task or issue, or a task fenced before. Scopes
// are the sorted union of the active grants.
func resolveRunProfile(grants []db.RunCapabilityGrant, taintIDs []string, issueTainted, fenced bool) runProfile {
	p := runProfile{Profile: runProfileRestricted, Scopes: []string{}, TaintIDs: append([]string{}, taintIDs...)}
	if len(grants) == 0 || len(taintIDs) > 0 || issueTainted || fenced {
		return p
	}
	p.Profile = runProfileElevated
	for _, g := range grants {
		p.Scopes = append(p.Scopes, g.Scopes...)
	}
	slices.Sort(p.Scopes)
	p.Scopes = slices.Compact(p.Scopes)
	return p
}

// runProfileForTask resolves the profile of a task on one issue. Taint marks
// and the fence state have no storage yet (B5a, B4a), so they enter as empty;
// a read error resolves restricted.
func (h *Handler) runProfileForTask(ctx context.Context, workspaceID, issueID pgtype.UUID) runProfile {
	grants, err := h.Queries.ListActiveRunCapabilityGrants(ctx, db.ListActiveRunCapabilityGrantsParams{WorkspaceID: workspaceID, IssueID: issueID})
	if err != nil {
		slog.Warn("run profile grant read failed; resolving restricted", "issue_id", uuidToString(issueID), "error", err)
		grants = nil
	}
	return resolveRunProfile(grants, nil, false, false)
}

// initialProtectedPaths is the policy a workspace has until an admin writes one
// under the same name (plan 3.2).
var initialProtectedPaths = []string{
	".github/**", "**/AGENTS.md", "**/CLAUDE.md", ".claude/**", ".codex/**", "skills/**", "standards/**",
	".opencodereview/**", "scripts/post-handoff-comment.sh", "scripts/check-handoff-links.sh",
}

var policyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// protectedPathPatterns returns the named policy, or the initial one when no
// admin has written it.
func (h *Handler) protectedPathPatterns(ctx context.Context, workspaceID pgtype.UUID, name string) ([]string, error) {
	row, err := h.Queries.GetProtectedPathPolicy(ctx, db.GetProtectedPathPolicyParams{WorkspaceID: workspaceID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return slices.Clone(initialProtectedPaths), nil
	}
	if err != nil {
		return nil, err
	}
	return row.Patterns, nil
}

func validPolicyPattern(p string) bool {
	return p != "" && utf8.RuneCountInString(p) <= maxPolicyPatternLen && !strings.ContainsRune(p, 0) &&
		!strings.HasPrefix(p, "/") && !slices.Contains(strings.Split(p, "/"), "..")
}

type setProtectedPathPolicyRequest struct {
	Patterns []string `json:"patterns"`
}

// SetProtectedPathPolicy replaces one named policy. Human owner/admin only: any
// agent credential gets 403, because a policy an agent can write is no policy.
func (h *Handler) SetProtectedPathPolicy(w http.ResponseWriter, r *http.Request) {
	id := workspaceIDFromURL(r, "id")
	actorType, _ := h.resolveActor(r, requestUserID(r), id)
	if isGovernedFieldMachineActor(r, actorType) {
		writeError(w, http.StatusForbidden, "only a person can write a protected-path policy")
		return
	}
	member, ok := h.requireWorkspaceRole(w, r, id, "workspace not found", "owner", "admin")
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	if !policyName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid policy name")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req setProtectedPathPolicyRequest
	if err := dec.Decode(&req); err != nil || len(req.Patterns) == 0 || len(req.Patterns) > maxPolicyPatterns || slices.ContainsFunc(req.Patterns, func(p string) bool { return !validPolicyPattern(p) }) {
		writeError(w, http.StatusBadRequest, "patterns must hold 1 to 64 relative paths without '..'")
		return
	}
	row, err := h.Queries.UpsertProtectedPathPolicy(r.Context(), db.UpsertProtectedPathPolicyParams{
		WorkspaceID: member.WorkspaceID, Name: name, Patterns: req.Patterns, UpdatedByMemberID: member.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save the policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": row.Name, "patterns": row.Patterns})
}

// intakePinForTask reports whether an intake item dispatched this task, the PR
// head ref it is pinned to and the PR's repository as host/owner/name (plan
// 3.6). Branch and repository are empty for a fork PR or a PR with no head ref
// or repository, and when the read fails: an intake run with no pin may touch
// no ref and no repository at all, so every failure path is the closed one.
func (h *Handler) intakePinForTask(ctx context.Context, workspaceID, taskID pgtype.UUID) (intake bool, branch, repo string) {
	pin, err := h.Queries.GetGitHubIntakePinForTask(ctx, db.GetGitHubIntakePinForTaskParams{WorkspaceID: workspaceID, TaskID: taskID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", ""
	}
	if err != nil {
		slog.Warn("intake pin read failed; pinning nothing", "task_id", uuidToString(taskID), "error", err)
		return true, "", ""
	}
	host := ""
	if u, err := url.Parse(pin.HtmlUrl); err == nil {
		host = u.Host
	}
	if !pin.HeadRepoSame || pin.Branch == "" || host == "" || pin.RepoOwner == "" || pin.RepoName == "" {
		return true, "", ""
	}
	return true, pin.Branch, strings.ToLower(host + "/" + pin.RepoOwner + "/" + pin.RepoName)
}

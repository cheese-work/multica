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
	// PinnedBaseSHA and PinnedHeadSHA seed the elevated run's synthetic base
	// (plan 3.5); empty when no grant pins one.
	PinnedBaseSHA string
	PinnedHeadSHA string
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

// runProfileForTask resolves the profile of a task on one issue and records the
// task's first elevated claim (task_elevation), whose scopes and epoch then stay
// frozen for the task. A task whose row left `elevated` is fenced for good. The
// task's taint IDs come from its row; the issue's from issue_taint, which only a
// member clear empties. Any read or write error resolves restricted.
func (h *Handler) runProfileForTask(ctx context.Context, workspaceID, issueID, taskID pgtype.UUID, taintIDs []string) runProfile {
	restricted := resolveRunProfile(nil, nil, false, false)
	grants, err := h.Queries.ListActiveRunCapabilityGrants(ctx, db.ListActiveRunCapabilityGrantsParams{WorkspaceID: workspaceID, IssueID: issueID})
	if err != nil {
		slog.Warn("run profile grant read failed; resolving restricted", "issue_id", uuidToString(issueID), "error", err)
		return restricted
	}
	issueTaint, err := h.Queries.GetIssueTaintIDs(ctx, issueID)
	if err != nil {
		slog.Warn("issue taint read failed; resolving restricted", "issue_id", uuidToString(issueID), "error", err)
		return restricted
	}
	issueTainted := len(issueTaint) > 0
	te, err := h.Queries.GetTaskElevation(ctx, taskID)
	known := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("task elevation read failed; resolving restricted", "task_id", uuidToString(taskID), "error", err)
		return restricted
	}
	if known && (len(taintIDs) > 0 || issueTainted) && te.State == elevationElevated {
		// Taint grew after the elevation was recorded (a resumed session, a
		// late assignment, tainted material on the issue): the stored row must not stay usable. Authorization
		// refuses tainted tasks on its own; a failed fence here is swept later.
		if _, err := h.Queries.FenceTaintedTaskElevations(ctx, taskID); err != nil {
			slog.Warn("tainted task elevation fence failed", "task_id", uuidToString(taskID), "error", err)
		}
	}
	rp := resolveRunProfile(grants, taintIDs, issueTainted, known && te.State != elevationElevated)
	if rp.Profile != runProfileElevated {
		return rp
	}
	if known {
		// The frozen grants must all still be live; the fence sweep may not have run yet.
		live, err := h.Queries.CountLiveGrantsByID(ctx, te.GrantIds)
		if err != nil || int(live) != len(te.GrantIds) {
			return restricted
		}
	} else {
		ids := make([]pgtype.UUID, len(grants))
		for i, g := range grants {
			ids[i] = g.ID
		}
		if err := h.Queries.InsertTaskElevation(ctx, db.InsertTaskElevationParams{
			TaskID: taskID, WorkspaceID: workspaceID, IssueID: issueID, GrantIds: ids, Scopes: rp.Scopes,
		}); err != nil {
			slog.Warn("task elevation write failed; resolving restricted", "task_id", uuidToString(taskID), "error", err)
			return restricted
		}
		if te, err = h.Queries.GetTaskElevation(ctx, taskID); err != nil {
			return restricted
		}
	}
	rp.Scopes, rp.Epoch = te.Scopes, te.Epoch
	rp.PinnedBaseSHA, rp.PinnedHeadSHA = grantPins(grants, te.GrantIds)
	return rp
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

// GetDaemonProtectedPathPolicy returns one named policy to the daemon push
// broker, which reads it live on every push.
func (h *Handler) GetDaemonProtectedPathPolicy(w http.ResponseWriter, r *http.Request) {
	workspaceID := strings.TrimSpace(chi.URLParam(r, "workspaceId"))
	if !h.requireDaemonWorkspaceAccess(w, r, workspaceID) {
		return
	}
	name := chi.URLParam(r, "name")
	if !policyName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid policy name")
		return
	}
	patterns, err := h.protectedPathPatterns(r.Context(), parseUUID(workspaceID), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "patterns": patterns})
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
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save the policy")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	var before map[string]any
	if prev, err := qtx.GetProtectedPathPolicy(r.Context(), db.GetProtectedPathPolicyParams{WorkspaceID: member.WorkspaceID, Name: name}); err == nil {
		before = protectedPathPolicyAuditFields(prev.Name, prev.Patterns)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to save the policy")
		return
	}
	row, err := qtx.UpsertProtectedPathPolicy(r.Context(), db.UpsertProtectedPathPolicyParams{
		WorkspaceID: member.WorkspaceID, Name: name, Patterns: req.Patterns, UpdatedByMemberID: member.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save the policy")
		return
	}
	if err := h.writeAudit(r.Context(), qtx, r, auditInput{
		Kind: "workspace.protected_path_policy.update", WorkspaceID: member.WorkspaceID, TargetID: member.WorkspaceID,
		Before: before, After: protectedPathPolicyAuditFields(row.Name, row.Patterns),
	}); err != nil {
		writeAuditFailure(w, r, err, "workspace.protected_path_policy.update")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
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

// grantPins is the pinned commit pair of the grants that elevated a task:
// only the frozen grants when the task's elevation is already recorded. A
// field with two different values, or no value, resolves empty and the daemon
// then refuses the elevated checkout.
func grantPins(grants []db.RunCapabilityGrant, frozen []pgtype.UUID) (base, head string) {
	bases, heads := map[string]bool{}, map[string]bool{}
	for _, g := range grants {
		if frozen != nil && !slices.Contains(frozen, g.ID) {
			continue
		}
		if g.PinnedBaseSha.Valid {
			bases[g.PinnedBaseSha.String] = true
		}
		if g.PinnedHeadSha.Valid {
			heads[g.PinnedHeadSha.String] = true
		}
	}
	if len(bases) > 1 || len(heads) > 1 {
		return "", ""
	}
	for base = range bases {
	}
	for head = range heads {
	}
	return base, head
}

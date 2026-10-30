package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Task-token write scoping (CHE-2216 plan 3.2, slice A5a). A run's task token
// writes to its own issue, that issue's descendants and the issues its task
// created; any other issue answers 403. Reads stay workspace-wide.
//
// This slice covers the issue routes (/api/issues/...). Every other write
// endpoint is audited against this middleware in A5b.
const (
	TaskWriteScopeOff   = "off"
	TaskWriteScopeAudit = "audit"
	TaskWriteScopeOn    = "on"

	// crossIssueScope is the grant scope that lets a coordinator run (decision
	// 3a) also write to its parent and sibling issues.
	crossIssueScope = "multica_cross_issue"

	maxWriteScopeBody = 1 << 20
)

// writeScopeReadOnlyPOSTs are fixed /api/issues/* POST routes that only read or
// create a new issue (a created issue is the task's own).
var writeScopeReadOnlyPOSTs = []string{"query", "table", "preview-trigger", "quick-create"}

// TaskTokenWriteScope returns the middleware for one enforcement mode: "on"
// refuses, "audit" logs what it would refuse and passes, anything else is off.
// It must run after Auth, which sets X-Actor-Source, X-Task-ID and
// X-Workspace-ID from the validated token. Other credentials pass untouched.
func (h *Handler) TaskTokenWriteScope(mode string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if mode != TaskWriteScopeOn && mode != TaskWriteScopeAudit {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Actor-Source") != "task_token" || r.Header.Get("X-Task-Token-Purpose") != "agent_task" || isReadMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			refs, bad := writeScopeTargets(r)
			if bad || !h.allTargetsInWriteScope(r, refs) {
				slog.Warn("task token write outside run scope",
					"mode", mode, "method", r.Method, "path", r.URL.Path,
					"task_id", r.Header.Get("X-Task-ID"), "agent_id", r.Header.Get("X-Agent-ID"))
				if mode == TaskWriteScopeOn {
					writeError(w, http.StatusForbidden, "task token may only write to its own issue, its descendants and issues it created")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isReadMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// writeScopeTargets lists the issue references a write request touches. bad is
// true when the request cannot be read in full, which fails closed.
func writeScopeTargets(r *http.Request) (refs []string, bad bool) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) < 2 || segs[0] != "api" || segs[1] != "issues" {
		return nil, false
	}
	rest := segs[2:]
	switch {
	case len(rest) == 0:
		return bodyParentRefs(r, false)
	case slices.Contains(writeScopeReadOnlyPOSTs, rest[0]) && r.Method == http.MethodPost:
		return nil, false
	case rest[0] == "batch-update" || rest[0] == "batch-delete":
		return batchTargets(r, rest[0] == "batch-update")
	case isReadOnlyIssuePOST(rest[1:]):
		return nil, false
	}
	refs = []string{rest[0]}
	if len(rest) == 1 && r.Method == http.MethodPut {
		parents, bad := bodyParentRefs(r, false)
		return append(refs, parents...), bad
	}
	return refs, false
}

// isReadOnlyIssuePOST matches the per-issue POST routes that only preview.
func isReadOnlyIssuePOST(sub []string) bool {
	return (len(sub) == 2 && sub[0] == "comments" && sub[1] == "trigger-preview") ||
		(len(sub) == 3 && sub[0] == "quick-actions" && sub[2] == "render")
}

// readBodyOnce reads a bounded request body and puts it back for the handler.
func readBodyOnce(r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxWriteScopeBody+1))
	if err != nil || len(b) > maxWriteScopeBody {
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, true
}

// bodyParentRefs returns the parent_issue_id of a create or update body: a
// parent outside the run's scope is a write to that parent. nested reads it
// from the "updates" object of a batch update.
func bodyParentRefs(r *http.Request, nested bool) (refs []string, bad bool) {
	b, ok := readBodyOnce(r)
	if !ok {
		return nil, true
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return nil, true
	}
	if nested {
		raw, ok := fields["updates"]
		if !ok || json.Unmarshal(raw, &fields) != nil {
			return nil, false
		}
	}
	raw, ok := fields["parent_issue_id"]
	if !ok {
		return nil, false
	}
	var parent *string
	if json.Unmarshal(raw, &parent) != nil {
		return nil, true
	}
	if parent == nil || *parent == "" {
		return nil, false
	}
	return []string{*parent}, false
}

func batchTargets(r *http.Request, isUpdate bool) (refs []string, bad bool) {
	b, ok := readBodyOnce(r)
	if !ok {
		return nil, true
	}
	var req struct {
		IssueIDs []string `json:"issue_ids"`
	}
	if json.Unmarshal(b, &req) != nil {
		return nil, true
	}
	refs = req.IssueIDs
	if isUpdate {
		parents, bad := bodyParentRefs(r, true)
		return append(refs, parents...), bad
	}
	return refs, false
}

// allTargetsInWriteScope reports whether every referenced issue is in the
// task's write scope, or the run holds a live multica_cross_issue grant on its
// own issue. Both are read per request, so a revoked grant stops the next write.
func (h *Handler) allTargetsInWriteScope(r *http.Request, refs []string) bool {
	workspaceID, err := util.ParseUUID(r.Header.Get("X-Workspace-ID"))
	if err != nil {
		return false
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		return false
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil {
		return false
	}
	crossIssue := func() bool {
		return task.IssueID.Valid && slices.Contains(h.runProfileForTask(r.Context(), workspaceID, task.IssueID).Scopes, crossIssueScope)
	}
	for _, ref := range refs {
		issueID, ok := h.resolveWriteScopeIssue(r, workspaceID, ref)
		if !ok {
			return false
		}
		if h.issueInWriteScope(r, workspaceID, taskID, task.IssueID, issueID) {
			continue
		}
		// Decision 3a: a live multica_cross_issue grant reaches the rest of the
		// bound workspace, never another one.
		if _, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: workspaceID}); err != nil || !crossIssue() {
			return false
		}
	}
	return true
}

// resolveWriteScopeIssue maps a UUID or PREFIX-NUMBER reference to an issue id.
// Anything it cannot resolve is out of scope.
func (h *Handler) resolveWriteScopeIssue(r *http.Request, workspaceID pgtype.UUID, ref string) (pgtype.UUID, bool) {
	if id, err := util.ParseUUID(ref); err == nil {
		return id, true
	}
	issue, ok := h.resolveIssueByIdentifier(r.Context(), ref, util.UUIDToString(workspaceID))
	return issue.ID, ok
}

func (h *Handler) issueInWriteScope(r *http.Request, workspaceID, taskID, ownIssueID, issueID pgtype.UUID) bool {
	if ownIssueID.Valid {
		inTree, err := h.Queries.IsIssueInRunIssueTree(r.Context(), db.IsIssueInRunIssueTreeParams{IssueID: issueID, WorkspaceID: workspaceID, OwnIssueID: ownIssueID})
		if err != nil {
			return false
		}
		if inTree {
			return true
		}
	}
	created, err := h.Queries.IsIssueCreatedByTask(r.Context(), db.IsIssueCreatedByTaskParams{IssueID: issueID, WorkspaceID: workspaceID, TaskID: taskID})
	return err == nil && created
}

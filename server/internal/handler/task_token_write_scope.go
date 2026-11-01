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

// writeScopeReadOnlyPOSTs are fixed /api/issues/* POST routes that only read.
var writeScopeReadOnlyPOSTs = []string{"query", "table", "preview-trigger"}

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
	case len(rest) == 0 || (rest[0] == "quick-create" && r.Method == http.MethodPost):
		return bodyParentRefs(r, false)
	case slices.Contains(writeScopeReadOnlyPOSTs, rest[0]) && r.Method == http.MethodPost:
		return nil, false
	case rest[0] == "batch-update" || rest[0] == "batch-delete":
		return batchTargets(r, rest[0] == "batch-update")
	case isReadOnlyIssuePOST(rest[1:]):
		return nil, false
	}
	refs = []string{rest[0]}
	// The routes that can set a parent: update and move.
	if (len(rest) == 1 && r.Method == http.MethodPut) || (len(rest) == 2 && rest[1] == "move") {
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
// A body over the bound, or one that fails to read, is put back whole, so audit
// mode forwards it unchanged; ok is false and the caller fails closed.
func readBodyOnce(r *http.Request) (body []byte, ok bool) {
	if r.Body == nil {
		return nil, true
	}
	orig := r.Body
	b, err := io.ReadAll(io.LimitReader(orig, maxWriteScopeBody+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), orig), orig}
	if err != nil || len(b) > maxWriteScopeBody {
		return nil, false
	}
	return b, true
}

// objectValues returns every value of a JSON object whose key equals name,
// ignoring case and keeping duplicates: the handlers decode into structs, where
// the last of several case-insensitive matches wins, so every one is checked.
func objectValues(b []byte, name string) (values []json.RawMessage, bad bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err == io.EOF {
		return nil, false
	}
	if err != nil {
		return nil, true
	}
	if tok == nil { // a JSON null body decodes to no fields
		return nil, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, true
	}
	for dec.More() {
		keyTok, err := dec.Token()
		key, isKey := keyTok.(string)
		if err != nil || !isKey {
			return nil, true
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return nil, true
		}
		if strings.EqualFold(key, name) {
			values = append(values, raw)
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, true
	}
	return values, false
}

// parentRefsOf returns every parent_issue_id set in a create, update, move or
// quick-create body: a parent outside the run's scope is a write to that
// parent. nested reads it from the "updates" objects of a batch update.
func parentRefsOf(b []byte, nested bool) (refs []string, bad bool) {
	objects := []json.RawMessage{b}
	if nested {
		var bad bool
		if objects, bad = objectValues(b, "updates"); bad {
			return nil, true
		}
	}
	for _, obj := range objects {
		values, bad := objectValues(obj, "parent_issue_id")
		if bad {
			return nil, true
		}
		for _, raw := range values {
			var parent *string
			if json.Unmarshal(raw, &parent) != nil {
				return nil, true
			}
			if parent != nil && *parent != "" {
				refs = append(refs, *parent)
			}
		}
	}
	return refs, false
}

func bodyParentRefs(r *http.Request, nested bool) (refs []string, bad bool) {
	b, ok := readBodyOnce(r)
	if !ok {
		return nil, true
	}
	return parentRefsOf(b, nested)
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
		parents, bad := parentRefsOf(b, true)
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
		// Only a task whose claim recorded an elevation holds the scope: a grant
		// that appears later must not elevate a running task, and
		// runProfileForTask would record that first claim.
		if !task.IssueID.Valid {
			return false
		}
		if _, err := h.Queries.GetTaskElevation(r.Context(), taskID); err != nil {
			return false
		}
		return slices.Contains(h.runProfileForTask(r.Context(), workspaceID, task.IssueID, task.ID, task.TaintIds).Scopes, crossIssueScope)
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

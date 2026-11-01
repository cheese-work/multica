package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const presetLabelColor = "#6b7280"

// issuePresetLabel finds the workspace issue label `preset:<name>`, creating
// it on first use.
func (h *Handler) issuePresetLabel(r *http.Request, workspaceID pgtype.UUID, name string) (db.IssueLabel, error) {
	find := func() (db.IssueLabel, bool, error) {
		labels, err := h.Queries.ListLabels(r.Context(), db.ListLabelsParams{WorkspaceID: workspaceID, ResourceType: "issue"})
		if err != nil {
			return db.IssueLabel{}, false, err
		}
		for _, l := range labels {
			if strings.EqualFold(l.Name, presetLabelPrefix+name) {
				return db.IssueLabel{ID: l.ID, WorkspaceID: l.WorkspaceID, ResourceType: l.ResourceType, Name: l.Name, Description: l.Description, Color: l.Color}, true, nil
			}
		}
		return db.IssueLabel{}, false, nil
	}
	if l, ok, err := find(); err != nil || ok {
		return l, err
	}
	l, err := h.Queries.CreateLabel(r.Context(), db.CreateLabelParams{
		WorkspaceID: workspaceID, ResourceType: "issue", Name: presetLabel(name), Color: presetLabelColor,
	})
	if isUniqueViolation(err) { // another request created it first
		l, _, err = find()
	}
	return l, err
}

func presetLabel(name string) string { return presetLabelPrefix + name }

type issuePresetResult struct {
	changed  bool
	revision int64
}

// replaceIssuePreset leaves the issue with at most the one `preset:*` label for
// keep (nil keeps none), and every other label untouched, in one transaction.
func (h *Handler) replaceIssuePreset(w http.ResponseWriter, r *http.Request, keep *db.IssueLabel) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	res, err := wakeupWrite(h, r, func(q *db.Queries) (issuePresetResult, error) {
		var out issuePresetResult
		// Serialize replacements: without the lock two requests both read an
		// empty label set and each attach their own preset.
		if _, err := q.LockIssueForAttachmentWrite(r.Context(), db.LockIssueForAttachmentWriteParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID}); err != nil {
			return out, err
		}
		current, err := q.ListLabelsByIssue(r.Context(), db.ListLabelsByIssueParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return out, err
		}
		for _, l := range current {
			if !strings.HasPrefix(l.Name, presetLabelPrefix) || (keep != nil && l.ID == keep.ID) {
				continue
			}
			d, err := q.DetachLabelFromIssue(r.Context(), db.DetachLabelFromIssueParams{IssueID: issue.ID, LabelID: l.ID, WorkspaceID: issue.WorkspaceID})
			if err != nil {
				return out, err
			}
			out.changed, out.revision = out.changed || d.Changed, max(out.revision, d.IssueRevision)
		}
		if keep != nil {
			a, err := q.AttachLabelToIssue(r.Context(), db.AttachLabelToIssueParams{IssueID: issue.ID, LabelID: keep.ID, WorkspaceID: issue.WorkspaceID})
			if err != nil {
				return out, err
			}
			out.changed, out.revision = out.changed || a.Changed, max(out.revision, a.IssueRevision)
		}
		return out, nil
	})
	if err != nil {
		slog.Warn("replace issue preset failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to set issue preset")
		return
	}
	labels, ok := h.listLabelsForIssueSafe(r, issue.ID, issue.WorkspaceID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	resp := labelsToResponse(labels)
	if res.changed {
		h.publish(protocol.EventIssueLabelsChanged, uuidToString(issue.WorkspaceID), "member", userID, map[string]any{
			"issue_id": uuidToString(issue.ID), "labels": resp, "issue_revision": res.revision,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"labels": resp})
}

// SetIssuePreset selects a preset for an issue by replacing its `preset:*`
// label (S1). The name is not checked against an agent: which agent runs the
// issue can change, and a name no agent has is skipped at claim.
func (h *Handler) SetIssuePreset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !presetNameOK(req.Name) {
		writeError(w, http.StatusBadRequest, "name must be a preset name or base")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	label, err := h.issuePresetLabel(r, issue.WorkspaceID, req.Name)
	if err != nil {
		slog.Warn("issue preset label failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to set issue preset")
		return
	}
	h.replaceIssuePreset(w, r, &label)
}

// ClearIssuePreset removes every `preset:*` label, so the issue falls back to
// the workflow step, the agent default or base.
func (h *Handler) ClearIssuePreset(w http.ResponseWriter, r *http.Request) {
	h.replaceIssuePreset(w, r, nil)
}

package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type DescriptionRevisionResponse struct {
	Revision  int64   `json:"revision"`
	ActorType *string `json:"actor_type"`
	ActorID   *string `json:"actor_id"`
	CreatedAt string  `json:"created_at"`
}

// ListIssueDescriptionRevisions lists every revision that changed the
// description, oldest first (CHE-5230). Only metadata: the text stays behind
// the diff endpoint, which applies the external-read gate.
func (h *Handler) ListIssueDescriptionRevisions(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, err := h.Queries.ListIssueDescriptionRevisions(r.Context(), db.ListIssueDescriptionRevisionsParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list description revisions")
		return
	}
	out := make([]DescriptionRevisionResponse, len(rows))
	for i, row := range rows {
		out[i] = DescriptionRevisionResponse{
			Revision:  row.Revision,
			ActorType: textToPtr(row.ActorType),
			ActorID:   uuidToPtr(row.ActorID),
			CreatedAt: timestampToString(row.CreatedAt),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": out})
}

// DiffIssueDescription returns a unified diff of the description between two
// issue revisions. A revision that did not touch the description resolves to
// the text in force at that revision.
func (h *Handler) DiffIssueDescription(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	from, okFrom := parseRevisionParam(w, r, "from")
	if !okFrom {
		return
	}
	to, okTo := parseRevisionParam(w, r, "to")
	if !okTo {
		return
	}
	// Same fail-closed rule as a marked attachment: no representation of a
	// withheld body, and a diff is one.
	if h.newReadFilter(r).withholds(issue.DescriptionExternalRead) {
		writeError(w, http.StatusForbidden, "description withheld: untrusted content is not readable by an elevated run")
		return
	}
	text := func(rev int64) (string, bool) {
		row, err := h.Queries.GetIssueDescriptionAtRevision(r.Context(), db.GetIssueDescriptionAtRevisionParams{
			IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, Revision: rev,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no description revision at or before "+strconv.FormatInt(rev, 10))
			return "", false
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load description revision")
			return "", false
		}
		return row.Description, true
	}
	if to > issue.Revision || from > issue.Revision {
		writeError(w, http.StatusNotFound, "unknown revision; issue is at revision "+strconv.FormatInt(issue.Revision, 10))
		return
	}
	a, ok := text(from)
	if !ok {
		return
	}
	b, ok := text(to)
	if !ok {
		return
	}
	diff, err := unifiedDiff(a, b, "revision "+strconv.FormatInt(from, 10), "revision "+strconv.FormatInt(to, 10))
	if errors.Is(err, errDiffTooLarge) {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build diff")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "diff": diff})
}

func parseRevisionParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, name+" must be a positive integer revision")
		return 0, false
	}
	return n, true
}

package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type revisionView struct {
	Revision int32   `json:"revision"`
	DueDate  *string `json:"due_date"`
}

func readRevision(t *testing.T, id string) revisionView {
	t.Helper()
	var v revisionView
	req := withURLParam(newRequest("GET", "/api/issues/"+id, nil), "id", id)
	testutil.Call(t, testHandler.GetIssue, req).Want(http.StatusOK).JSON(&v)
	return v
}

// A writer that re-sends the stored due date must still move the revision, so a
// fence on the revision of an earlier write sees that someone wrote since.
func TestExplicitSameValueDueDateWriteMovesRevision(t *testing.T) {
	id := dbfx.Issue(t, "same-date write", testutil.Cols{"due_date": "2026-12-01"})
	before := readRevision(t, id)

	putIssue(t, id, map[string]any{"due_date": "2026-12-01"}).Want(http.StatusOK)
	after := readRevision(t, id)
	if after.Revision != before.Revision+1 {
		t.Fatalf("same-date write: revision %d -> %d, want +1", before.Revision, after.Revision)
	}
	if after.DueDate == nil || *after.DueDate != "2026-12-01" {
		t.Fatalf("same-date write changed the date: %v", after.DueDate)
	}
}

func TestExplicitSameValueDueDateWriteHonoursExpectedRevision(t *testing.T) {
	id := dbfx.Issue(t, "same-date fence", testutil.Cols{"due_date": "2026-12-01"})
	mine := readRevision(t, id).Revision

	putIssue(t, id, map[string]any{"due_date": "2026-12-01"}).Want(http.StatusOK) // another writer, same value
	putIssue(t, id, map[string]any{"due_date": "2026-12-08", "expected_revision": mine}).Want(http.StatusConflict)
	if got := readRevision(t, id); got.DueDate == nil || *got.DueDate != "2026-12-01" {
		t.Fatalf("a fenced write must not land: %v", got.DueDate)
	}
}

func TestWriteWithoutDueDateLeavesRevisionAlone(t *testing.T) {
	id := dbfx.Issue(t, "no due field", testutil.Cols{"due_date": "2026-12-01", "priority": "high"})
	before := readRevision(t, id)

	putIssue(t, id, map[string]any{"priority": "high"}).Want(http.StatusOK) // no field changes, no due_date sent
	if after := readRevision(t, id); after.Revision != before.Revision {
		t.Fatalf("a no-op write without due_date moved the revision: %d -> %d", before.Revision, after.Revision)
	}
}

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CHE-1119: pins when the parent's child_done rule wakes, for every way a
// child can end (done, cancelled and the two review verdicts, which live in
// the done category) across unstaged, staged and mixed sibling sets. The two
// verdicts are re-entrant, but the rule reads categories, so they close a
// child exactly like done.
var childEndings = []struct{ status, category string }{
	{"done", ""},
	{"cancelled", ""},
	{"changes_requested", "done"},
	{"blockings_found", "done"},
}

func prepareEnding(t *testing.T, status, category string) {
	t.Helper()
	if category != "" {
		createTestCustomStatus(t, status, category)
	}
}

func listChildrenFor(t *testing.T, parentID string) map[string]IssueResponse {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.ListChildIssues(w, withURLParam(newRequest("GET", "/api/issues/"+parentID+"/children", nil), "id", parentID))
	if w.Code != http.StatusOK {
		t.Fatalf("list children: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Issues []IssueResponse `json:"issues"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	byID := map[string]IssueResponse{}
	for _, c := range resp.Issues {
		byID[c.ID] = c
	}
	return byID
}

func completeParentRuns(t *testing.T, parentID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE issue_id = $1`, parentID)
}

func TestChildDoneEndingsUnstagedSet(t *testing.T) {
	for _, e := range childEndings {
		t.Run(e.status, func(t *testing.T) {
			prepareEnding(t, e.status, e.category)
			fx := newChildDoneFixture(t, "in_progress")
			setIssueAssigneeDirect(t, fx.parent.ID, "agent", handlerTestAgentID(t))
			other := createUnstagedChild(t, fx.parent.ID, "in_progress")

			updateChildStatus(t, fx.child.ID, e.status)
			if got := len(childDoneEntries(t, fx.parent.ID)); got != 0 {
				t.Fatalf("%s with an open sibling fired %d entries", e.status, got)
			}
			updateChildStatus(t, other.ID, e.status)
			entries := childDoneEntries(t, fx.parent.ID)
			if len(entries) != 1 || entries[0].Outcome != "woke" || entries[0].Stage != nil || entries[0].Total != 2 {
				t.Fatalf("%s closing the last unstaged child: entries = %+v, want one wake for all", e.status, entries)
			}
		})
	}
}

func TestChildDoneEndingsStagedSet(t *testing.T) {
	for _, e := range childEndings {
		t.Run(e.status, func(t *testing.T) {
			prepareEnding(t, e.status, e.category)
			fx := newChildDoneFixture(t, "in_progress")
			setIssueAssigneeDirect(t, fx.parent.ID, "agent", handlerTestAgentID(t))
			dbfx.Exec(t, `UPDATE issue SET stage = 1 WHERE id = $1`, fx.child.ID)
			last := createStagedChild(t, fx.parent.ID, 2, "in_progress")

			updateChildStatus(t, fx.child.ID, e.status)
			entries := childDoneEntries(t, fx.parent.ID)
			if len(entries) != 1 || entries[0].Stage == nil || *entries[0].Stage != 1 || entries[0].Outcome != "woke" {
				t.Fatalf("%s closing stage 1: entries = %+v, want one stage-1 wake", e.status, entries)
			}
			completeParentRuns(t, fx.parent.ID)

			updateChildStatus(t, last.ID, e.status)
			entries = childDoneEntries(t, fx.parent.ID)
			if len(entries) != 2 || entries[1].Stage != nil || entries[1].Total != 2 || entries[1].Outcome != "woke" {
				t.Fatalf("%s closing the last stage: entries = %+v, want a second wake for all", e.status, entries)
			}
		})
	}
}

// The last staged stage closing while an unstaged child is open wakes nobody:
// the wrap-up waits for the unstaged child. Callers that need the stage wake
// must stage every child (documented limitation of the mixed set).
func TestChildDoneEndingsMixedSet(t *testing.T) {
	for _, e := range childEndings {
		t.Run(e.status, func(t *testing.T) {
			prepareEnding(t, e.status, e.category)
			fx := newChildDoneFixture(t, "in_progress") // unstaged and open
			setIssueAssigneeDirect(t, fx.parent.ID, "agent", handlerTestAgentID(t))
			staged := createStagedChild(t, fx.parent.ID, 1, "in_progress")

			updateChildStatus(t, staged.ID, e.status)
			if got := len(childDoneEntries(t, fx.parent.ID)); got != 0 {
				t.Fatalf("%s closing the only stage with an unstaged sibling open fired %d entries", e.status, got)
			}
			updateChildStatus(t, fx.child.ID, e.status)
			entries := childDoneEntries(t, fx.parent.ID)
			if len(entries) != 1 || entries[0].Stage != nil || entries[0].Total != 2 || entries[0].Outcome != "woke" {
				t.Fatalf("%s closing the unstaged child: entries = %+v, want one wake for all", e.status, entries)
			}
		})
	}
}

// A review verdict closes a child for the rule but is re-entrant: sending the
// child back to work and closing it again is a new fact and wakes once more.
func TestChildDoneReviewVerdictReopenWakesAgain(t *testing.T) {
	for _, status := range []string{"changes_requested", "blockings_found"} {
		t.Run(status, func(t *testing.T) {
			createTestCustomStatus(t, status, "done")
			fx := newChildDoneFixture(t, "in_progress")
			setIssueAssigneeDirect(t, fx.parent.ID, "agent", handlerTestAgentID(t))

			updateChildStatus(t, fx.child.ID, status)
			completeParentRuns(t, fx.parent.ID)
			updateChildStatus(t, fx.child.ID, "in_progress")
			updateChildStatus(t, fx.child.ID, "done")
			if got := len(childDoneEntries(t, fx.parent.ID)); got != 2 {
				t.Fatalf("entries = %d, want 2 (verdict, then done after rework)", got)
			}
		})
	}
}

// Reproduces the CHE-821 report: a child finishing under a CHILD of the parent
// is a grandchild event. The grandparent's rule reads its own direct children
// only, so it must not wake, and `issue children` reports done: 0 for its
// stage because the direct child is still open, while a direct lookup of the
// grandchild says done. Both are correct.
func TestChildDoneGrandchildDoesNotWakeGrandparent(t *testing.T) {
	grand := newChildDoneFixture(t, "in_progress") // grand.parent is the grandparent
	setIssueAssigneeDirect(t, grand.parent.ID, "agent", handlerTestAgentID(t))
	mid := createStagedChild(t, grand.parent.ID, 1, "in_review")
	leaf := createUnstagedChild(t, mid.ID, "in_progress")

	updateChildStatus(t, leaf.ID, "done")
	if got := len(childDoneEntries(t, grand.parent.ID)); got != 0 {
		t.Fatalf("a grandchild finishing woke the grandparent: %d entries", got)
	}
	if got := len(childDoneEntries(t, mid.ID)); got != 1 {
		t.Fatalf("the direct parent of the grandchild: %d entries, want 1", got)
	}
	children := listChildrenFor(t, grand.parent.ID)
	if c := children[mid.ID]; c.Status != "in_review" || c.StatusCategory != "in_review" {
		t.Fatalf("direct child = %q/%q, want in_review (counts as not done)", c.Status, c.StatusCategory)
	}
	if c := listChildrenFor(t, mid.ID)[leaf.ID]; c.Status != "done" || c.StatusCategory != "done" {
		t.Fatalf("grandchild = %q/%q, want done", c.Status, c.StatusCategory)
	}
}

// `issue children` counts progress from status_category; every ending must
// read as terminal there exactly as it does in the rule.
func TestListChildIssuesEndingsCategory(t *testing.T) {
	for _, e := range childEndings {
		t.Run(e.status, func(t *testing.T) {
			prepareEnding(t, e.status, e.category)
			fx := newChildDoneFixture(t, "in_progress")
			updateChildStatus(t, fx.child.ID, e.status)
			want := "done"
			if e.status == "cancelled" {
				want = "cancelled"
			}
			if c := listChildrenFor(t, fx.parent.ID)[fx.child.ID]; c.StatusCategory != want {
				t.Fatalf("%s: status_category = %q, want %q", e.status, c.StatusCategory, want)
			}
		})
	}
}

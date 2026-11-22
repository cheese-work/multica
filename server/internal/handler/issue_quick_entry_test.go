package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7681 (slice 16 of CHE-7658): every bind into Quick writes the activity
// `quick_entry` with a server-computed `full_workflow_id`, the workflow the
// issue followed just before. The draw writes it for both arms (arm D records
// its route too); an issue-level override into Quick writes it with the
// binding the issue had: its previous override, else the title map, else the
// project's workflow. Quick is the workflow named "Quick", or the one the
// quick_first setting names.

// quickEntry is one `quick_entry` row: details.arm is set on a draw row only;
// full_workflow_id is empty when the issue had no binding to return to.
type quickEntry struct {
	Source         string `json:"source"`
	Arm            string `json:"arm"`
	FullWorkflowID string `json:"full_workflow_id"`
}

func quickEntries(t *testing.T, issueID string) []quickEntry {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT details FROM activity_log WHERE issue_id = $1 AND action = 'quick_entry' ORDER BY created_at, id`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []quickEntry
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var e quickEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("quick_entry details %s: %v", raw, err)
		}
		out = append(out, e)
	}
	return out
}

func wantQuickEntries(t *testing.T, issueID string, want ...quickEntry) {
	t.Helper()
	got := quickEntries(t, issueID)
	if len(got) != len(want) {
		t.Fatalf("quick_entry rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("quick_entry row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestQuickEntryDrawWritesBothArms(t *testing.T) {
	f := newQuickFirstFixture(t)

	t.Run("project route", func(t *testing.T) {
		f.setting(t, f.on(nil), nil)
		wantQ := quickEntry{Source: "arm", Arm: "Q", FullWorkflowID: f.mapped}
		wantD := quickEntry{Source: "arm", Arm: "D", FullWorkflowID: f.mapped}
		wantQuickEntries(t, f.create(t, true, nil).ID, wantQ)
		wantQuickEntries(t, f.create(t, false, nil).ID, wantD)
	})
	t.Run("title-mapped route outside any project", func(t *testing.T) {
		f.setting(t, f.on(nil), f.byType())
		wantQuickEntries(t, f.create(t, true, map[string]any{"project_id": ""}).ID,
			quickEntry{Source: "arm", Arm: "Q", FullWorkflowID: f.mapped})
		wantQuickEntries(t, f.create(t, false, map[string]any{"project_id": ""}).ID,
			quickEntry{Source: "arm", Arm: "D", FullWorkflowID: f.mapped})
	})
	t.Run("a child of a Quick parent records the route it had", func(t *testing.T) {
		f.setting(t, f.on(nil), nil)
		parent := f.create(t, true, nil)
		nextNumber(t, false)
		var child quickCreated
		testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
			"title": "feat(x): child " + workflowTestSuffix(), "project_id": f.projectID, "parent_issue_id": parent.ID,
		})).Want(http.StatusCreated).JSON(&child)
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, child.ID) })
		wantQuickEntries(t, child.ID, quickEntry{Source: "arm", Arm: "Q", FullWorkflowID: f.mapped})
	})
	t.Run("an issue left out of the draw writes nothing", func(t *testing.T) {
		f.setting(t, f.on(nil), nil)
		wantQuickEntries(t, f.create(t, true, map[string]any{"title": "docs(x): not listed " + workflowTestSuffix()}).ID)
		f.setting(t, nil, nil)
		wantQuickEntries(t, f.create(t, true, nil).ID)
	})
}

type overrideFixture struct {
	quickFirstFixture
	quickByName string // a workflow named Quick
	other       string // a second workflow the issue can follow before
}

func newOverrideFixture(t *testing.T) overrideFixture {
	t.Helper()
	f := overrideFixture{quickFirstFixture: newQuickFirstFixture(t)}
	f.quickByName = createTestWorkflow(t, "Quick", "todo", "in_progress", "in_review").ID
	f.other = createTestWorkflow(t, "quick-entry-other-"+workflowTestSuffix(), "todo", "in_progress", "in_review").ID
	return f
}

func (f overrideFixture) issue(t *testing.T, title, projectID string) string {
	t.Helper()
	cols := testutil.Cols{"status": "todo"}
	if projectID != "" {
		cols["project_id"] = projectID
	}
	return dbfx.Issue(t, title, cols)
}

func TestQuickEntryMemberOverrideRecordsThePriorBinding(t *testing.T) {
	f := newOverrideFixture(t)

	t.Run("previous override wins", func(t *testing.T) {
		f.setting(t, nil, f.byType())
		id := f.issue(t, "feat(x): override first "+workflowTestSuffix(), f.projectID)
		setIssueOverride(t, id, f.other)
		putIssueWorkflow(t, id, f.quickByName).Want(http.StatusOK)
		wantQuickEntries(t, id, quickEntry{Source: "override", FullWorkflowID: f.other})
	})
	t.Run("title map before the project", func(t *testing.T) {
		f.setting(t, nil, map[string]string{"feat": f.other})
		id := f.issue(t, "feat(x): map first "+workflowTestSuffix(), f.projectID)
		putIssueWorkflow(t, id, f.quickByName).Want(http.StatusOK)
		wantQuickEntries(t, id, quickEntry{Source: "override", FullWorkflowID: f.other})
	})
	t.Run("project workflow last", func(t *testing.T) {
		f.setting(t, nil, nil)
		id := f.issue(t, "feat(x): project "+workflowTestSuffix(), f.projectID)
		putIssueWorkflow(t, id, f.quickByName).Want(http.StatusOK)
		wantQuickEntries(t, id, quickEntry{Source: "override", FullWorkflowID: f.mapped})
	})
	t.Run("no prior binding leaves the target empty", func(t *testing.T) {
		f.setting(t, nil, nil)
		id := f.issue(t, "feat(x): unbound "+workflowTestSuffix(), "")
		putIssueWorkflow(t, id, f.quickByName).Want(http.StatusOK)
		wantQuickEntries(t, id, quickEntry{Source: "override"})
	})
	t.Run("the quick_first workflow counts as Quick whatever its name", func(t *testing.T) {
		f.setting(t, f.on(nil), nil)
		id := f.issue(t, "feat(x): by setting "+workflowTestSuffix(), f.projectID)
		putIssueWorkflow(t, id, f.quick).Want(http.StatusOK)
		wantQuickEntries(t, id, quickEntry{Source: "override", FullWorkflowID: f.mapped})
	})
	t.Run("a later bind appends a row", func(t *testing.T) {
		f.setting(t, nil, nil)
		id := f.issue(t, "feat(x): twice "+workflowTestSuffix(), f.projectID)
		putIssueWorkflow(t, id, f.quickByName).Want(http.StatusOK)
		putIssueWorkflow(t, id, f.other).Want(http.StatusOK)
		putIssueWorkflow(t, id, f.quickByName).Want(http.StatusOK)
		wantQuickEntries(t, id,
			quickEntry{Source: "override", FullWorkflowID: f.mapped},
			quickEntry{Source: "override", FullWorkflowID: f.other})
	})
	t.Run("a bind that is not into Quick writes nothing", func(t *testing.T) {
		f.setting(t, nil, nil)
		id := f.issue(t, "feat(x): elsewhere "+workflowTestSuffix(), f.projectID)
		putIssueWorkflow(t, id, f.other).Want(http.StatusOK)
		putIssueWorkflow(t, id, "").Want(http.StatusOK)
		wantQuickEntries(t, id)
	})
	t.Run("a no-op switch writes nothing", func(t *testing.T) {
		f.setting(t, nil, nil)
		id := f.issue(t, "feat(x): noop "+workflowTestSuffix(), f.projectID)
		setIssueOverride(t, id, f.quickByName)
		putIssueWorkflow(t, id, f.quickByName).Want(http.StatusOK)
		wantQuickEntries(t, id)
	})
	t.Run("a refused switch writes nothing", func(t *testing.T) {
		f.setting(t, nil, nil)
		id := dbfx.Issue(t, "feat(x): refused "+workflowTestSuffix(), testutil.Cols{"project_id": f.projectID, "status": "in_review"})
		// Free the name Quick, then take it with a workflow that has no in_review.
		dbfx.Exec(t, `UPDATE issue_workflow SET name = 'Quick old ' || id::text WHERE id = $1`, f.quickByName)
		narrow := createTestWorkflow(t, "Quick", "todo")
		putIssueWorkflow(t, id, narrow.ID).Want(http.StatusBadRequest)
		wantQuickEntries(t, id)
	})
}

func TestQuickEntryAgentOverrideRecordsOnceApproved(t *testing.T) {
	f := newRebindFixture(t, 7681100)
	dbfx.Exec(t, `UPDATE issue_workflow SET name = 'Quick' WHERE id = $1`, f.wide)

	var opened workflowSetOpened
	taskTokenSetWorkflow(t, f, map[string]any{"workflow_id": f.wide}).Want(http.StatusAccepted).JSON(&opened)
	wantQuickEntries(t, f.c.issueID) // a pending card binds nothing

	card := openWorkflowSetCard(t, f)
	if out := card.approve(t); out.State != "executed" {
		t.Fatalf("outcome = %+v, want executed", out)
	}
	wantQuickEntries(t, f.c.issueID, quickEntry{Source: "override", FullWorkflowID: f.narrow})
}

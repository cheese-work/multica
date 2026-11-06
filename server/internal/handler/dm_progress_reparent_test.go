package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Reporting follows the original commission (CHE-3473 D4 = A, M8): a move
// changes the tree, never who follows.

func updateIssueField(t *testing.T, issue string, body map[string]any) {
	t.Helper()
	req := withURLParam(newRequest("PUT", "/api/issues/"+issue, body), "id", issue)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
}

func movedUnderEvents(t *testing.T, issue, lane string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM dm_progress_lane_event WHERE issue_id = $1 AND lineage_id = $2 AND kind = 'moved_under'`, issue, lane)
}

// reparentWorld: lane A (testUser) started `moved`; lane B (another member)
// started `newParent`.
type reparentWorld struct {
	laneA, laneB, moved, newParent, other string
}

func newReparentWorld(t *testing.T) reparentWorld {
	t.Helper()
	w := newDMWorld(t)
	other := createSecondWorkspaceMember(t)
	laneA, laneB := w.dm(t, testUserID), w.dm(t, other)
	return reparentWorld{
		laneA: laneA, laneB: laneB, other: other,
		moved:     w.agentIssue(t, w.task(t, laneA, testUserID), map[string]any{"title": "moved " + t.Name()}),
		newParent: w.agentIssue(t, w.task(t, laneB, other), map[string]any{"title": "new parent " + t.Name()}),
	}
}

func TestDMProgress_ReparentKeepsTheOriginFollowAndNotTheNewParentLane(t *testing.T) {
	w := newReparentWorld(t)
	child := childOf(t, w.moved, "child of the moved issue")

	updateIssueField(t, w.moved, map[string]any{"parent_issue_id": w.newParent})

	rows := followRows(t, w.moved)
	if len(rows) != 1 || rows[0].Lineage != w.laneA || rows[0].State != "active" {
		t.Fatalf("moved issue follows = %+v, want one active follow in the origin lane %s", rows, w.laneA)
	}
	if got := reportingRoot(t, w.moved); got != w.moved {
		t.Errorf("moved issue reports to %s, want itself %s", got, w.moved)
	}
	if got := followState(t, w.moved, w.laneB); got != "" {
		t.Errorf("new parent's lane follows the moved issue (%q), want no follow without an explicit one", got)
	}
	if got := reportingRoot(t, child); got != w.moved {
		t.Errorf("child of the moved issue reports to %s, want %s", got, w.moved)
	}
	// Children created later still inherit the existing follow.
	later := childOf(t, w.moved, "created after the move")
	if got, root := followState(t, later, w.laneA), reportingRoot(t, later); got != "active" || root != w.moved {
		t.Errorf("later child follow = %q, root %s; want active in the origin lane, root %s", got, root, w.moved)
	}
	if got := followState(t, later, w.laneB); got != "" {
		t.Errorf("later child follows in the new parent's lane (%q), want none", got)
	}
}

func TestDMProgress_ExplicitFollowIsHowTheNewParentLaneGetsTheIssue(t *testing.T) {
	w := newReparentWorld(t)
	updateIssueField(t, w.moved, map[string]any{"parent_issue_id": w.newParent})

	req := progressReq("POST", w.moved, "", w.laneB)
	req.Header.Set("X-User-ID", w.other)
	testutil.Call(t, testHandler.FollowIssueProgress, req).Want(http.StatusOK)

	rows := followRows(t, w.moved)
	if len(rows) != 2 {
		t.Fatalf("moved issue follows = %+v, want origin lane plus the explicit follow", rows)
	}
	if got := reportingRoot(t, w.moved); got != w.moved {
		t.Errorf("explicit follow changed the root to %s", got)
	}
}

func TestDMProgress_DetachKeepsTheFollowAndTheRoot(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	root := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	child := childOf(t, root, "to detach")
	before := followRows(t, child)

	updateIssueField(t, child, map[string]any{"parent_issue_id": nil})

	after := followRows(t, child)
	if len(after) != 1 || after[0].State != "active" || after[0].Lineage != dm || len(before) != 1 || after[0].Reasons[0] != before[0].Reasons[0] {
		t.Fatalf("follows after detach = %+v, want the one inherited follow unchanged (%+v)", after, before)
	}
	if got := reportingRoot(t, child); got != root {
		t.Errorf("detached issue reports to %s, want its original root %s", got, root)
	}
}

func TestDMProgress_ReparentWritesOneMovedUnderEventToTheOriginLane(t *testing.T) {
	w := newReparentWorld(t)

	updateIssueField(t, w.moved, map[string]any{"title": "not a move"})
	if n := movedUnderEvents(t, w.moved, w.laneA); n != 0 {
		t.Fatalf("a title edit wrote %d moved_under events, want 0", n)
	}

	updateIssueField(t, w.moved, map[string]any{"parent_issue_id": w.newParent})
	if n := movedUnderEvents(t, w.moved, w.laneA); n != 1 {
		t.Fatalf("origin lane has %d moved_under events, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_lane_event WHERE issue_id = $1 AND lineage_id = $2 AND related_issue_id = $3`, w.moved, w.laneA, w.newParent); n != 1 {
		t.Errorf("the event does not name the new parent")
	}
	if n := movedUnderEvents(t, w.moved, w.laneB); n != 0 {
		t.Errorf("new parent's lane got %d moved_under events, want 0", n)
	}
	// Writing the same parent again is not a move.
	updateIssueField(t, w.moved, map[string]any{"parent_issue_id": w.newParent})
	if n := movedUnderEvents(t, w.moved, w.laneA); n != 1 {
		t.Errorf("a no-op parent write left %d events, want still 1", n)
	}
}

// failMovedUnderEvents makes every moved_under insert for issue fail, so a test
// can prove the event and the reparent commit or roll back together.
func failMovedUnderEvents(t *testing.T, issue string) {
	t.Helper()
	name := "che3548_fail_" + issue[:8]
	ctx := t.Context()
	stmts := []string{
		`CREATE OR REPLACE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced moved_under failure'; END $$`,
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON dm_progress_lane_event FOR EACH ROW WHEN (NEW.issue_id = '%s') EXECUTE FUNCTION %s()`, name, issue, name),
	}
	for _, stmt := range stmts {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+name+` ON dm_progress_lane_event`)
		testPool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+name+`()`)
	})
}

func parentIDText(t *testing.T, issue string) string {
	t.Helper()
	var parent string
	if err := testPool.QueryRow(t.Context(), `SELECT COALESCE(parent_issue_id::text, '') FROM issue WHERE id = $1`, issue).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	return parent
}

func TestDMProgress_ReparentAndItsEventCommitTogether(t *testing.T) {
	w := newReparentWorld(t)
	failMovedUnderEvents(t, w.moved)

	req := withURLParam(newRequest("PUT", "/api/issues/"+w.moved, map[string]any{"parent_issue_id": w.newParent}), "id", w.moved)
	if rr := testutil.Call(t, testHandler.UpdateIssue, req); rr.Code == http.StatusOK {
		t.Fatalf("reparent succeeded although its moved_under event could not be written")
	}
	if got := parentIDText(t, w.moved); got != "" {
		t.Errorf("moved issue has parent %s after a failed event write, want the reparent rolled back", got)
	}
}

func TestDMProgress_BatchReparentAndItsEventCommitTogether(t *testing.T) {
	w := newReparentWorld(t)
	failMovedUnderEvents(t, w.moved)

	body := map[string]any{"issue_ids": []string{w.moved}, "updates": map[string]any{"parent_issue_id": w.newParent}}
	testutil.Call(t, testHandler.BatchUpdateIssues, newRequest("POST", "/api/issues/batch-update", body))
	if got := parentIDText(t, w.moved); got != "" {
		t.Errorf("moved issue has parent %s after a failed event write, want the reparent rolled back", got)
	}
}

func TestDMProgress_BatchReparentWritesTheMovedUnderEvent(t *testing.T) {
	w := newReparentWorld(t)
	body := map[string]any{"issue_ids": []string{w.moved}, "updates": map[string]any{"parent_issue_id": w.newParent}}
	testutil.Call(t, testHandler.BatchUpdateIssues, newRequest("POST", "/api/issues/batch-update", body)).Want(http.StatusOK)
	if n := movedUnderEvents(t, w.moved, w.laneA); n != 1 {
		t.Fatalf("origin lane has %d moved_under events after a batch reparent, want 1", n)
	}
	if got := followState(t, w.moved, w.laneB); got != "" {
		t.Errorf("batch reparent gave the new parent's lane a follow (%q)", got)
	}
}

func TestDMProgress_ProjectMoveChangesNoFollow(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	root := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	child := childOf(t, root, "project mover")
	project := createChatProjectTestProject(t, testWorkspaceID, "move target", "")
	snapshot := func() []dmFollowRow { return append(followRows(t, root), followRows(t, child)...) }
	before := snapshot()
	beforeFollowCount := dbfx.Count(t, `SELECT count(*) FROM dm_progress_follow WHERE lineage_id = $1`, dm)

	updateIssueField(t, child, map[string]any{"project_id": project})

	after := snapshot()
	if len(before) != len(after) {
		t.Fatalf("follows before %+v, after %+v", before, after)
	}
	for i := range before {
		if before[i].Lineage != after[i].Lineage || before[i].State != after[i].State || before[i].Muted != after[i].Muted || len(before[i].Reasons) != len(after[i].Reasons) {
			t.Errorf("follow %d changed by a project move: before %+v, after %+v", i, before[i], after[i])
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_follow WHERE lineage_id = $1`, dm); n != beforeFollowCount {
		t.Errorf("lane has %d follows after a project move, want %d", n, beforeFollowCount)
	}
	if got := reportingRoot(t, child); got != root {
		t.Errorf("project move changed the root to %s", got)
	}
	if n := movedUnderEvents(t, child, dm); n != 0 {
		t.Errorf("a project move wrote %d moved_under events, want 0", n)
	}
}

func TestDMProgress_DeletingTheMovedIssueSweepsItsLaneEvents(t *testing.T) {
	w := newReparentWorld(t)
	updateIssueField(t, w.moved, map[string]any{"parent_issue_id": w.newParent})
	req := withURLParam(newRequest("DELETE", "/api/issues/"+w.moved, nil), "id", w.moved)
	testutil.Call(t, testHandler.DeleteIssue, req).Want(http.StatusNoContent)
	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_lane_event WHERE issue_id = $1`, w.moved); n != 0 {
		t.Fatalf("%d lane events left behind after the issue was deleted", n)
	}
}

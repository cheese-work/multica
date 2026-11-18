package handler

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Unlinked roots, duplicates, deletion, reopen and epic completion (CHE-3550,
// CHE-3473 M9 D5, M10 D6, M13).

func TestDMProgress_UnlinkedRootFromASquadGetsNoFollowUntilExplicitAttach(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	// A squad run is an issue run: an agent task with no DM, started by the
	// same member who owns the DM. Creator and initiator must not link it.
	root := w.agentIssue(t, w.task(t, "", testUserID), map[string]any{"title": "squad root " + t.Name()})
	if rows := followRows(t, root); len(rows) != 0 {
		t.Fatalf("follows before an attach = %+v, want none", rows)
	}

	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", root, "", dm)).Want(http.StatusOK)
	rows := followRows(t, root)
	if len(rows) != 1 || rows[0].Lineage != dm || !slices.Equal(rows[0].Reasons, []string{"manual"}) {
		t.Fatalf("follows after the attach = %+v, want one manual follow in %s", rows, dm)
	}
	if got := reportingRoot(t, root); got != root {
		t.Errorf("attached root reports to %s, want itself %s", got, root)
	}
}

func TestDMProgress_ChildCreatedByAnIssueRunInheritsTheParentFollow(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	root := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	child := w.agentIssue(t, w.task(t, "", testUserID), map[string]any{"parent_issue_id": root})
	if got := followState(t, child, dm); got != "active" {
		t.Fatalf("child created by an issue run: follow = %q, want active", got)
	}
	if got := reportingRoot(t, child); got != root {
		t.Errorf("child reports to %s, want %s", got, root)
	}
}

func TestDMProgressDispatch_DuplicateCancelOffersFollowAndMovesNoFollow(t *testing.T) {
	e := newDispatchEnv(t)
	canonical := dbfx.Issue(t, "canonical "+t.Name())
	updateIssueField(t, e.issue, map[string]any{"status": "cancelled", "duplicate_of_issue_id": canonical})
	dbfx.Exec(t, `UPDATE dm_progress_pending SET created_at = $2 WHERE issue_id = $1`, e.issue, e.at(0))

	e.run(t, 15)
	got := e.wantCards(t, 1)
	var raw struct {
		Items []struct {
			IssueID       string `json:"issue_id"`
			Status        string `json:"status"`
			DuplicateOf   string `json:"duplicate_of"`
			FollowOffered bool   `json:"follow_offer"`
		} `json:"items"`
	}
	var payload []byte
	dbfx.QueryRow(t, `SELECT payload FROM dm_progress_delivery WHERE lineage_id = $1 AND seq = $2`, e.lane, got[0].Seq).Scan(&payload)
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Items) != 1 || raw.Items[0].Status != "cancelled" || raw.Items[0].DuplicateOf != canonical || !raw.Items[0].FollowOffered {
		t.Fatalf("card items = %+v, want one cancelled item offering a follow of %s", raw.Items, canonical)
	}
	if got := followState(t, e.issue, e.lane); got != "active" {
		t.Errorf("duplicate's follow = %q, want it kept active", got)
	}
	if got := followState(t, canonical, e.lane); got != "" {
		t.Errorf("canonical follow = %q, want none until the one click", got)
	}
}

func TestDMProgressDispatch_DuplicateOfflineOfferNeedsAnExistingUnfollowedOriginal(t *testing.T) {
	cases := map[string]func(t *testing.T, e *dispatchEnv, canonical string){
		"original deleted": func(t *testing.T, e *dispatchEnv, c string) { dbfx.Exec(t, `DELETE FROM issue WHERE id = $1`, c) },
		"original followed": func(t *testing.T, e *dispatchEnv, c string) {
			testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", c, "", e.lane)).Want(http.StatusOK)
			dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, c)
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			e := newDispatchEnv(t)
			canonical := dbfx.Issue(t, "canonical "+t.Name())
			updateIssueField(t, e.issue, map[string]any{"status": "cancelled", "duplicate_of_issue_id": canonical})
			dbfx.Exec(t, `UPDATE dm_progress_pending SET created_at = $2 WHERE issue_id = $1`, e.issue, e.at(0))
			prepare(t, e, canonical)
			e.run(t, 15)
			e.wantCards(t, 1)
			var offered bool
			dbfx.QueryRow(t, `SELECT COALESCE(bool_or((i->>'follow_offer')::boolean), false) FROM dm_progress_delivery d, jsonb_array_elements(d.payload->'items') i WHERE d.lineage_id = $1`, e.lane).Scan(&offered)
			if offered {
				t.Errorf("%s: card offers a follow, want none", name)
			}
		})
	}
}

func TestDMProgress_ReopenKeepsTheFollowActiveAndCapturesTheEvent(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "cancelled", 0)
	dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, e.issue)
	e.status(t, e.issue, "in_progress", 1)
	if got := followState(t, e.issue, e.lane); got != "active" {
		t.Fatalf("follow after reopen = %q, want active", got)
	}
	if n := e.pending(t); n != 1 {
		t.Errorf("pending rows after reopen = %d, want the reopen event", n)
	}
}

// epic world: root (origin follow) with n tracked children.
type epicWorld struct {
	dispatchEnvLane string
	root            string
	kids            []string
}

func newEpicWorld(t *testing.T, kids int) epicWorld {
	t.Helper()
	cw := newCapWorld(t)
	w := epicWorld{dispatchEnvLane: cw.lane, root: cw.issue}
	for i := 0; i < kids; i++ {
		w.kids = append(w.kids, numberedChild(t, cw.issue))
	}
	return w
}

func setStatus(t *testing.T, issue, status string) {
	t.Helper()
	if _, err := testHandler.Queries.UpdateIssueStatus(t.Context(), db.UpdateIssueStatusParams{ID: uuidOf(issue), Status: status, WorkspaceID: uuidOf(testWorkspaceID)}); err != nil {
		t.Fatal(err)
	}
}

func (w epicWorld) eval(t *testing.T) service.EpicState {
	t.Helper()
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	st, err := service.EvaluateEpicDone(t.Context(), testHandler.Queries.WithTx(tx), uuidOf(w.dispatchEnvLane), uuidOf(w.root))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return st
}

func (w epicWorld) revisionRow(t *testing.T) (revision int32, fired bool) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT revision, fired_at IS NOT NULL FROM dm_progress_epic_episode WHERE lineage_id = $1 AND root_issue_id = $2`, w.dispatchEnvLane, w.root).Scan(&revision, &fired)
	return
}

func TestDMProgressEpic_NotDoneWhileADescendantIsUnfinished(t *testing.T) {
	w := newEpicWorld(t, 2)
	setStatus(t, w.root, "done")
	setStatus(t, w.kids[0], "done")
	if st := w.eval(t); st.Done || st.Fire {
		t.Fatalf("state = %+v, want not done with one child unfinished", st)
	}
}

func TestDMProgressEpic_DoneFiresOnceAndReopenInvalidatesTheEpisode(t *testing.T) {
	w := newEpicWorld(t, 2)
	for _, k := range w.kids {
		setStatus(t, k, "done")
	}
	setStatus(t, w.root, "done")

	first := w.eval(t)
	if !first.Done || !first.Fire || first.Revision != 1 {
		t.Fatalf("first evaluation = %+v, want done, fired, revision 1", first)
	}
	if again := w.eval(t); !again.Done || again.Fire || again.Revision != 1 {
		t.Fatalf("second evaluation = %+v, want done, not fired again, revision 1", again)
	}

	// The reopen and the close both commit before anyone evaluates: the reopen
	// alone must end the episode.
	setStatus(t, w.root, "in_progress")
	setStatus(t, w.root, "done")
	if rev, fired := w.revisionRow(t); rev != 2 || fired {
		t.Fatalf("episode after reopen = revision %d fired %v, want revision 2 not fired", rev, fired)
	}
	if st := w.eval(t); !st.Done || !st.Fire || st.Revision != 2 {
		t.Fatalf("evaluation after the second close = %+v, want done, fired, revision 2", st)
	}
}

func TestDMProgressEpic_NewUnfinishedTrackedChildInvalidatesTheEpisode(t *testing.T) {
	w := newEpicWorld(t, 1)
	setStatus(t, w.kids[0], "done")
	setStatus(t, w.root, "done")
	if st := w.eval(t); !st.Fire {
		t.Fatalf("setup: %+v, want fired", st)
	}
	late := numberedChild(t, w.root)
	if rev, fired := w.revisionRow(t); rev != 2 || fired {
		t.Fatalf("episode after a new unfinished child = revision %d fired %v, want revision 2 not fired", rev, fired)
	}
	if st := w.eval(t); st.Done {
		t.Fatalf("state = %+v, want not done while the late child is unfinished", st)
	}
	setStatus(t, late, "done")
	if st := w.eval(t); !st.Done || !st.Fire || st.Revision != 2 {
		t.Fatalf("state = %+v, want done, fired, revision 2", st)
	}
}

func TestDMProgressEpic_CancelledDescendantsAreTerminalAndReportedApart(t *testing.T) {
	w := newEpicWorld(t, 3)
	setStatus(t, w.kids[0], "done")
	setStatus(t, w.kids[1], "cancelled")
	setStatus(t, w.kids[2], "cancelled")
	setStatus(t, w.root, "done")
	if st := w.eval(t); !st.Done || st.Cancelled != 2 {
		t.Fatalf("state = %+v, want done with 2 cancelled descendants reported apart", st)
	}
}

func TestDMProgressEpic_ReadsLifecycleCategoryNotStatusNames(t *testing.T) {
	w := newEpicWorld(t, 2)
	dbfx.Exec(t, `INSERT INTO issue_status (workspace_id, key, name, description, category, color, position) VALUES
		($1, 'epic_shipped', 'Epic shipped', '', 'done', '#00ff00', 90),
		($1, 'epic_review', 'Epic review', '', 'started', '#ff0000', 91)`, testWorkspaceID)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM issue_status WHERE workspace_id = $1 AND key IN ('epic_shipped', 'epic_review')`, testWorkspaceID)
	})
	setStatus(t, w.root, "done")
	setStatus(t, w.kids[0], "epic_shipped")
	setStatus(t, w.kids[1], "epic_review")
	if st := w.eval(t); st.Done {
		t.Fatalf("state = %+v, want not done: a started-category custom status is unfinished", st)
	}
	setStatus(t, w.kids[1], "epic_shipped")
	if st := w.eval(t); !st.Done {
		t.Fatalf("state = %+v, want done: a done-category custom status is terminal", st)
	}
}

func TestDMProgressEpic_CancelledRootIsNotDone(t *testing.T) {
	w := newEpicWorld(t, 1)
	setStatus(t, w.kids[0], "done")
	setStatus(t, w.root, "cancelled")
	if st := w.eval(t); st.Done {
		t.Fatalf("state = %+v, want a cancelled root never done", st)
	}
}

func TestDMProgressEpic_ChildEventsCountWhateverTheRootStatusAndAreNotImmediate(t *testing.T) {
	w := newEpicWorld(t, 1)
	setStatus(t, w.root, "cancelled")
	dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE lineage_id = $1`, w.dispatchEnvLane)
	setStatus(t, w.kids[0], "done")
	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_pending WHERE issue_id = $1 AND lineage_id = $2 AND NOT immediate`, w.kids[0], w.dispatchEnvLane); n != 1 {
		t.Fatalf("batched child events under a cancelled root = %d, want 1", n)
	}
	count, err := service.CountProgressDescendants(t.Context(), testHandler.Queries, uuidOf(w.dispatchEnvLane), uuidOf(w.root))
	if err != nil || count != 1 {
		t.Fatalf("tracked descendants = %d, %v; want 1", count, err)
	}
}

func TestDMProgressDispatch_EpicDoneRidesTheCardOnceAndReopenStartsANewEpisode(t *testing.T) {
	e := newDispatchEnv(t)
	k1, k2 := e.child(t), e.child(t)
	e.status(t, k1, "done", 0)
	e.status(t, k2, "cancelled", 1)
	e.status(t, e.issue, "done", 2)
	e.run(t, 20)
	got := e.wantCards(t, 1)
	if len(got[0].Payload.Epics) != 1 || got[0].Payload.Epics[0] != (protocol.ProgressCardEpic{IssueID: e.issue, Revision: 1, Cancelled: 1}) {
		t.Fatalf("epics on card 1 = %+v, want the root at revision 1 with 1 cancelled descendant", got[0].Payload.Epics)
	}

	// Nothing changed: the episode never fires twice.
	e.run(t, 40)
	e.wantCards(t, 1)

	// A reopen ends the episode and says so on the next card without an epic.
	e.status(t, e.issue, "in_progress", 50)
	e.run(t, 70)
	if got = e.wantCards(t, 2); len(got[1].Payload.Epics) != 0 {
		t.Fatalf("epics after the reopen = %+v, want none", got[1].Payload.Epics)
	}

	e.status(t, e.issue, "done", 80)
	e.run(t, 100)
	got = e.wantCards(t, 3)
	if len(got[2].Payload.Epics) != 1 || got[2].Payload.Epics[0].Revision != 2 {
		t.Fatalf("epics on card 3 = %+v, want the root at revision 2", got[2].Payload.Epics)
	}
}

func TestDMProgressDispatch_EpicDoneRidesTheLastChildEvenWhenTheRootAlreadyClosed(t *testing.T) {
	e := newDispatchEnv(t)
	k1 := e.child(t)
	e.status(t, e.issue, "done", 0)
	e.run(t, 20)
	if got := e.wantCards(t, 1); len(got[0].Payload.Epics) != 0 {
		t.Fatalf("epics with a child still unfinished = %+v, want none", got[0].Payload.Epics)
	}
	e.status(t, k1, "done", 30)
	e.run(t, 50)
	if got := e.wantCards(t, 2); len(got[1].Payload.Epics) != 1 || got[1].Payload.Epics[0].IssueID != e.issue {
		t.Fatalf("epics on card 2 = %+v, want the root once the last child closes", got[1].Payload.Epics)
	}
}

func TestDMProgressDispatch_ARootWithNoTrackedChildIsNotAnEpic(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "done", 0)
	e.run(t, 20)
	if got := e.wantCards(t, 1); len(got[0].Payload.Epics) != 0 {
		t.Fatalf("epics = %+v, want none for a root with no tracked descendant", got[0].Payload.Epics)
	}
}

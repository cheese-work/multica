package handler

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func reportingRoot(t *testing.T, issueID string) string {
	t.Helper()
	var root string
	dbfx.QueryRow(t, `SELECT reporting_root_id::text FROM dm_progress_follow WHERE issue_id = $1`, issueID).Scan(&root)
	return root
}

var numberedChildSeq atomic.Int64

func numberedChild(t *testing.T, parent string) string {
	t.Helper()
	return createDMIssue(t, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": fmt.Sprintf("child %d", numberedChildSeq.Add(1)), "parent_issue_id": parent}))
}

func TestDMProgress_ReportingRootIsTheTopRootAtAnyDepth(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	root := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	if got := reportingRoot(t, root); got != root {
		t.Fatalf("origin root reports to %s, want itself %s", got, root)
	}
	leaf := root
	for level := 1; level <= 4; level++ {
		leaf = numberedChild(t, leaf)
	}
	if got := reportingRoot(t, leaf); got != root {
		t.Fatalf("level-4 descendant reports to %s, want top root %s", got, root)
	}
}

func TestDMProgress_ParentCycleBecomesStandaloneRootAndRaisesIncident(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	a := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	b := numberedChild(t, a)
	c := numberedChild(t, b)
	// Corrupt the chain: a's parent is now c (a -> c -> b -> a).
	dbfx.Exec(t, `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, a, c)

	d := numberedChild(t, c) // must terminate
	if got := reportingRoot(t, d); got != d {
		t.Fatalf("cycle node reports to %s, want standalone root %s", got, d)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_data_incident WHERE issue_id = $1 AND kind = 'parent_cycle'`, d); n != 1 {
		t.Fatalf("%d parent_cycle incidents for the cycle node, want 1", n)
	}
}

func TestDMProgress_ReportingRootIsImmutable(t *testing.T) {
	w := newDMWorld(t)
	root := w.agentIssue(t, w.task(t, w.dm(t, testUserID), testUserID), nil)
	if _, err := testPool.Exec(t.Context(), `UPDATE dm_progress_follow SET reporting_root_id = gen_random_uuid() WHERE issue_id = $1`, root); err == nil || !strings.Contains(err.Error(), "reporting_root_id is immutable") {
		t.Fatalf("reporting_root_id update err = %v, want the immutable-column rejection", err)
	}
}

func TestDMProgress_DescendantCountIsCompleteAcrossPages(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	root := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	want := map[string]bool{}
	parent := root
	for i := 0; i < 150; i++ {
		id := numberedChild(t, parent)
		want[id] = true
		if i%3 == 0 { // mix wide and deep
			parent = id
		}
	}
	var lane, rootID, after pgtype.UUID
	_ = lane.Scan(dm)
	_ = rootID.Scan(root)
	q := db.New(testPool)

	n, err := service.CountProgressDescendants(t.Context(), q, lane, rootID)
	if err != nil || n != 150 {
		t.Fatalf("count = %d, %v; want 150", n, err)
	}
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		page, err := service.ListProgressDescendants(t.Context(), q, lane, rootID, after, 40)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if pages > 10 {
			t.Fatal("pagination does not terminate")
		}
		for _, id := range page {
			s := id.String()
			if seen[s] {
				t.Fatalf("descendant %s listed twice", s)
			}
			seen[s] = true
		}
		after = page[len(page)-1]
	}
	if len(seen) != 150 {
		t.Fatalf("paged %d descendants, want 150", len(seen))
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("descendant %s missing from pages", id)
		}
	}
}

func TestDMProgress_ChildOfReparentedIssueKeepsTheStoredRoot(t *testing.T) {
	w := newDMWorld(t)
	root := w.agentIssue(t, w.task(t, w.dm(t, testUserID), testUserID), nil)
	mid := numberedChild(t, root)
	// mid is detached from its parent issue; its stored root must not change.
	dbfx.Exec(t, `UPDATE issue SET parent_issue_id = NULL WHERE id = $1`, mid)

	leaf := numberedChild(t, mid)
	if got := reportingRoot(t, leaf); got != root {
		t.Fatalf("child of a detached issue reports to %s, want the stored root %s", got, root)
	}
}

// SEC-2: incident dedup is a unique index on (follow_id, kind), so the insert
// never scans the incident table (it ran under the workspace counter lock).
func TestDMProgress_IncidentFollowKindIsUniqueByIndex(t *testing.T) {
	const insert = `INSERT INTO dm_progress_data_incident (workspace_id, issue_id, follow_id, kind) VALUES ($1, gen_random_uuid(), $2, 'parent_cycle')`
	var follow string
	dbfx.QueryRow(t, `SELECT gen_random_uuid()::text`).Scan(&follow)
	dbfx.Cleanup(t, `DELETE FROM dm_progress_data_incident WHERE follow_id = $1`, follow)
	dbfx.Exec(t, insert, testWorkspaceID, follow)
	if _, err := testPool.Exec(t.Context(), insert, testWorkspaceID, follow); err == nil || !strings.Contains(err.Error(), "SQLSTATE 23505") {
		t.Fatalf("duplicate (follow_id, kind) err = %v, want a unique violation", err)
	}
}

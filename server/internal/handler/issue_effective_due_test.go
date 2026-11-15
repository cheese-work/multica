package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type dueView struct {
	Identifier   string  `json:"identifier"`
	EffectiveDue *string `json:"effective_due"`
	DueSource    string  `json:"due_source"`
	Diagnostics  []struct {
		Type    string   `json:"type"`
		Members []string `json:"members"`
	} `json:"diagnostics"`
}

func readDue(t *testing.T, id string) dueView {
	t.Helper()
	var v dueView
	req := withURLParam(newRequest("GET", "/api/issues/"+id, nil), "id", id)
	testutil.Call(t, testHandler.GetIssue, req).Want(http.StatusOK).JSON(&v)
	return v
}

func (v dueView) want(t *testing.T, date, source string) {
	t.Helper()
	if v.EffectiveDue == nil || *v.EffectiveDue != date || v.DueSource != source {
		got := "<none>"
		if v.EffectiveDue != nil {
			got = *v.EffectiveDue
		}
		t.Fatalf("effective_due=%s source=%q, want %s %q", got, v.DueSource, date, source)
	}
}

func (v dueView) wantNone(t *testing.T) {
	t.Helper()
	if v.EffectiveDue != nil || v.DueSource != "" {
		t.Fatalf("want no effective_due, got %v source=%q", *v.EffectiveDue, v.DueSource)
	}
}

func (v dueView) diag(typ string) []string {
	for _, d := range v.Diagnostics {
		if d.Type == typ {
			return append([]string{typ}, d.Members...)
		}
	}
	return nil
}

// watchDone makes watcher wait for watched in the given state.
func watchDone(t *testing.T, watcher, watched, state string) {
	t.Helper()
	agent := dbfx.Agent(t, "due watcher "+watcher[:8]+watched[:8], "")
	dbfx.Insert(t, "issue_wakeup", testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": watcher, "agent_id": agent, "created_by": testUserID, "instruction": "resume", "kind": "event", "mode": "once", "event_types": testutil.Raw(`ARRAY['issue.status_changed']`),
		"condition": fmt.Sprintf(`{"type":"other_issue","issue_id":%q,"state":%q}`, watched, state)})
}

func TestEffectiveDueViaParent(t *testing.T) {
	parent := dbfx.Issue(t, "due parent", testutil.Cols{"due_date": "2026-12-01"})
	child := dbfx.Issue(t, "due child", testutil.Cols{"parent_issue_id": parent})
	parentView := readDue(t, parent)
	readDue(t, child).want(t, "2026-12-01", "parent:"+parentView.Identifier)
	parentView.want(t, "2026-12-01", "own")

	earlier := dbfx.Issue(t, "due earlier child", testutil.Cols{"parent_issue_id": parent, "due_date": "2026-11-20"})
	readDue(t, earlier).want(t, "2026-11-20", "own")
	later := dbfx.Issue(t, "due later child", testutil.Cols{"parent_issue_id": parent, "due_date": "2027-01-10"})
	readDue(t, later).want(t, "2026-12-01", "parent:"+parentView.Identifier)
}

func TestEffectiveDueViaDonePrerequisiteWakeup(t *testing.T) {
	prereq := dbfx.Issue(t, "due prerequisite")
	waiter := dbfx.Issue(t, "due waiter", testutil.Cols{"due_date": "2026-11-30"})
	watchDone(t, waiter, prereq, "done")
	waiterView := readDue(t, waiter)
	readDue(t, prereq).want(t, "2026-11-30", "prereq_of:"+waiterView.Identifier)
}

func TestEffectiveDueIgnoresNonDoneWatchesAndDisabledWakeups(t *testing.T) {
	prereq := dbfx.Issue(t, "due non-done prerequisite")
	for _, state := range []string{"ended", "in_review", "blocked"} {
		waiter := dbfx.Issue(t, "due waiter "+state, testutil.Cols{"due_date": "2026-11-30"})
		watchDone(t, waiter, prereq, state)
	}
	readDue(t, prereq).wantNone(t)

	off := dbfx.Issue(t, "due disabled waiter", testutil.Cols{"due_date": "2026-11-30"})
	watchDone(t, off, prereq, "done")
	dbfx.Exec(t, `UPDATE issue_wakeup SET enabled=false, disabled_at=now() WHERE issue_id=$1`, off)
	readDue(t, prereq).wantNone(t)
}

func TestEffectiveDueExcludesClosedIssues(t *testing.T) {
	for _, status := range []string{"done", "cancelled"} {
		closedParent := dbfx.Issue(t, "due closed parent "+status, testutil.Cols{"due_date": "2026-12-01", "status": status})
		child := dbfx.Issue(t, "due child of closed "+status, testutil.Cols{"parent_issue_id": closedParent})
		readDue(t, child).wantNone(t)
		readDue(t, closedParent).wantNone(t)

		prereq := dbfx.Issue(t, "due prereq of closed waiter "+status)
		closedWaiter := dbfx.Issue(t, "due closed waiter "+status, testutil.Cols{"due_date": "2026-12-01", "status": status})
		watchDone(t, closedWaiter, prereq, "done")
		readDue(t, prereq).wantNone(t)
	}
	// A closed child with an open dated parent still has no projection.
	parent := dbfx.Issue(t, "due open parent", testutil.Cols{"due_date": "2026-12-01"})
	closedChild := dbfx.Issue(t, "due closed child", testutil.Cols{"parent_issue_id": parent, "status": "done"})
	readDue(t, closedChild).wantNone(t)
}

func TestEffectiveDueCycle(t *testing.T) {
	a := dbfx.Issue(t, "due cycle a", testutil.Cols{"due_date": "2026-12-01"})
	b := dbfx.Issue(t, "due cycle b")
	watchDone(t, a, b, "done")
	watchDone(t, b, a, "done")
	av, bv := readDue(t, a), readDue(t, b)
	av.want(t, "2026-12-01", "own")
	bv.want(t, "2026-12-01", "prereq_of:"+av.Identifier)
	for _, v := range []dueView{av, bv} {
		members := v.diag("cycle")
		if len(members) != 3 || !strings.Contains(strings.Join(members, ","), av.Identifier) || !strings.Contains(strings.Join(members, ","), bv.Identifier) {
			t.Fatalf("%s: cycle diagnostic %v, want both %s and %s", v.Identifier, members, av.Identifier, bv.Identifier)
		}
	}
}

func chainOfParents(t *testing.T, n int) []string {
	t.Helper()
	ids := make([]string, n) // ids[0] is the leaf, ids[n-1] the top
	for i := n - 1; i >= 0; i-- {
		cols := testutil.Cols{}
		if i < n-1 {
			cols["parent_issue_id"] = ids[i+1]
		} else {
			cols["due_date"] = "2026-12-01"
		}
		ids[i] = dbfx.Issue(t, fmt.Sprintf("due chain %d", i), cols)
	}
	return ids
}

func TestEffectiveDueDepthLimit(t *testing.T) {
	within := chainOfParents(t, 11) // top sits 10 hops above the leaf
	v := readDue(t, within[0])
	top := readDue(t, within[10]).Identifier
	if v.EffectiveDue == nil || *v.EffectiveDue != "2026-12-01" || !strings.HasPrefix(v.DueSource, "parent:"+top) {
		t.Fatalf("effective_due=%v source=%q, want 2026-12-01 parent:%s...", v.EffectiveDue, v.DueSource, top)
	}
	if v.diag("depth_limit") != nil {
		t.Fatalf("unexpected depth_limit: %+v", v.Diagnostics)
	}

	beyond := chainOfParents(t, 12) // top sits 11 hops above the leaf
	v = readDue(t, beyond[0])
	v.wantNone(t)
	if v.diag("depth_limit") == nil {
		t.Fatalf("want depth_limit diagnostic, got %+v", v.Diagnostics)
	}
}

// B and C both wait on A and on each other: the B<->C cycle is reachable from A
// only through cross edges, after both were already reached directly.
func TestEffectiveDueCycleBehindConvergingPaths(t *testing.T) {
	a := dbfx.Issue(t, "due converge a")
	b := dbfx.Issue(t, "due converge b", testutil.Cols{"due_date": "2026-12-01"})
	c := dbfx.Issue(t, "due converge c")
	watchDone(t, b, a, "done")
	watchDone(t, c, a, "done")
	watchDone(t, c, b, "done")
	watchDone(t, b, c, "done")
	av, bv, cv := readDue(t, a), readDue(t, b), readDue(t, c)
	av.want(t, "2026-12-01", "prereq_of:"+bv.Identifier)
	members := av.diag("cycle")
	if len(members) != 3 || !strings.Contains(strings.Join(members, ","), bv.Identifier) || !strings.Contains(strings.Join(members, ","), cv.Identifier) {
		t.Fatalf("cycle diagnostic %v, want %s and %s", members, bv.Identifier, cv.Identifier)
	}
}

// One parent, 900 children, every child waiting on the parent: 901 roots, each
// reaching all 901 nodes through one big cycle. The walk, the retained cycle
// members and the response must stay bounded.
func TestEffectiveDueGraphWorkIsBounded(t *testing.T) {
	parent := dbfx.Issue(t, "bound parent", testutil.Cols{"due_date": "2026-12-01"})
	agent := dbfx.Agent(t, "bound watcher", "")
	var child string
	for i := 0; i < 900; i++ {
		cols := testutil.Cols{"parent_issue_id": parent}
		if i == 899 {
			cols["due_date"] = "2026-11-01" // the earliest deadline sits among the nodes the walk may drop
		}
		child = dbfx.Issue(t, fmt.Sprintf("bound child %d", i), cols)
		dbfx.Insert(t, "issue_wakeup", testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": child, "agent_id": agent, "created_by": testUserID, "instruction": "resume", "kind": "event", "mode": "once", "event_types": testutil.Raw(`ARRAY['issue.status_changed']`),
			"condition": fmt.Sprintf(`{"type":"other_issue","issue_id":%q,"state":"done"}`, parent)})
	}
	for _, id := range []string{parent, child} {
		v := readDue(t, id)
		if v.diag("graph_limit") == nil {
			t.Fatalf("want graph_limit diagnostic, got %+v", v.Diagnostics)
		}
		for _, d := range v.Diagnostics {
			if len(d.Members) > 20 {
				t.Fatalf("cycle members must be capped at 20, got %d", len(d.Members))
			}
		}
		// A date from a partial walk can miss the true earliest deadline: publish none.
		v.wantNone(t)
	}
}

// A narrow root filter must not bound the related nodes loaded: past the node
// budget the list is refused (400), like the row cap.
func TestListSortEffectiveDueNodeBudget(t *testing.T) {
	old := effectiveDueMaxNodes
	effectiveDueMaxNodes = 10
	t.Cleanup(func() { effectiveDueMaxNodes = old })
	project := dbfx.Project(t, "sort effective due node budget")
	parent := dbfx.Issue(t, "budget parent", testutil.Cols{"project_id": project})
	for i := 0; i < 12; i++ {
		watchDone(t, dbfx.Issue(t, fmt.Sprintf("budget watcher %d", i)), parent, "done")
	}
	req := newRequest("GET", "/api/issues?sort=effective_due&limit=1&project_id="+project, nil)
	w := testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "narrow") {
		t.Fatalf("400 must tell the caller to narrow the filter, got %s", w.Body.String())
	}
}

// A root whose walk hit the visit limit has an unknown date, not "no date":
// sorting it as undated could bury urgent work behind later deadlines, so the
// list is refused (400), like the row cap and node budget.
func TestListSortEffectiveDueGraphLimit(t *testing.T) {
	project := dbfx.Project(t, "sort effective due graph limit")
	parent := dbfx.Issue(t, "limit parent", testutil.Cols{"project_id": project, "due_date": "2026-12-01"})
	agent := dbfx.Agent(t, "limit watcher", "")
	for i := 0; i < 900; i++ {
		child := dbfx.Issue(t, fmt.Sprintf("limit child %d", i), testutil.Cols{"project_id": project, "parent_issue_id": parent})
		dbfx.Insert(t, "issue_wakeup", testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": child, "agent_id": agent, "created_by": testUserID, "instruction": "resume", "kind": "event", "mode": "once", "event_types": testutil.Raw(`ARRAY['issue.status_changed']`),
			"condition": fmt.Sprintf(`{"type":"other_issue","issue_id":%q,"state":"done"}`, parent)})
	}
	dbfx.Issue(t, "limit competitor", testutil.Cols{"project_id": project, "due_date": "2026-12-05"})
	req := newRequest("GET", "/api/issues?sort=effective_due&limit=1&project_id="+project, nil)
	w := testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "narrow") {
		t.Fatalf("400 must tell the caller to narrow the filter, got %s", w.Body.String())
	}
}

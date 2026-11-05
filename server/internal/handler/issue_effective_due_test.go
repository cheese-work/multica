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

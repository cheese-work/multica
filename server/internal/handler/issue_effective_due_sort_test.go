package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDueInstantIsEndOfDayICT(t *testing.T) {
	ict := time.FixedZone("ICT", 7*3600)
	want := time.Date(2026, 11, 5, 23, 59, 59, 999999999, ict)
	got, ok := dueInstant("2026-11-05")
	if !ok || !got.Equal(want) {
		t.Fatalf("dueInstant = %v ok=%v, want %v", got, ok, want)
	}
	next, _ := dueInstant("2026-11-06")
	if !got.Before(next) {
		t.Fatalf("2026-11-05 must sort before 2026-11-06")
	}
	if _, ok := dueInstant("not-a-date"); ok {
		t.Fatal("a malformed date must not parse")
	}
}

func listSorted(t *testing.T, project string) []string {
	t.Helper()
	var resp struct {
		Issues []struct {
			Identifier   string  `json:"identifier"`
			EffectiveDue *string `json:"effective_due"`
			DueSource    string  `json:"due_source"`
		} `json:"issues"`
	}
	req := newRequest("GET", "/api/issues?sort=effective_due&limit=100&project_id="+project, nil)
	testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusOK).JSON(&resp)
	ids := make([]string, len(resp.Issues))
	for i, is := range resp.Issues {
		ids[i] = is.Identifier
	}
	return ids
}

// Dated issues come first by effective date (ties: own, parent, prereq_of,
// then lowest key); undated and closed issues follow by priority, then oldest.
func TestListSortEffectiveDue(t *testing.T) {
	project := dbfx.Project(t, "sort effective due")
	in := func(title string, cols testutil.Cols) string {
		cols["project_id"] = project
		return dbfx.Issue(t, title, cols)
	}
	later := in("later", testutil.Cols{"due_date": "2026-12-05"})
	early := in("early", testutil.Cols{"due_date": "2026-12-01"})
	child1 := in("child1", testutil.Cols{"parent_issue_id": early})
	child2 := in("child2", testutil.Cols{"parent_issue_id": early})
	prereq := in("prereq", testutil.Cols{})
	watchDone(t, early, prereq, "done")
	// Undated: urgent beats high beats none; closed issues never carry a date.
	oldestNone := in("closed", testutil.Cols{"status": "done", "due_date": "2026-11-01", "created_at": "2026-01-01T00:00:00Z"})
	none := in("none", testutil.Cols{"created_at": "2026-02-01T00:00:00Z"})
	high := in("high", testutil.Cols{"priority": "high", "created_at": "2026-03-01T00:00:00Z"})
	urgent := in("urgent", testutil.Cols{"priority": "urgent", "created_at": "2026-04-01T00:00:00Z"})

	want := []string{early, child1, child2, prereq, later, urgent, high, oldestNone, none}
	idOf := map[string]string{}
	for _, id := range want {
		idOf[id] = readDue(t, id).Identifier
	}
	got := listSorted(t, project)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %d issues", got, len(want))
	}
	for i, id := range want {
		if got[i] != idOf[id] {
			t.Fatalf("position %d: got %v, want order %v", i, got, func() []string {
				o := make([]string, len(want))
				for j, w := range want {
					o[j] = idOf[w]
				}
				return o
			}())
		}
	}
}

// An offset near MaxInt64 must give an empty page, not an overflow panic.
func TestListSortEffectiveDueHugeOffset(t *testing.T) {
	project := dbfx.Project(t, "sort effective due offset")
	dbfx.Issue(t, "only", testutil.Cols{"project_id": project})
	var resp struct {
		Issues []struct{} `json:"issues"`
		Total  int        `json:"total"`
	}
	req := newRequest("GET", "/api/issues?sort=effective_due&offset=9223372036854775807&project_id="+project, nil)
	testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusOK).JSON(&resp)
	if len(resp.Issues) != 0 || resp.Total != 1 {
		t.Fatalf("want empty page with total 1, got %d issues, total %d", len(resp.Issues), resp.Total)
	}
}

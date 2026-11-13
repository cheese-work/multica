package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// A candidate set above the cap is refused (400), never silently truncated:
// a truncated sort would return a wrong order. Exactly the cap still sorts.
func TestListSortEffectiveDueCap(t *testing.T) {
	old := effectiveDueMaxCandidates
	effectiveDueMaxCandidates = 3
	t.Cleanup(func() { effectiveDueMaxCandidates = old })
	project := dbfx.Project(t, "sort effective due cap")
	for i := 0; i < 3; i++ {
		dbfx.Issue(t, fmt.Sprintf("cap %d", i), testutil.Cols{"project_id": project})
	}
	if got := listSorted(t, project); len(got) != 3 {
		t.Fatalf("exactly cap issues must sort, got %v", got)
	}
	dbfx.Issue(t, "cap over", testutil.Cols{"project_id": project})
	req := newRequest("GET", "/api/issues?sort=effective_due&limit=1&project_id="+project, nil)
	w := testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "narrow") {
		t.Fatalf("400 must tell the caller to narrow the filter, got %s", w.Body.String())
	}
}

type issueQueryRecorder struct {
	dbExecutor
	queries []string
}

func (r *issueQueryRecorder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.queries = append(r.queries, sql)
	return r.dbExecutor.Query(ctx, sql, args...)
}

// The candidate set (up to effectiveDueMaxCandidates+1 rows) is read narrow:
// no title, description, metadata or properties. Those load only for the returned
// page, so a one-row sort cannot materialize every candidate's large fields.
func TestListSortEffectiveDueCandidatesAreNarrow(t *testing.T) {
	project := dbfx.Project(t, "sort effective due narrow")
	for i, due := range []string{"2026-12-03", "2026-12-01", "2026-12-02"} {
		dbfx.Issue(t, fmt.Sprintf("narrow %d", i), testutil.Cols{"project_id": project, "due_date": due, "description": fmt.Sprintf("body %d", i)})
	}
	rec := &issueQueryRecorder{dbExecutor: testHandler.DB}
	h := *testHandler
	h.DB = rec
	var resp struct {
		Issues []struct {
			Title       string  `json:"title"`
			Description *string `json:"description"`
		} `json:"issues"`
	}
	req := newRequest("GET", "/api/issues?sort=effective_due&limit=1&offset=1&project_id="+project, nil)
	testutil.Call(t, h.ListIssues, req).Want(http.StatusOK).JSON(&resp)
	if len(resp.Issues) != 1 || resp.Issues[0].Title != "narrow 2" || resp.Issues[0].Description == nil || *resp.Issues[0].Description != "body 2" {
		t.Fatalf("page must be the second-earliest issue with its description, got %+v", resp.Issues)
	}
	for _, q := range rec.queries {
		if strings.Contains(q, "ORDER BY") && strings.Contains(q, "FROM issue i") {
			// the external-read flag is a boolean the read filter needs, not a large column
			narrowed := strings.ReplaceAll(q, "i.description_external_read", "")
			for _, col := range []string{"i.title", "i.description", "i.metadata", "i.properties"} {
				if strings.Contains(narrowed, col) {
					t.Fatalf("candidate query must not read %s: %s", col, q)
				}
			}
		}
	}
}

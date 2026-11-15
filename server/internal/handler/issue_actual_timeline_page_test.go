package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4343 (S1b of CHE-4312): timeline scope, filters and keyset paging.

type tlPage struct {
	AsOf       time.Time  `json:"as_of"`
	Issues     []tlResult `json:"issues"`
	NextCursor *string    `json:"next_cursor"`
}

func postTL(t *testing.T, body map[string]any, mutate ...func(*http.Request)) (int, tlPage) {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/issues/timeline", body)
	for _, m := range mutate {
		m(req)
	}
	w := httptest.NewRecorder()
	testHandler.ListIssueActualTimeline(w, req)
	var page tlPage
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	return w.Code, page
}

func pageBody(scope, filters map[string]any, limit int, cursor *string) map[string]any {
	body := map[string]any{"scope": scope, "page": map[string]any{"limit": limit}}
	if filters != nil {
		body["filters"] = filters
	}
	if cursor != nil {
		body["page"] = map[string]any{"limit": limit, "cursor": *cursor}
	}
	return body
}

// allPages follows next_cursor and fails on a repeated issue.
func allPages(t *testing.T, scope, filters map[string]any, limit int) []string {
	t.Helper()
	var ids []string
	var cursor *string
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("cursor chain does not end")
		}
		code, page := postTL(t, pageBody(scope, filters, limit, cursor))
		if code != http.StatusOK {
			t.Fatalf("page %d: %d", pages, code)
		}
		for _, issue := range page.Issues {
			if slices.Contains(ids, issue.IssueID) {
				t.Fatalf("issue %s repeated on page %d", issue.IssueID, pages)
			}
			ids = append(ids, issue.IssueID)
		}
		if cursor = page.NextCursor; cursor == nil {
			return ids
		}
	}
}

func sameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Fatalf("%s: got %d issues %v, want %d %v", label, len(g), g, len(w), w)
	}
}

// pagedFixture is a project of 260 started issues: 255 fillers created first
// by another member, then the matching issues created after them, so every
// match sorts after the first 200-issue page.
type pagedFixture struct {
	project, other string
	matches        []string
}

func seedPaged(t *testing.T, fillerCols, matchCols testutil.Cols, matches int) pagedFixture {
	t.Helper()
	f := pagedFixture{other: dbfx.User(t, "paged other", fmt.Sprintf("paged-%d@multica.test", time.Now().UnixNano()))}
	dbfx.Member(t, testWorkspaceID, f.other, "member")
	f.project = dbfx.Project(t, "paged timeline")
	base := time.Now().Add(-10 * day)
	for i := 0; i < 260-matches; i++ {
		dbfx.Issue(t, "filler", testutil.Cols{
			"project_id": f.project, "status": "in_progress", "creator_id": f.other,
			"created_at": base.Add(time.Duration(i) * time.Second),
		}, fillerCols)
	}
	for i := 0; i < matches; i++ {
		f.matches = append(f.matches, dbfx.Issue(t, "match", testutil.Cols{
			"project_id": f.project, "status": "in_progress", "creator_id": f.other,
			"created_at": base.Add(time.Hour + time.Duration(i)*time.Second),
		}, matchCols))
	}
	return f
}

func inProject(f pagedFixture) map[string]any {
	return map[string]any{"project_ids": []string{f.project}}
}

func TestTimelineM1MyAssigned(t *testing.T) {
	f := seedPaged(t, nil, testutil.Cols{"assignee_type": "member", "assignee_id": testUserID}, 5)
	got := allPages(t, map[string]any{"kind": "my", "relation": "assigned"}, inProject(f), 2)
	sameSet(t, "my/assigned", got, f.matches)
}

func TestTimelineM2MyCreated(t *testing.T) {
	f := seedPaged(t, nil, testutil.Cols{"creator_id": testUserID}, 5)
	got := allPages(t, map[string]any{"kind": "my", "relation": "created"}, inProject(f), 2)
	sameSet(t, "my/created", got, f.matches)
}

func TestTimelineM3MyInvolved(t *testing.T) {
	agent := dbfx.Agent(t, "paged involved agent", "") // owned by the test user
	f := seedPaged(t, nil, testutil.Cols{"assignee_type": "agent", "assignee_id": agent}, 5)
	got := allPages(t, map[string]any{"kind": "my", "relation": "involved"}, inProject(f), 2)
	sameSet(t, "my/involved", got, f.matches)
}

func TestTimelineM4MyAnyReturnsOnce(t *testing.T) {
	f := seedPaged(t, nil, testutil.Cols{"assignee_type": "member", "assignee_id": testUserID, "creator_id": testUserID}, 5)
	got := allPages(t, map[string]any{"kind": "my", "relation": "any"}, inProject(f), 2)
	sameSet(t, "my/any", got, f.matches)
}

func TestTimelineM5StatusFilterAfterFirstPage(t *testing.T) {
	f := seedPaged(t, nil, testutil.Cols{"status": "in_review"}, 5)
	scope := map[string]any{"kind": "project", "project_id": f.project}
	got := allPages(t, scope, map[string]any{"statuses": []string{"in_review"}}, 2)
	sameSet(t, "status filter", got, f.matches)
	if all := allPages(t, scope, nil, 200); len(all) != 260 {
		t.Fatalf("unfiltered project: got %d, want 260", len(all))
	}
}

func TestTimelineM6CursorReusedWithOtherQueryIs400(t *testing.T) {
	f := seedPaged(t, nil, testutil.Cols{"assignee_type": "member", "assignee_id": testUserID}, 5)
	scope := map[string]any{"kind": "my", "relation": "assigned"}
	_, first := postTL(t, pageBody(scope, inProject(f), 2, nil))
	if first.NextCursor == nil {
		t.Fatal("first page has no cursor")
	}
	for name, body := range map[string]map[string]any{
		"other relation": pageBody(map[string]any{"kind": "my", "relation": "created"}, inProject(f), 2, first.NextCursor),
		"other filter":   pageBody(scope, map[string]any{"project_ids": []string{f.project}, "statuses": []string{"todo"}}, 2, first.NextCursor),
		"other scope":    pageBody(map[string]any{"kind": "workspace"}, inProject(f), 2, first.NextCursor),
	} {
		if code, _ := postTL(t, body); code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", name, code)
		}
	}
	if code, _ := postTL(t, pageBody(scope, inProject(f), 2, first.NextCursor)); code != http.StatusOK {
		t.Fatalf("same query: got %d, want 200", code)
	}
	win := map[string]any{"from": "2020-01-01T00:00:00Z", "to": "2020-02-01T00:00:00Z"}
	body := pageBody(scope, inProject(f), 2, first.NextCursor)
	body["window"] = win
	if code, _ := postTL(t, body); code != http.StatusBadRequest {
		t.Fatalf("other window: got %d, want 400", code)
	}
}

// T8: no issue lost or repeated across pages, a boundary inside equal sort_at
// values, and a status change after page 1 does not move an issue.
func TestTimelineT8PagesAreStable(t *testing.T) {
	project := dbfx.Project(t, "t8")
	base := time.Now().Add(-10 * day).Truncate(time.Microsecond)
	var ids []string
	for i := 0; i < 5; i++ { // five issues share one sort_at
		ids = append(ids, dbfx.Issue(t, "tie", testutil.Cols{"project_id": project, "status": "in_progress", "created_at": base}))
	}
	for i := 1; i <= 2; i++ {
		ids = append(ids, dbfx.Issue(t, "later", testutil.Cols{"project_id": project, "status": "in_progress", "created_at": base.Add(time.Duration(i) * time.Hour)}))
	}
	scope := map[string]any{"kind": "project", "project_id": project}

	code, first := postTL(t, pageBody(scope, nil, 2, nil))
	if code != http.StatusOK || len(first.Issues) != 2 || first.NextCursor == nil {
		t.Fatalf("page 1: %d, %d issues", code, len(first.Issues))
	}
	// A page-1 issue changes status after as_of: its first row would jump its
	// sort_at past the cursor and repeat it, unless rows after as_of are ignored.
	moved := first.Issues[0].IssueID
	dbfx.Exec(t, `UPDATE issue SET status = 'done' WHERE id = $1`, moved)
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details, created_at)
		VALUES ($1, $2, 'system', 'status_changed', '{"from":"in_progress","to":"done"}', $3)`, testWorkspaceID, moved, first.AsOf.Add(time.Second))

	seen := []string{first.Issues[0].IssueID, first.Issues[1].IssueID}
	cursor := first.NextCursor
	for cursor != nil {
		code, page := postTL(t, pageBody(scope, nil, 2, cursor))
		if code != http.StatusOK {
			t.Fatalf("next page: %d", code)
		}
		if !page.AsOf.Equal(first.AsOf) {
			t.Fatalf("as_of moved: %s -> %s", first.AsOf, page.AsOf)
		}
		for _, issue := range page.Issues {
			if slices.Contains(seen, issue.IssueID) {
				t.Fatalf("issue %s repeated", issue.IssueID)
			}
			seen = append(seen, issue.IssueID)
		}
		cursor = page.NextCursor
	}
	sameSet(t, "all pages", seen, ids)
}

// T8 with a status filter: the filter reads the as-of status, not the live one.
// An unseen issue that leaves the filtered status after page 1 still appears;
// one that enters it after page 1 still does not.
func TestTimelineT8StatusFilterUsesAsOfStatus(t *testing.T) {
	project := dbfx.Project(t, "t8 status filter")
	base := time.Now().Add(-10 * day).Truncate(time.Microsecond)
	var ids []string
	for i := 0; i < 6; i++ {
		status := "in_progress"
		if i == 5 {
			status = "todo"
		}
		ids = append(ids, dbfx.Issue(t, "t8s", testutil.Cols{"project_id": project, "status": status, "created_at": base.Add(time.Duration(i) * time.Second)}))
	}
	scope := map[string]any{"kind": "project", "project_id": project}
	filters := map[string]any{"statuses": []string{"in_progress"}}

	code, first := postTL(t, pageBody(scope, filters, 2, nil))
	if code != http.StatusOK || first.NextCursor == nil {
		t.Fatalf("page 1: %d", code)
	}
	change := func(id, from, to string) {
		dbfx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, id, to)
		dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details, created_at)
			VALUES ($1, $2, 'system', 'status_changed', jsonb_build_object('from', $3::text, 'to', $4::text), $5)`,
			testWorkspaceID, id, from, to, first.AsOf.Add(time.Second))
	}
	change(ids[4], "in_progress", "done") // leaves the filter after page 1
	change(ids[5], "todo", "in_progress") // enters the filter after page 1

	got := []string{first.Issues[0].IssueID, first.Issues[1].IssueID}
	for cursor := first.NextCursor; cursor != nil; {
		code, page := postTL(t, pageBody(scope, filters, 2, cursor))
		if code != http.StatusOK {
			t.Fatalf("next page: %d", code)
		}
		for _, issue := range page.Issues {
			got = append(got, issue.IssueID)
		}
		cursor = page.NextCursor
	}
	sameSet(t, "as-of status filter", got, ids[:5])
}

func TestTimelineN1ForeignProjectScopeIs404(t *testing.T) {
	foreign := dbfx.Workspace(t, "n1 foreign", fmt.Sprintf("n1-%d", time.Now().UnixNano()))
	project := dbfx.Project(t, "foreign project", testutil.Cols{"workspace_id": foreign})
	code, _ := postTL(t, pageBody(map[string]any{"kind": "project", "project_id": project}, nil, 10, nil))
	if code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", code)
	}
}

// N2: the endpoint takes no root issue, so a foreign-workspace root cannot be
// named. What N2 protects is that a foreign-workspace issue never leaks into a
// result, whatever the scope.
func TestTimelineN2ForeignIssueNeverReturned(t *testing.T) {
	foreign := dbfx.Workspace(t, "n2 foreign", fmt.Sprintf("n2-%d", time.Now().UnixNano()))
	foreignIssue := dbfx.Issue(t, "n2 foreign issue", testutil.Cols{"workspace_id": foreign, "status": "in_progress", "created_at": time.Now().Add(-day)})
	ownIssue := dbfx.Issue(t, "n2 own issue", testutil.Cols{"status": "in_progress", "created_at": time.Now().Add(-day)})
	got := allPages(t, map[string]any{"kind": "workspace"}, nil, 500)
	if slices.Contains(got, foreignIssue) {
		t.Fatal("foreign-workspace issue returned")
	}
	if !slices.Contains(got, ownIssue) {
		t.Fatal("own issue missing")
	}
}

// The access boundary is the existing one (workspaceMember): a non-member gets
// the same 404 as the table endpoints, which hides that the workspace exists.
func TestTimelineN3NonMemberIsDenied(t *testing.T) {
	stranger := dbfx.User(t, "n3 stranger", fmt.Sprintf("n3-%d@multica.test", time.Now().UnixNano()))
	code, _ := postTL(t, pageBody(map[string]any{"kind": "workspace"}, nil, 10, nil), func(r *http.Request) { r.Header.Set("X-User-ID", stranger) })
	if code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", code)
	}
}

// N4: an issue moved to another workspace between pages is absent from the next page.
func TestTimelineN4MovedIssueLeavesChain(t *testing.T) {
	foreign := dbfx.Workspace(t, "n4 foreign", fmt.Sprintf("n4-%d", time.Now().UnixNano()))
	project := dbfx.Project(t, "n4")
	base := time.Now().Add(-10 * day)
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, dbfx.Issue(t, "n4", testutil.Cols{"project_id": project, "status": "in_progress", "created_at": base.Add(time.Duration(i) * time.Second)}))
	}
	scope := map[string]any{"kind": "project", "project_id": project}
	_, first := postTL(t, pageBody(scope, nil, 2, nil))
	if first.NextCursor == nil {
		t.Fatal("no cursor")
	}
	dbfx.Exec(t, `UPDATE issue SET workspace_id = $2, project_id = NULL WHERE id = $1`, ids[3], foreign)
	_, second := postTL(t, pageBody(scope, nil, 2, first.NextCursor))
	for _, issue := range second.Issues {
		if issue.IssueID == ids[3] {
			t.Fatal("moved issue still on the next page")
		}
	}
	if len(second.Issues) != 1 || second.NextCursor != nil {
		t.Fatalf("second page: %d issues, cursor %v", len(second.Issues), second.NextCursor)
	}
}

func TestTimelineRejectsBadScopeAndPage(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"assignee scope": {"scope": map[string]any{"kind": "assignee", "actor": map[string]any{"type": "member", "id": testUserID}}},
		"team scope":     {"scope": map[string]any{"kind": "team"}},
		"limit too high": {"page": map[string]any{"limit": 501}},
		"limit negative": {"page": map[string]any{"limit": -1}},
		"bad cursor":     {"page": map[string]any{"cursor": "nope"}},
		"half window":    {"window": map[string]any{"from": "2020-01-01T00:00:00Z"}},
	} {
		if code, _ := postTL(t, body); code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", name, code)
		}
	}
}

func TestTimelineDefaultWindowIsLast30Days(t *testing.T) {
	recent := seedTLIssue(t, tlIssue{status: "in_progress", created: 5 * day})
	old := seedTLIssue(t, tlIssue{status: "backlog", created: 200 * day, rows: []tlRow{{ago: 100 * day, from: "backlog", to: "todo"}}})
	got := map[string]bool{}
	for _, id := range allPages(t, map[string]any{"kind": "workspace"}, nil, 500) {
		got[id] = true
	}
	if !got[recent] || got[old] {
		t.Fatalf("recent present %v, old present %v", got[recent], got[old])
	}
}

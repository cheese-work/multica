package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// createdViaFixture is one issue per classifier value, plus an issue whose
// created_via is NULL (made before CHE-6098, or by an integration channel).
// Every issue lives in one project so the project scope isolates the fixture
// from the rest of the workspace.
type createdViaFixture struct {
	project string
	byValue map[string]string // created_via value ("unknown" = NULL) -> issue id
}

func newCreatedViaFixture(t *testing.T) createdViaFixture {
	t.Helper()
	suffix := time.Now().UnixNano()
	fixture := createdViaFixture{
		project: dbfx.Project(t, fmt.Sprintf("created via %d", suffix)),
		byValue: map[string]string{},
	}
	for _, value := range []string{"dm", "comment", "agent", "form", "unknown"} {
		var createdVia any
		if value != "unknown" {
			createdVia = value
		}
		fixture.byValue[value] = dbfx.Issue(t, fmt.Sprintf("created via %s %d", value, suffix), testutil.Cols{
			"project_id": fixture.project, "created_via": createdVia,
		})
	}
	return fixture
}

func (f createdViaFixture) ids(values ...string) []string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, f.byValue[value])
	}
	sort.Strings(ids)
	return ids
}

// query is the table query for the fixture's project. Filters are raw JSON
// so this file states the wire contract, whatever the Go field is called.
func (f createdViaFixture) query(filters map[string]any) map[string]any {
	return map[string]any{
		"scope":   map[string]any{"kind": "project", "project_id": f.project},
		"filters": filters,
		"sort":    map[string]any{"field": "title", "direction": "asc"},
	}
}

func (f createdViaFixture) rowsResponse(t *testing.T, filters map[string]any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.ListIssueTableRows,
		newRequest(http.MethodPost, "/api/issues/table/rows", map[string]any{
			"query": f.query(filters),
			"group": map[string]any{"kind": "none"},
			"page":  map[string]any{"limit": 100},
		}))
}

func (f createdViaFixture) rows(t *testing.T, filters map[string]any) []string {
	t.Helper()
	var response issueTableRowsResponse
	f.rowsResponse(t, filters).Want(http.StatusOK).JSON(&response)
	ids := make([]string, 0, len(response.Rows))
	for _, row := range response.Rows {
		ids = append(ids, row.Issue.ID)
	}
	sort.Strings(ids)
	return ids
}

func (f createdViaFixture) assertRows(t *testing.T, name string, filters map[string]any, want ...string) {
	t.Helper()
	if got := f.rows(t, filters); !reflect.DeepEqual(got, f.ids(want...)) {
		t.Fatalf("%s: ids = %v, want %v", name, got, f.ids(want...))
	}
}

func TestIssueTableRowsFilterByCreatedVia(t *testing.T) {
	f := newCreatedViaFixture(t)
	filter := func(values ...string) map[string]any {
		return map[string]any{"created_via": values}
	}
	negated := func(values ...string) map[string]any {
		return map[string]any{"created_via": values, "negate": []string{"created_via"}}
	}

	f.assertRows(t, "no filter", map[string]any{}, "dm", "comment", "agent", "form", "unknown")
	f.assertRows(t, "one value", filter("dm"), "dm")
	f.assertRows(t, "values OR together", filter("dm", "comment"), "dm", "comment")
	f.assertRows(t, "unknown is the NULL rows", filter("unknown"), "unknown")
	f.assertRows(t, "unknown ORs with a value", filter("dm", "unknown"), "dm", "unknown")
	// "Is not" keeps what "is" drops, including NULL rows.
	f.assertRows(t, "is not dm keeps unknown", negated("dm"), "comment", "agent", "form", "unknown")
	f.assertRows(t, "is not dm or comment", negated("dm", "comment"), "agent", "form", "unknown")
	f.assertRows(t, "is not unknown", negated("unknown"), "dm", "comment", "agent", "form")
	f.assertRows(t, "negate without values changes nothing",
		map[string]any{"negate": []string{"created_via"}},
		"dm", "comment", "agent", "form", "unknown")
	f.assertRows(t, "values are deduplicated", filter("dm", "dm"), "dm")

	f.rowsResponse(t, filter("bogus")).Want(http.StatusBadRequest)
	f.rowsResponse(t, filter("")).Want(http.StatusBadRequest)
}

func TestIssueTableFacetsCountCreatedVia(t *testing.T) {
	f := newCreatedViaFixture(t)
	counts := func(filters map[string]any) map[string]int64 {
		t.Helper()
		w := httptest.NewRecorder()
		testHandler.ListIssueTableFacets(w, newRequest(http.MethodPost, "/api/issues/table/facets", map[string]any{
			"query":         f.query(filters),
			"facets":        []map[string]any{{"kind": "created_via"}},
			"include_total": false,
		}))
		if w.Code != http.StatusOK {
			t.Fatalf("ListIssueTableFacets: %d %s", w.Code, w.Body.String())
		}
		var response issueTableFacetsResponse
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, facet := range response.Facets {
			if facet.Kind != "created_via" {
				t.Fatalf("facet kind = %q", facet.Kind)
			}
			for _, value := range facet.Values {
				got[value.Key] = value.Count
			}
		}
		return got
	}

	all := map[string]int64{"dm": 1, "comment": 1, "agent": 1, "form": 1, "unknown": 1}
	if got := counts(map[string]any{}); !reflect.DeepEqual(got, all) {
		t.Fatalf("counts = %v, want %v (NULL counts as unknown)", got, all)
	}
	// Disjunctive: the facet ignores its own dimension, so picking a value
	// keeps the other options on screen with their counts.
	if got := counts(map[string]any{"created_via": []string{"dm"}}); !reflect.DeepEqual(got, all) {
		t.Fatalf("counts under own filter = %v, want %v", got, all)
	}
	if got := counts(map[string]any{"created_via": []string{"dm"}, "negate": []string{"created_via"}}); !reflect.DeepEqual(got, all) {
		t.Fatalf("counts under own negated filter = %v, want %v", got, all)
	}
	// Other dimensions still narrow it.
	other := counts(map[string]any{"priorities": []string{"urgent"}})
	if len(other) != 0 {
		t.Fatalf("counts under a priority nobody has = %v, want none", other)
	}
}

func TestIssueTableRowsAdvancedFilterByCreatedVia(t *testing.T) {
	f := newCreatedViaFixture(t)
	advanced := func(negate bool, values ...string) map[string]any {
		return map[string]any{"advanced": map[string]any{
			"match": "all",
			"children": []map[string]any{
				{"field": "created_via", "negate": negate, "values": values},
			},
		}}
	}
	f.assertRows(t, "advanced is", advanced(false, "dm", "comment"), "dm", "comment")
	f.assertRows(t, "advanced unknown", advanced(false, "unknown"), "unknown")
	f.assertRows(t, "advanced is not", advanced(true, "dm"), "comment", "agent", "form", "unknown")
	f.rowsResponse(t, advanced(false, "bogus")).Want(http.StatusBadRequest)
}

// The fingerprint ties a cursor to its query: a different created_via set
// must not share one, and the order of the values must not matter.
func TestIssueTableFingerprintCoversCreatedVia(t *testing.T) {
	fingerprint := func(createdVia string) string {
		t.Helper()
		var spec issueTableQuerySpec
		body := `{"scope":{"kind":"workspace"},"filters":{"created_via":` + createdVia + `},"sort":{"field":"title","direction":"asc"}}`
		if err := json.Unmarshal([]byte(body), &spec); err != nil {
			t.Fatal(err)
		}
		value, err := canonicalIssueTableFingerprint(testWorkspaceID, spec)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if fingerprint(`["dm"]`) == fingerprint(`["comment"]`) {
		t.Fatal("different created_via values share a fingerprint")
	}
	if fingerprint(`["dm","comment"]`) != fingerprint(`["comment","dm","dm"]`) {
		t.Fatal("created_via order or duplicates change the fingerprint")
	}
}

// A saved view with createdViaFilters lists what the same table query lists.
func TestIssueViewListsCreatedViaLikeTheTableQuery(t *testing.T) {
	f := newCreatedViaFixture(t)
	for _, tc := range []struct {
		name    string
		query   map[string]any
		filters map[string]any
		want    []string
	}{
		{
			"is",
			map[string]any{"createdViaFilters": []string{"dm", "comment"}},
			map[string]any{"created_via": []string{"dm", "comment"}},
			[]string{"dm", "comment"},
		},
		{
			"is not",
			map[string]any{"createdViaFilters": []string{"dm"}, "negatedFilters": []string{"createdVia"}},
			map[string]any{"created_via": []string{"dm"}, "negate": []string{"created_via"}},
			[]string{"comment", "agent", "form", "unknown"},
		},
		{
			"unknown",
			map[string]any{"createdViaFilters": []string{"unknown"}},
			map[string]any{"created_via": []string{"unknown"}},
			[]string{"unknown"},
		},
		{
			"an unknown value the client cannot show is dropped",
			map[string]any{"createdViaFilters": []string{"bogus"}},
			map[string]any{},
			[]string{"dm", "comment", "agent", "form", "unknown"},
		},
	} {
		view, code, body := createIssueViewForTest(t, map[string]any{
			"name":       "created via " + tc.name,
			"scope_type": "project",
			"scope_id":   f.project,
			"visibility": "workspace",
			"query":      tc.query,
			"display":    map[string]any{"sortBy": "title", "sortDirection": "asc"},
		})
		if code != http.StatusCreated {
			t.Fatalf("%s: create view: %d %s", tc.name, code, body)
		}
		var viewIssues issueViewIssuesResponse
		listIssueViewIssuesForTest(t, testUserID, view.ID, "").Want(http.StatusOK).JSON(&viewIssues)
		got := make([]string, 0, len(viewIssues.Issues))
		for _, issue := range viewIssues.Issues {
			got = append(got, issue.ID)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, f.ids(tc.want...)) {
			t.Fatalf("%s: view lists %v, want %v", tc.name, got, f.ids(tc.want...))
		}
		if table := f.rows(t, tc.filters); !reflect.DeepEqual(got, table) {
			t.Fatalf("%s: view lists %v but the table query lists %v", tc.name, got, table)
		}
	}
}

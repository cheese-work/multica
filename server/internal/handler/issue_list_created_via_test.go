package handler

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6392 (S3a of CHE-6376): list rows return created_via, and the list
// endpoints filter by it.
type createdViaListFixture struct {
	dialog, dm, comment, agent, unknown string
}

func seedCreatedViaListFixture(t *testing.T) createdViaListFixture {
	t.Helper()
	mk := func(title, via string) string {
		cols := testutil.Cols{}
		if via != "" {
			cols["created_via"] = via
		}
		return dbfx.Issue(t, "created_via list "+title, cols)
	}
	return createdViaListFixture{
		dialog:  mk("dialog", "dialog"),
		dm:      mk("dm", "dm"),
		comment: mk("comment", "comment"),
		agent:   mk("agent", "agent"),
		unknown: mk("unknown", ""), // NULL
	}
}

func listIssuesCreatedVia(t *testing.T, query string) []IssueResponse {
	t.Helper()
	req := newRequest(http.MethodGet, "/api/issues?workspace_id="+testWorkspaceID+"&limit=100"+query, nil)
	var resp struct {
		Issues []IssueResponse `json:"issues"`
	}
	testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusOK).JSON(&resp)
	return resp.Issues
}

func createdViaIssueIDs(issues []IssueResponse) []string {
	ids := make([]string, 0, len(issues))
	for _, i := range issues {
		ids = append(ids, i.ID)
	}
	return ids
}

func TestListIssues_ReturnsCreatedVia(t *testing.T) {
	f := seedCreatedViaListFixture(t)

	for _, openOnly := range []string{"", "&open_only=true"} {
		got := map[string]string{}
		for _, i := range listIssuesCreatedVia(t, openOnly) {
			got[i.ID] = i.CreatedVia
		}
		if got[f.dialog] != "dialog" {
			t.Errorf("list%s: dialog issue created_via = %q, want dialog", openOnly, got[f.dialog])
		}
		if got[f.unknown] != "" {
			t.Errorf("list%s: NULL issue created_via = %q, want empty", openOnly, got[f.unknown])
		}
	}

	// Narrow sort path (effective_due) selects blank columns; created_via must still scan.
	for _, i := range listIssuesCreatedVia(t, "&sort=effective_due") {
		if i.ID == f.dialog && i.CreatedVia != "dialog" {
			t.Errorf("effective_due: created_via = %q, want dialog", i.CreatedVia)
		}
	}
}

func TestIssueTableRows_ReturnsCreatedVia(t *testing.T) {
	f := seedCreatedViaListFixture(t)
	req := newRequest(http.MethodPost, "/api/issues/table/rows", map[string]any{
		"query": map[string]any{
			"scope":   map[string]any{"kind": "workspace"},
			"filters": map[string]any{},
			"sort":    map[string]any{"field": "position", "direction": "asc"},
		},
		"group": map[string]any{"kind": "none"},
		"page":  map[string]any{"limit": 100},
	})
	var resp issueTableRowsResponse
	testutil.Call(t, testHandler.ListIssueTableRows, req).Want(http.StatusOK).JSON(&resp)
	for _, row := range resp.Rows {
		if row.Issue.ID == f.dialog {
			if row.Issue.CreatedVia != "dialog" {
				t.Fatalf("table row created_via = %q, want dialog", row.Issue.CreatedVia)
			}
			return
		}
	}
	t.Fatalf("dialog issue %s missing from table rows", f.dialog)
}

func TestListIssues_FilterByCreatedVia(t *testing.T) {
	f := seedCreatedViaListFixture(t)

	cases := []struct {
		query string
		want  []string
		not   []string
	}{
		{"&created_via=dm,comment", []string{f.dm, f.comment}, []string{f.dialog, f.agent, f.unknown}},
		{"&created_via=dialog", []string{f.dialog}, []string{f.dm, f.comment, f.agent, f.unknown}},
		{"&created_via=unknown", []string{f.unknown}, []string{f.dialog, f.dm, f.comment, f.agent}},
		{"&created_via=dm,unknown", []string{f.dm, f.unknown}, []string{f.dialog, f.comment, f.agent}},
		{"&created_via=dm,comment&open_only=true", []string{f.dm, f.comment}, []string{f.dialog, f.agent, f.unknown}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			ids := createdViaIssueIDs(listIssuesCreatedVia(t, tc.query))
			for _, id := range tc.want {
				if !slices.Contains(ids, id) {
					t.Errorf("missing %s", id)
				}
			}
			for _, id := range tc.not {
				if slices.Contains(ids, id) {
					t.Errorf("leaked %s", id)
				}
			}
		})
	}
}

func TestListIssues_CreatedViaInvalidValue(t *testing.T) {
	for _, bad := range []string{"bogus", "dm,bogus"} {
		req := newRequest(http.MethodGet, "/api/issues?workspace_id="+testWorkspaceID+"&created_via="+url.QueryEscape(bad), nil)
		var body map[string]any
		w := testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusBadRequest)
		w.JSON(&body)
		msg, _ := body["error"].(string)
		for _, v := range []string{"dialog", "dm", "comment", "agent", "autopilot", "cli", "form", "unknown"} {
			if !strings.Contains(msg, v) {
				t.Errorf("created_via=%s: error %q does not name valid value %q", bad, msg, v)
			}
		}
	}
}

func TestListGroupedIssues_FilterByCreatedVia(t *testing.T) {
	f := seedCreatedViaListFixture(t)
	req := newRequest(http.MethodGet, "/api/issues/grouped?workspace_id="+testWorkspaceID+"&created_via=dm,unknown", nil)
	var resp GroupedIssuesResponse
	testutil.Call(t, testHandler.ListGroupedIssues, req).Want(http.StatusOK).JSON(&resp)
	var ids []string
	via := map[string]string{}
	for _, g := range resp.Groups {
		for _, i := range g.Issues {
			ids = append(ids, i.ID)
			via[i.ID] = i.CreatedVia
		}
	}
	for _, id := range []string{f.dm, f.unknown} {
		if !slices.Contains(ids, id) {
			t.Errorf("grouped: missing %s", id)
		}
	}
	for _, id := range []string{f.dialog, f.comment, f.agent} {
		if slices.Contains(ids, id) {
			t.Errorf("grouped: leaked %s", id)
		}
	}
	if via[f.dm] != "dm" {
		t.Errorf("grouped row created_via = %q, want dm", via[f.dm])
	}

	bad := newRequest(http.MethodGet, "/api/issues/grouped?workspace_id="+testWorkspaceID+"&created_via=bogus", nil)
	testutil.Call(t, testHandler.ListGroupedIssues, bad).Want(http.StatusBadRequest)
}

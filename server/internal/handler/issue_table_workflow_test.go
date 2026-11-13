package handler

import (
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The workflow filter matches an issue's own override. An issue that follows
// its project has no override, so it never matches.
func TestIssueTableRowsFilterByWorkflow(t *testing.T) {
	suffix := time.Now().UnixNano()
	workflow := func(name string) string {
		return dbfx.Insert(t, "issue_workflow", testutil.Cols{
			"workspace_id":       testWorkspaceID,
			"name":               fmt.Sprintf("%s %d", name, suffix),
			"initial_status_key": "todo",
		})
	}
	flowA, flowB := workflow("wf a"), workflow("wf b")

	issue := func(title string, workflowID any) string {
		return dbfx.Issue(t, fmt.Sprintf("%s %d", title, suffix),
			testutil.Cols{"workflow_id": workflowID})
	}
	onA := issue("wf on a", flowA)
	onB := issue("wf on b", flowB)
	follows := issue("wf follows project", nil)

	fixture := map[string]struct{}{onA: {}, onB: {}, follows: {}}
	rows := func(filters issueTableFiltersRequest) []string {
		t.Helper()
		var response issueTableRowsResponse
		testutil.Call(t, testHandler.ListIssueTableRows,
			newRequest(http.MethodPost, "/api/issues/table/rows", issueTableRowsRequest{
				Query: issueTableQuerySpec{
					Scope:   issueTableScope{Kind: "workspace"},
					Filters: filters,
					Sort:    issueTableSortRequest{Field: "title", Direction: "asc"},
				},
				Group: issueTableGroupSpec{Kind: "none"},
				Page:  issueTablePageRequest{Limit: 100},
			}),
		).Want(http.StatusOK).JSON(&response)
		ids := []string{}
		for _, row := range response.Rows {
			if _, ok := fixture[row.Issue.ID]; ok {
				ids = append(ids, row.Issue.ID)
			}
		}
		sort.Strings(ids)
		return ids
	}
	assertRows := func(name string, filters issueTableFiltersRequest, want ...string) {
		t.Helper()
		sort.Strings(want)
		if got := rows(filters); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: ids = %v, want %v", name, got, want)
		}
	}

	assertRows("no filter", issueTableFiltersRequest{}, onA, onB, follows)
	assertRows("one workflow", issueTableFiltersRequest{WorkflowIDs: []string{flowA}}, onA)
	assertRows("OR within the dimension", issueTableFiltersRequest{WorkflowIDs: []string{flowA, flowB}}, onA, onB)
}

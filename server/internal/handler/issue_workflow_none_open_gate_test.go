package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// pr.none_open (CHE-8771): an agent closes an issue from in_review through
// agent_accepted only while no open or draft PR is linked to it or its children.

func noneOpenSteps() []map[string]any {
	return []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "todo"},
		{"status_key": "in_progress"},
		{"status_key": "in_review", "edges": []map[string]any{{"to": "agent_accepted", "guard": "agent_done"}}},
		{"status_key": "agent_accepted", "entry": []map[string]any{gate("pr.none_open", "block")}},
		{"status_key": "blocked"},
	}
}

func TestAgentCloseFromInReviewNeedsNoOpenPR(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	createTestCustomStatus(t, "agent_accepted", "done")
	rows := []struct {
		name    string
		prs     []gatePR
		refused bool
	}{
		{"no PR", nil, false},
		{"merged only", []gatePR{mergedPR}, false},
		{"closed only", []gatePR{closedPR}, false},
		{"open PR", []gatePR{openGreen}, true},
		{"draft PR", []gatePR{draftPR}, true},
		{"open beside merged", []gatePR{mergedPR, openRed}, true},
	}
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			installationID := int64(8771000 + i)
			c := newEdgeCase(t, installationID, noneOpenSteps())
			linkGatePRs(t, c, installationID, row.prs...)
			c.enter(t, "in_review")
			res := putIssue(t, c.issueID, map[string]any{"status": "agent_accepted"}, func(req *http.Request) *http.Request { return asAgentTask(t, req) })
			if !row.refused {
				res.Want(http.StatusOK)
				wantIssueStatus(t, c.issueID, "agent_accepted")
				return
			}
			res.Want(http.StatusConflict)
			if got := failedGuards(t, res); len(got) != 1 || got[0] != "pr.none_open" {
				t.Fatalf("failed guards = %v, want [pr.none_open]; body %s", got, res.Text())
			}
			// The refusal names the open PR, as the number linkGatePRs gave it.
			open := 0
			for j, p := range row.prs {
				if p.state != "open" && p.state != "draft" {
					continue
				}
				open++
				ref := fmt.Sprintf("acme/widget#%d", int32(installationID%100000)*10+int32(j))
				if !strings.Contains(res.Text(), ref) {
					t.Fatalf("refusal must name %s; body %s", ref, res.Text())
				}
			}
			if open == 0 {
				t.Fatal("test row refuses without an open PR")
			}
			wantIssueStatus(t, c.issueID, "in_review")
		})
	}
}

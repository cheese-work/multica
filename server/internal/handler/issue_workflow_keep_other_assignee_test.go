package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A step flagged keep_other_assignee leaves an assignee chosen on purpose in
// place (CHE-5013). TEMPORARY: CHE-5024 (per-issue workflows) replaces it.
// Every other case keeps the handoff: an unflagged step, an unassigned issue,
// an issue already on the handler, and an issue owned by the handler of the
// step it leaves.
func TestKeepOtherAssigneeStepSkipsHandoffOnlyForAChosenAssignee(t *testing.T) {
	seedTestCatalog(t)
	handlerID := seededReadyAgentID(t)
	otherID := dbfx.Agent(t, "keep-other chosen "+workflowTestSuffix(), handlerTestRuntimeID(t))
	leavingID := dbfx.Agent(t, "keep-other leaving "+workflowTestSuffix(), handlerTestRuntimeID(t))

	const (
		none     = ""
		other    = "other"
		handler  = "handler"
		leaving  = "leaving"
		keptRuns = "kept"
	)
	cases := []struct {
		name         string
		flag         bool
		assignee     string // who owns the issue before the move
		noStart      bool
		wantAssignee string // who owns it after
		wantRuns     map[string]int
	}{
		{"flagged, other agent, start", true, other, false, other, map[string]int{other: 1, handler: 0}},
		{"flagged, other agent, no-start", true, other, true, other, map[string]int{other: 0, handler: 0}},
		{"flagged, unassigned", true, none, false, handler, map[string]int{handler: 1}},
		{"flagged, already the handler", true, handler, false, handler, map[string]int{handler: 1}},
		{"flagged, handler of the step it leaves", true, leaving, false, handler, map[string]int{handler: 1, leaving: 0}},
		{"unflagged, other agent", false, other, false, handler, map[string]int{handler: 1, other: 0}},
	}
	ids := map[string]string{other: otherID, handler: handlerID, leaving: leavingID}
	for _, writer := range issueWriters {
		for _, tc := range cases {
			t.Run(writer.name+"/"+tc.name, func(t *testing.T) {
				projectID := createWorkflowTestProject(t, "Keep-other project "+workflowTestSuffix())
				setColumnHandoff(t, projectID, "todo", handToAgent(leavingID, "Triage it.")).Want(http.StatusOK)
				step := handToAgent(handlerID, "Implement it.")
				if tc.flag {
					step["keep_other_assignee"] = true
				}
				setColumnHandoff(t, projectID, "in_progress", step).Want(http.StatusOK)

				cols := testutil.Cols{"project_id": projectID, "status": "todo"}
				if tc.assignee != none {
					cols["assignee_type"], cols["assignee_id"] = "agent", ids[tc.assignee]
				}
				issueID := dbfx.Issue(t, "keep other "+tc.name, cols)

				body := map[string]any{"status": "in_progress"}
				if tc.noStart {
					body["suppress_run"] = true
				}
				writer.write(t, issueID, body, nil).Want(http.StatusOK)

				_, _, gotID := issueStatusAndAssignee(t, issueID)
				if want := ids[tc.wantAssignee]; gotID != want {
					t.Fatalf("assignee = %s, want %s (%s)", gotID, want, tc.wantAssignee)
				}
				for who, want := range tc.wantRuns {
					if got := len(pendingStepRuns(t, issueID, ids[who])); got != want {
						t.Fatalf("%s runs = %d, want %d", who, got, want)
					}
				}
			})
		}
	}
}

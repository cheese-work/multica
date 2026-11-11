package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// --no-start (suppress_run) starts no run, including the run of a workflow
// step's handler, whoever the handler is (CHE-4964). Without it the handler's
// run still starts.
func TestNoStartSuppressesHandoffRunForEveryHandler(t *testing.T) {
	seedTestCatalog(t)
	handlerID := seededReadyAgentID(t)
	actorID := dbfx.Agent(t, "no-start actor "+workflowTestSuffix(), handlerTestRuntimeID(t))

	const (
		asMember  = "member"
		asHandler = "handler" // the actor is the step's handler itself
		asOther   = "other agent"
	)
	cases := []struct {
		name    string
		actor   string
		handoff bool
		noStart bool
		wantRun bool
	}{
		{"handoff, other agent, no-start", asOther, true, true, false},
		{"handoff, other agent, start", asOther, true, false, true},
		{"handoff, member, no-start", asMember, true, true, false},
		{"handoff, member, start", asMember, true, false, true},
		{"handoff, actor is handler, no-start", asHandler, true, true, false},
		{"handoff, actor is handler, start", asHandler, true, false, true},
		{"no handoff, member, no-start", asMember, false, true, false},
		{"no handoff, member, start", asMember, false, false, true},
	}
	for _, writer := range issueWriters {
		for _, tc := range cases {
			t.Run(writer.name+"/"+tc.name, func(t *testing.T) {
				projectID := createWorkflowTestProject(t, "No-start project "+workflowTestSuffix())
				if tc.handoff {
					setColumnHandoff(t, projectID, "in_review", handToAgent(handlerID, "Review it.")).Want(http.StatusOK)
				}
				issueID := dbfx.Issue(t, "no-start "+tc.name, testutil.Cols{"project_id": projectID, "status": "in_progress"})

				headers := map[string]string{}
				if tc.actor != asMember {
					actingAgent := actorID
					if tc.actor == asHandler {
						actingAgent = handlerID
					}
					taskID := dbfx.Task(t, actingAgent, testutil.Cols{
						"issue_id": issueID, "runtime_id": handlerTestRuntimeID(t), "status": "running", "workflow_step": "in_progress",
					})
					headers = map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": actingAgent, "X-Task-ID": taskID}
				}

				body := map[string]any{}
				if tc.handoff {
					body["status"] = "in_review"
				} else {
					body["assignee_type"], body["assignee_id"] = "agent", handlerID
				}
				if tc.noStart {
					body["suppress_run"] = true
				}
				writer.write(t, issueID, body, headers).Want(http.StatusOK)

				if runs := len(pendingStepRuns(t, issueID, handlerID)); (runs == 1) != tc.wantRun {
					t.Fatalf("handler runs = %d, want run=%v", runs, tc.wantRun)
				}
			})
		}
	}
}

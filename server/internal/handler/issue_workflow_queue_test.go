package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// pendingStepRuns lists the workflow step of every pending run the agent holds
// on the issue.
func pendingStepRuns(t *testing.T, issueID, agentID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT COALESCE(workflow_step, '') FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched')`, issueID, agentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, step)
	}
	return steps
}

// A queued run stamped for an earlier step must not stand in for the run the
// next step needs. When the same agent handles two successive steps, entering
// the second leaves one pending run, and it is stamped for the second step;
// otherwise that run would be refused with workflow_step_moved the moment it
// moved the issue, and no run could finish the step.
func TestSameAgentSuccessiveStepGetsAnExecutableRun(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	for _, writer := range issueWriters {
		t.Run(writer.name, func(t *testing.T) {
			projectID := deliveryProject(t, "Workflow successive step project", agentID)
			setColumnHandoff(t, projectID, "in_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
			issueID := dbfx.Issue(t, "successive steps", testutil.Cols{"project_id": projectID, "status": "todo"})

			updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
			if steps := pendingStepRuns(t, issueID, agentID); len(steps) != 1 || steps[0] != "in_progress" {
				t.Fatalf("runs after the first step = %v, want one for in_progress", steps)
			}

			writer.write(t, issueID, map[string]any{"status": "in_review"}, nil).Want(http.StatusOK)
			if steps := pendingStepRuns(t, issueID, agentID); len(steps) != 1 || steps[0] != "in_review" {
				t.Fatalf("pending runs after the second step = %v, want exactly one, stamped in_review", steps)
			}
		})
	}
}

// Cancelling the previous assignee's runs must happen before the enqueue
// decision. A pending run made for the prior squad that is also the new
// handler's run would otherwise suppress the new run and then be cancelled,
// leaving the handler with nothing to run.
func TestHandoffFromSquadKeepsAnExecutableRunForTheLeader(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	for _, writer := range issueWriters {
		t.Run(writer.name, func(t *testing.T) {
			squadID := dbfx.Squad(t, "Workflow cancel squad "+workflowTestSuffix(), agentID)
			projectID := createWorkflowTestProject(t, "Workflow squad cancel project")
			setColumnHandoff(t, projectID, "in_review", handToAgent(agentID, "Take it from the squad.")).Want(http.StatusOK)
			issueID := dbfx.Issue(t, "squad then agent", testutil.Cols{
				"project_id": projectID, "status": "in_progress", "assignee_type": "squad", "assignee_id": squadID,
			})
			squadRun := dbfx.Task(t, agentID, testutil.Cols{
				"issue_id": issueID, "squad_id": squadID, "runtime_id": runtimeID, "status": "queued", "workflow_step": "in_review",
			})

			writer.write(t, issueID, map[string]any{"status": "in_review", "stop_previous_assignee_runs": true}, nil).Want(http.StatusOK)

			var squadRunStatus string
			dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, squadRun).Scan(&squadRunStatus)
			if squadRunStatus != "cancelled" {
				t.Fatalf("the squad's run = %q, want cancelled", squadRunStatus)
			}
			if steps := pendingStepRuns(t, issueID, agentID); len(steps) != 1 || steps[0] != "in_review" {
				t.Fatalf("pending runs for the new handler = %v, want one for in_review: the cancelled run left no replacement", steps)
			}
		})
	}
}

// A write that hands an issue to an agent skips that agent's event wakeups
// only when the agent gets the change as a run. A backlog assignment and a
// suppressed run start none, so the wakeup must still capture the event;
// otherwise the change reaches the agent through neither path.
func TestHandedToAgentWakeupKeptWhenNoDirectRunStarts(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	svc := service.IssueWakeupService{Tasks: testHandler.TaskService}

	cases := []struct {
		name        string
		status      string
		suppressRun bool
		wantRun     bool
	}{
		{"assignment in backlog parks without a run", "backlog", false, false},
		{"assignment with the run suppressed", "todo", true, false},
		{"assignment that starts a run", "todo", false, true},
	}
	for _, writer := range issueWriters {
		for _, tc := range cases {
			t.Run(writer.name+"/"+tc.name, func(t *testing.T) {
				issueID := dbfx.Issue(t, "wakeup "+tc.name, testutil.Cols{"status": tc.status})
				wakeup, err := svc.Create(context.Background(), parseUUID(issueID), parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{
					AgentID: agentID, Kind: "event", EventTypes: []string{"issue.assignee_changed"}, Instruction: "look again",
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					testPool.Exec(context.Background(), `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID)
					testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id = $1`, issueID)
				})

				body := map[string]any{"assignee_type": "agent", "assignee_id": agentID}
				if tc.suppressRun {
					body["suppress_run"] = true
				}
				writer.write(t, issueID, body, nil).Want(http.StatusOK)

				runs := len(pendingStepRuns(t, issueID, agentID))
				var receipts int
				dbfx.QueryRow(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1 AND processed_at IS NULL`, wakeup.ID).Scan(&receipts)
				if (runs == 1) != tc.wantRun {
					t.Fatalf("direct runs = %d, want run=%v", runs, tc.wantRun)
				}
				// The agent hears about the change exactly once: as the run, or
				// through the wakeup.
				if runs+receipts != 1 {
					t.Fatalf("direct runs = %d and wakeup receipts = %d, want exactly one delivery", runs, receipts)
				}
			})
		}
	}
}

// A two-rune workflow name leaves no room for a 61-rune project title in
// "Workflow (Project)". The fallback numbering must still fit the name limit
// instead of slicing past the workflow name.
func TestProjectCopyNameShortWorkflowLongProject(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("naming the copy panicked: %v", r)
		}
	}()
	project := strings.Repeat("p", 61)
	got := projectCopyName("QA", project, nil)
	if n := utf8.RuneCountInString(got); n < 1 || n > issueworkflow.MaxNameRunes || !strings.HasPrefix(got, "QA") {
		t.Fatalf("copy name = %q (%d runes)", got, n)
	}
	if again := projectCopyName("QA", project, map[string]bool{strings.ToLower(got): true}); again == got {
		t.Fatalf("a taken name was returned again: %q", again)
	}
}

// The same inputs through the handler: saving a project-specific handoff on a
// short-named workflow for a project with a 61-rune title.
func TestWorkflowHandoffCopyForLongProjectTitle(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("copying the workflow panicked: %v", r)
		}
	}()
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	wf := createTestWorkflow(t, "QA"+workflowTestSuffix(), "in_progress")
	projectID := createWorkflowTestProject(t, strings.Repeat("p", 61))
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)

	body := handToAgent(agentID, "Only here")
	body["copy_for_project_id"] = projectID
	var copied IssueWorkflowResponse
	setWorkflowHandoff(t, wf.ID, "in_progress", body).Want(http.StatusCreated).JSON(&copied)
	if n := utf8.RuneCountInString(copied.Name); n < 1 || n > issueworkflow.MaxNameRunes {
		t.Fatalf("copy name %q has %d runes", copied.Name, n)
	}
}

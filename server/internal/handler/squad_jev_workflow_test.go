package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/jev"
)

// jevWorkflowSquad builds a squad whose Jev routing is on and always picks its
// one worker with high confidence.
func jevWorkflowSquad(t *testing.T) db.Squad {
	t.Helper()
	ctx := context.Background()
	suffix := workflowTestSuffix()
	leaderID := dbfx.Agent(t, "jev-wf-leader-"+suffix, testRuntimeID)
	workerName := "jev-wf-worker-" + suffix
	workerID := dbfx.Agent(t, workerName, testRuntimeID)
	squadID := dbfx.Squad(t, "Jev workflow squad "+suffix, leaderID, testutil.Cols{"instructions": "Tier S: " + workerName})
	dbfx.SquadMember(t, squadID, "agent", workerID, testutil.Cols{"role": "worker"})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{"route_squad_member": {
				Type: jev.TypeChoice, Choice: workerID, Confidence: 0.95,
				Probabilities: map[string]float64{workerID: 0.95, "escalate_to_leader": 0.05},
			}},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		})
	}))
	t.Cleanup(server.Close)

	minConfidence := 0.90
	settings, _ := json.Marshal(governance.WorkspaceSettings{JevSquadRoutingEnabled: true, JevAPIURL: server.URL, JevMinConfidence: &minConfidence})
	if _, err := testPool.Exec(ctx, `INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2) ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings`,
		util.MustParseUUID(testWorkspaceID), settings); err != nil {
		t.Fatal(err)
	}
	client, err := jev.NewClient(jev.Options{APIKey: "test-jev-key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	original := testHandler.Jev
	testHandler.Jev = client
	t.Cleanup(func() { testHandler.Jev = original })

	squad, err := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: util.MustParseUUID(squadID), WorkspaceID: util.MustParseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	return squad
}

func routeIssueThroughJev(t *testing.T, squad db.Squad, issueID string) bool {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, squad, "member", testUserID, testUserID)
	if !routed {
		t.Fatalf("routing did not dispatch: %s", advisory)
	}
	return routed
}

// Routing moves a backlog or todo issue to in_progress. A named workflow may
// omit in_progress; routing then dispatches the worker and leaves the status.
func TestJevRoutingKeepsTheIssueInsideTheProjectWorkflow(t *testing.T) {
	seedTestCatalog(t)
	squad := jevWorkflowSquad(t)
	squadID := uuidToString(squad.ID)

	cases := []struct {
		name       string
		middle     []string
		wantStatus string
	}{
		{"workflow omits in_progress", nil, "todo"},
		{"workflow lists in_progress", []string{"in_progress"}, "in_progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := createTestWorkflow(t, "Jev workflow "+workflowTestSuffix(), tc.middle...)
			projectID := createWorkflowTestProject(t, "Jev workflow project")
			setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
			issueID := dbfx.Issue(t, "jev workflow", testutil.Cols{
				"project_id": projectID, "status": "todo", "assignee_type": "squad", "assignee_id": squadID,
			})

			routeIssueThroughJev(t, squad, issueID)

			wantIssueStatus(t, issueID, tc.wantStatus)
		})
	}
}

// A workflow switch that commits while routing is in flight must not leave the
// issue on a status the new workflow omits.
func TestJevRoutingRechecksWorkflowSwitchedMidFlight(t *testing.T) {
	seedTestCatalog(t)
	squad := jevWorkflowSquad(t)
	listsIt := createTestWorkflow(t, "Jev switch from "+workflowTestSuffix(), "in_progress")
	without := createTestWorkflow(t, "Jev switch to "+workflowTestSuffix())
	projectID := createWorkflowTestProject(t, "Jev switch project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": listsIt.ID}).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "jev switch", testutil.Cols{
		"project_id": projectID, "status": "todo", "assignee_type": "squad", "assignee_id": uuidToString(squad.ID),
	})

	raceUntilBlocked(t, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `SELECT 1 FROM project WHERE id = $1 FOR UPDATE`, projectID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(context.Background(), `UPDATE project SET workflow_id = $2 WHERE id = $1`, projectID, without.ID); err != nil {
			t.Fatal(err)
		}
	}, func() { routeIssueThroughJev(t, squad, issueID) })

	wantIssueStatus(t, issueID, "todo")
}

// An issue assigned to a squad is delivered to the squad's leader. When Jev
// routes the work to a specialist instead, the leader runs nothing, so the
// leader's event receipts for this write are the leader's only delivery and
// must stay pending.
func TestJevRoutedSquadAssignmentKeepsTheLeadersReceipts(t *testing.T) {
	seedTestCatalog(t)
	squad := jevWorkflowSquad(t)
	squadID := uuidToString(squad.ID)
	leaderID := uuidToString(squad.LeaderID)

	for _, writer := range issueWriters {
		t.Run(writer.name, func(t *testing.T) {
			issueID := dbfx.Issue(t, "jev squad assignment", testutil.Cols{"status": "todo"})
			wakeupID := subscribeToIssueEvent(t, issueID, leaderID, "issue.assignee_changed")

			writer.write(t, issueID, map[string]any{"assignee_type": "squad", "assignee_id": squadID}, nil).Want(http.StatusOK)

			var specialistRuns int
			dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id <> $2 AND status IN ('queued','dispatched')`, issueID, leaderID).Scan(&specialistRuns)
			if specialistRuns != 1 {
				t.Fatalf("specialist runs = %d, want 1: Jev should have routed the work", specialistRuns)
			}
			if leaderRuns := len(pendingStepRuns(t, issueID, leaderID)); leaderRuns != 0 {
				t.Fatalf("leader runs = %d, want 0 after routing", leaderRuns)
			}
			if got := pendingEventReceipts(t, wakeupID); got != 1 {
				t.Fatalf("leader's pending receipts = %d, want 1: no leader run carries this write", got)
			}
		})
	}
}

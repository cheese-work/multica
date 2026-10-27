package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// handoffWakeupCase is one way a write hands an issue to an agent, with the
// event wakeup that agent subscribed to on the issue.
type handoffWakeupCase struct {
	name      string
	eventType string
	// setup returns the issue and the body of the handing write.
	setup func(t *testing.T, agentID string) (issueID string, body map[string]any)
}

var handoffWakeupCases = []handoffWakeupCase{
	{"assignment", "issue.assignee_changed", func(t *testing.T, agentID string) (string, map[string]any) {
		return dbfx.Issue(t, "handoff wakeup assignment", testutil.Cols{"status": "todo"}),
			map[string]any{"assignee_type": "agent", "assignee_id": agentID}
	}},
	{"workflow step", "issue.status_changed", func(t *testing.T, agentID string) (string, map[string]any) {
		projectID := deliveryProject(t, "Handoff wakeup project", agentID)
		return dbfx.Issue(t, "handoff wakeup step", testutil.Cols{"project_id": projectID, "status": "todo"}),
			map[string]any{"status": "in_progress"}
	}},
}

func subscribeToIssueEvent(t *testing.T, issueID, agentID, eventType string) string {
	t.Helper()
	svc := service.IssueWakeupService{Tasks: testHandler.TaskService}
	wakeup, err := svc.Create(context.Background(), parseUUID(issueID), parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{
		AgentID: agentID, Kind: "event", EventTypes: []string{eventType}, Instruction: "look again",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id = $1`, issueID)
	})
	return uuidToString(wakeup.ID)
}

// onRunEnqueue installs a trigger that runs when the issue's direct run is
// inserted: it notes how many event receipts of the wakeup are pending and
// committed at that moment, and refuses the insert when fail is set.
func onRunEnqueue(t *testing.T, issueID, wakeupID string, fail bool) (pendingAtEnqueue func() []int) {
	t.Helper()
	suffix := strings.ReplaceAll(workflowTestSuffix(), "-", "")
	table, fn := "che1837_probe_"+suffix, "che1837_probe_fn_"+suffix
	refuse := ""
	if fail {
		refuse = "RAISE EXCEPTION 'enqueue refused for the test';"
	}
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE TABLE %s (pending int)`, table),
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			INSERT INTO %s SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = '%s' AND processed_at IS NULL;
			%s RETURN NEW; END $$`, fn, table, wakeupID, refuse),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON agent_task_queue FOR EACH ROW WHEN (NEW.issue_id = '%s') EXECUTE FUNCTION %s()`, fn, issueID, fn),
	} {
		dbfx.Exec(t, stmt)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON agent_task_queue`, fn))
		testPool.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
		testPool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table))
	})
	return func() []int {
		rows, err := testPool.Query(context.Background(), fmt.Sprintf(`SELECT pending FROM %s`, table))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []int
		for rows.Next() {
			var n int
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
}

func pendingEventReceipts(t *testing.T, wakeupID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1 AND processed_at IS NULL`, wakeupID).Scan(&n)
	return n
}

// A write that hands an issue to an agent starts the agent's run after the
// write commits. A process that stops between the two must not lose the
// change: the agent's event receipt is already committed with the write, and
// the run consumes it only once the run exists.
func TestHandedToAgentReceiptIsCommittedBeforeTheRunExists(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	for _, writer := range issueWriters {
		for _, tc := range handoffWakeupCases {
			t.Run(writer.name+"/"+tc.name, func(t *testing.T) {
				issueID, body := tc.setup(t, agentID)
				wakeupID := subscribeToIssueEvent(t, issueID, agentID, tc.eventType)
				pendingAtEnqueue := onRunEnqueue(t, issueID, wakeupID, false)

				writer.write(t, issueID, body, nil).Want(http.StatusOK)

				if got := pendingAtEnqueue(); len(got) != 1 || got[0] != 1 {
					t.Fatalf("pending receipts when the run was enqueued = %v, want one: a stop here loses the change", got)
				}
				if runs := len(pendingStepRuns(t, issueID, agentID)); runs != 1 {
					t.Fatalf("direct runs = %d, want 1", runs)
				}
				// The agent hears about the change once, as the run.
				if n := pendingEventReceipts(t, wakeupID); n != 0 {
					t.Fatalf("pending receipts after the run was enqueued = %d, want 0: the change would be delivered twice", n)
				}
			})
		}
	}
}

// A run that cannot be enqueued after the write committed leaves the issue
// event as the agent's only delivery.
func TestHandedToAgentReceiptSurvivesAFailedRunEnqueue(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	for _, writer := range issueWriters {
		for _, tc := range handoffWakeupCases {
			t.Run(writer.name+"/"+tc.name, func(t *testing.T) {
				issueID, body := tc.setup(t, agentID)
				wakeupID := subscribeToIssueEvent(t, issueID, agentID, tc.eventType)
				onRunEnqueue(t, issueID, wakeupID, true)

				writer.write(t, issueID, body, nil).Want(http.StatusOK)

				runs := len(pendingStepRuns(t, issueID, agentID))
				receipts := pendingEventReceipts(t, wakeupID)
				if runs != 0 {
					t.Fatalf("direct runs = %d, want 0 after the refused enqueue", runs)
				}
				if receipts != 1 {
					t.Fatalf("pending receipts = %d, want 1: the change reached the agent through neither path", receipts)
				}
			})
		}
	}
}

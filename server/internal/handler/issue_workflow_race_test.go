package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Issue writes against a concurrent workflow edit, project switch or member
// write. Each test holds the competing writer's transaction open, starts the
// issue write, waits until the write is observably blocked on a lock, and only
// then lets the competing writer commit. The outcome therefore never depends
// on timing.

// raceAgainst runs write while hold's transaction is uncommitted, then commits
// it. It fails when write finishes before the commit: a write that does not
// wait for the competing writer decided on state that writer is changing.
func raceAgainst(t *testing.T, hold func(tx pgx.Tx), write func() *testutil.Response) *testutil.Response {
	t.Helper()
	var resp *testutil.Response
	raceUntilBlocked(t, hold, func() { resp = write() })
	return resp
}

// raceUntilBlocked is raceAgainst for a write that returns no HTTP response.
// whileBlocked, when given, runs in the competing transaction once the write is
// blocked and before that transaction commits.
//
// "Blocked" means a backend is waiting on a lock the competing transaction
// holds, which pg_blocking_pids reports for that transaction's own backend. A
// lock waiter held up by anyone else does not count, so the hook cannot fire
// before the write has reached the lock under test.
func raceUntilBlocked(t *testing.T, hold func(tx pgx.Tx), write func(), whileBlocked ...func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	// The poll uses its own connection: the test holds and blocks pool
	// connections, and a pool with every connection taken would hang the poll.
	monitor := dedicatedConn(t)
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var holderPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	hold(tx)

	done := make(chan struct{})
	go func() { write(); close(done) }()

	deadline := time.Now().Add(10 * time.Second)
	for blocked := false; !blocked; {
		select {
		case <-done:
			t.Fatal("the write finished while a competing workflow or issue writer was uncommitted")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the write neither finished nor blocked on the competing transaction")
		}
		if err := monitor.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if !blocked {
			time.Sleep(10 * time.Millisecond)
		}
	}
	for _, step := range whileBlocked {
		step(tx)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the write never finished after the competing writer committed")
	}
}

// dedicatedConn opens a connection outside the test pool, closed with the test.
func dedicatedConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(context.Background(), testPool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// issueWriters are the two entry points that apply a workflow to an issue
// write: one issue, and a batch. withDescription adds a description so the
// write takes the atomic path that locks the issue row.
type issueWriter struct {
	name  string
	write func(t *testing.T, issueID string, body map[string]any, headers map[string]string) *testutil.Response
}

var issueWriters = []issueWriter{
	{"single", func(t *testing.T, issueID string, body map[string]any, headers map[string]string) *testutil.Response {
		req := withURLParam(newRequest("PUT", "/api/issues/"+issueID, body), "id", issueID)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return testutil.Call(t, testHandler.UpdateIssue, req)
	}},
	{"batch", func(t *testing.T, issueID string, body map[string]any, headers map[string]string) *testutil.Response {
		req := newRequest("POST", "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
			"issue_ids": []string{issueID},
			"updates":   body,
		})
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return testutil.Call(t, testHandler.BatchUpdateIssues, req)
	}},
}

func wantIssueStatus(t *testing.T, issueID, want string) {
	t.Helper()
	if status, _, _ := issueStatusAndAssignee(t, issueID); status != want {
		t.Fatalf("status = %q, want %q: the write must not leave the issue outside its workflow or over a newer move", status, want)
	}
}

func lockStatusCatalog(t *testing.T, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(context.Background(),
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':issue_status', 0))`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
}

func setWorkflowSteps(t *testing.T, tx pgx.Tx, workflowID string, steps []issueworkflow.Step) {
	t.Helper()
	raw, err := issueworkflow.EncodeSteps(steps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE issue_workflow SET steps = $2 WHERE id = $1`, workflowID, raw); err != nil {
		t.Fatal(err)
	}
}

func bareSteps(keys ...string) []issueworkflow.Step {
	steps := make([]issueworkflow.Step, len(keys))
	for i, key := range keys {
		steps[i] = issueworkflow.Step{StatusKey: key, Handler: issueworkflow.Handler{Type: issueworkflow.HandlerNone}}
	}
	return steps
}

// A workflow edit that commits after an issue write resolved the workflow
// must not leave the issue on a status the edited workflow no longer lists,
// nor hand it to a handler the edit replaced.
func TestIssueWriteRechecksWorkflowEditedMidFlight(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	otherAgent := dbfx.Agent(t, "Workflow race other handler "+workflowTestSuffix(), runtimeID)

	for _, writer := range issueWriters {
		for _, withDescription := range []bool{false, true} {
			name := writer.name
			if withDescription {
				name += " with description"
			}
			t.Run("edit drops the status/"+name, func(t *testing.T) {
				wf := createTestWorkflow(t, "Race drop "+workflowTestSuffix(), "in_progress")
				projectID := createWorkflowTestProject(t, "Workflow race drop project")
				setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
				issueID := dbfx.Issue(t, "race drop", testutil.Cols{"project_id": projectID, "status": "todo"})

				body := map[string]any{"status": "in_progress"}
				if withDescription {
					body["description"] = "edited with the status"
				}
				resp := raceAgainst(t, func(tx pgx.Tx) {
					lockStatusCatalog(t, tx)
					setWorkflowSteps(t, tx, wf.ID, bareSteps("backlog", "todo", "blocked", "done", "cancelled"))
				}, func() *testutil.Response { return writer.write(t, issueID, body, nil) })

				if writer.name == "single" && resp.Code == http.StatusOK {
					t.Fatalf("single write answered 200: %s", resp.Body.String())
				}
				wantIssueStatus(t, issueID, "todo")
			})

			t.Run("edit replaces the handler/"+name, func(t *testing.T) {
				wf := createTestWorkflow(t, "Race handler "+workflowTestSuffix(), "in_progress")
				setWorkflowHandoff(t, wf.ID, "in_progress", handToAgent(agentID, "first handler")).Want(http.StatusOK)
				projectID := createWorkflowTestProject(t, "Workflow race handler project")
				setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
				issueID := dbfx.Issue(t, "race handler", testutil.Cols{"project_id": projectID, "status": "todo"})

				body := map[string]any{"status": "in_progress"}
				if withDescription {
					body["description"] = "edited with the status"
				}
				resp := raceAgainst(t, func(tx pgx.Tx) {
					lockStatusCatalog(t, tx)
					steps := bareSteps("backlog", "todo", "in_progress", "blocked", "done", "cancelled")
					steps[2].Handler = issueworkflow.Handler{Type: issueworkflow.HandlerAgent, ID: otherAgent}
					steps[2].Instructions = "second handler"
					setWorkflowSteps(t, tx, wf.ID, steps)
				}, func() *testutil.Response { return writer.write(t, issueID, body, nil) })

				if writer.name == "single" {
					resp.Want(http.StatusConflict)
				}
				// Neither handler may be applied from the stale definition.
				status, _, assigneeID := issueStatusAndAssignee(t, issueID)
				if status == "in_progress" && assigneeID == agentID {
					t.Fatalf("the write applied the replaced handler %s", agentID)
				}
			})
		}
	}
}

// A project switched to another workflow after an issue write resolved the old
// one must not receive a status only the old workflow lists.
func TestIssueWriteRechecksProjectSwitchedMidFlight(t *testing.T) {
	seedTestCatalog(t)
	for _, writer := range issueWriters {
		t.Run(writer.name, func(t *testing.T) {
			listsIt := createTestWorkflow(t, "Race switch from "+workflowTestSuffix(), "in_progress")
			without := createTestWorkflow(t, "Race switch to "+workflowTestSuffix())
			projectID := createWorkflowTestProject(t, "Workflow race switch project")
			setProjectWorkflow(t, projectID, map[string]any{"workflow_id": listsIt.ID}).Want(http.StatusOK)
			issueID := dbfx.Issue(t, "race switch", testutil.Cols{"project_id": projectID, "status": "todo"})

			resp := raceAgainst(t, func(tx pgx.Tx) {
				if _, err := tx.Exec(context.Background(), `SELECT 1 FROM project WHERE id = $1 FOR UPDATE`, projectID); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(context.Background(), `UPDATE project SET workflow_id = $2 WHERE id = $1`, projectID, without.ID); err != nil {
					t.Fatal(err)
				}
			}, func() *testutil.Response {
				return writer.write(t, issueID, map[string]any{"status": "in_progress"}, nil)
			})

			if writer.name == "single" && resp.Code == http.StatusOK {
				t.Fatalf("single write answered 200: %s", resp.Body.String())
			}
			wantIssueStatus(t, issueID, "todo")
		})
	}
}

// An agent's run decided for the step it worked on. A member who moves the
// issue after the handler read it, but before the agent's write lands, wins:
// the agent write must be refused as stale, not overwrite the newer move.
func TestAgentWriteDoesNotOverwriteNewerMemberMove(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	for _, writer := range issueWriters {
		for _, withDescription := range []bool{false, true} {
			name := writer.name
			if withDescription {
				name += " with description"
			}
			t.Run(name, func(t *testing.T) {
				projectID := deliveryProject(t, "Workflow race member project", agentID)
				issueID := dbfx.Issue(t, "race member", testutil.Cols{"project_id": projectID, "status": "todo"})
				updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
				var taskID string
				dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID).Scan(&taskID)
				dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, taskID)

				body := map[string]any{"status": "in_review"}
				if withDescription {
					body["description"] = "edited with the status"
				}
				headers := map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": agentID, "X-Task-ID": taskID}
				resp := raceAgainst(t, func(tx pgx.Tx) {
					if _, err := tx.Exec(context.Background(), `UPDATE issue SET status = 'blocked' WHERE id = $1`, util.MustParseUUID(issueID)); err != nil {
						t.Fatal(err)
					}
				}, func() *testutil.Response { return writer.write(t, issueID, body, headers) })

				if writer.name == "single" {
					resp.Want(http.StatusConflict)
				}
				wantIssueStatus(t, issueID, "blocked")
			})
		}
	}
}

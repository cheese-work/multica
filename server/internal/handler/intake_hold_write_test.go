package handler

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7071 (child A2 of CHE-6911): E1, the hold on status writes and creates.

// holdLog captures the slog lines of the test, so an audit line can be asserted.
type holdLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *holdLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *holdLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureHoldLog(test *testing.T) *holdLog {
	test.Helper()
	captured := &holdLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	test.Cleanup(func() { slog.SetDefault(previous) })
	return captured
}

func holdAgentPut(test *testing.T, issueID, status string) *testutil.Response {
	test.Helper()
	return putIssue(test, issueID, map[string]any{"status": status}, func(request *http.Request) *http.Request { return asAgentTask(test, request) })
}

func wantIntakeRefusal(test *testing.T, response *testutil.Response) {
	test.Helper()
	response.Want(http.StatusConflict)
	body := response.Map()
	if body["code"] != "intake_hold" {
		test.Fatalf("code = %v, want intake_hold; body %s", body["code"], response.Text())
	}
	for _, key := range []string{"ends_at", "fix"} {
		if text, _ := body[key].(string); text == "" {
			test.Fatalf("refusal has no %s: %s", key, response.Text())
		}
	}
}

func holdStatusRows(test *testing.T, issueID string) int {
	test.Helper()
	return activityCount(test, issueID, "status_changed")
}

func holdIssueStatus(test *testing.T, issueID string) string {
	test.Helper()
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
	return status
}

func TestIntakeHoldRefusesAgentStatusMoves(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, nil)
	logged := captureHoldLog(test)
	issueID := fixture.issue(test, "feat: new request")
	for _, status := range []string{"todo", "in_progress", "in_review", "blocked", "done"} {
		test.Run(status, func(test *testing.T) {
			wantIntakeRefusal(test, holdAgentPut(test, issueID, status))
			if got := holdIssueStatus(test, issueID); got != "backlog" {
				test.Fatalf("status = %s, want backlog", got)
			}
		})
	}
	if rows := holdStatusRows(test, issueID); rows != 0 {
		test.Fatalf("a refused move wrote %d status rows", rows)
	}
	if !strings.Contains(logged.String(), "intake_hold") {
		test.Fatalf("no intake_hold audit line in %q", logged.String())
	}
}

func TestIntakeHoldRefusesBatchAndRoute(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, nil)
	issueID := fixture.issue(test, "feat: new request")

	batch := testutil.Call(test, testHandler.BatchUpdateIssues, asAgentTask(test, newRequest("POST", "/api/issues/batch-update?workspace_id="+testWorkspaceID, map[string]any{
		"issue_ids": []string{issueID}, "updates": map[string]any{"status": "in_progress"},
	}))).Want(http.StatusOK)
	failed, _ := batch.Map()["failed"].([]any)
	if len(failed) != 1 || failed[0].(map[string]any)["code"] != "intake_hold" {
		test.Fatalf("batch failed = %v, want one intake_hold; body %s", failed, batch.Text())
	}

	route := asAgentTask(test, withURLParam(newRequest("POST", "/api/issues/"+issueID+"/route", map[string]any{
		"status": "in_progress", "reason": "go", "confidence": 0.9,
	}), "id", issueID))
	wantIntakeRefusal(test, testutil.Call(test, testHandler.RouteIssue, route))

	if got := holdIssueStatus(test, issueID); got != "backlog" || holdStatusRows(test, issueID) != 0 {
		test.Fatalf("status = %s with %d rows, want backlog with none", got, holdStatusRows(test, issueID))
	}
}

func holdCreateAs(test *testing.T, body map[string]any, agent bool) *testutil.Response {
	test.Helper()
	// Fixture issues are numbered by MAX(number); catch the counter up.
	dbfx.Exec(test, `UPDATE workspace SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) WHERE id = $1`, testWorkspaceID)
	request := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)
	if agent {
		request = asAgentTask(test, request)
	}
	return testutil.Call(test, testHandler.CreateIssue, request)
}

func holdMarker(test *testing.T, issueID string) string {
	test.Helper()
	var value string
	dbfx.QueryRow(test, `SELECT COALESCE(metadata ->> 'intake_hold', '') FROM issue WHERE id = $1`, issueID).Scan(&value)
	return value
}

func TestIntakeHoldCreate(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, func(entry map[string]any) { entry["exempt_types"] = []string{"fix"} })
	logged := captureHoldLog(test)

	test.Run("an agent create with status todo is refused", func(test *testing.T) {
		wantIntakeRefusal(test, holdCreateAs(test, map[string]any{"title": "plan(x): todo", "project_id": fixture.projectID, "status": "todo"}, true))
	})
	test.Run("the same with an explicit assignee is refused", func(test *testing.T) {
		wantIntakeRefusal(test, holdCreateAs(test, map[string]any{
			"title": "feat(x): assigned", "project_id": fixture.projectID, "status": "todo",
			"assignee_type": "agent", "assignee_id": handlerTestAgentID(test),
		}, true))
	})
	test.Run("a create with no status lands in backlog with the marker", func(test *testing.T) {
		response := holdCreateAs(test, map[string]any{"title": "plan(x): no status", "project_id": fixture.projectID}, true).Want(http.StatusCreated)
		issueID := response.Map()["id"].(string)
		if got := holdIssueStatus(test, issueID); got != "backlog" {
			test.Fatalf("status = %s, want backlog", got)
		}
		if got := holdMarker(test, issueID); got != "CHE-6911" {
			test.Fatalf("marker = %q, want the entry source", got)
		}
		wantHold(test, issueID, true, true, "held")
	})
	test.Run("status backlog is created with the marker", func(test *testing.T) {
		response := holdCreateAs(test, map[string]any{"title": "feat(x): backlog", "project_id": fixture.projectID, "status": "backlog"}, true).Want(http.StatusCreated)
		if got := holdMarker(test, response.Map()["id"].(string)); got != "CHE-6911" {
			test.Fatalf("marker = %q, want the entry source", got)
		}
	})
	test.Run("an exempt type is not held and not marked", func(test *testing.T) {
		response := holdCreateAs(test, map[string]any{"title": "fix(x): bug", "project_id": fixture.projectID, "status": "todo"}, true)
		response.Want(http.StatusCreated)
		issueID := response.Map()["id"].(string)
		if got := holdIssueStatus(test, issueID); got != "todo" || holdMarker(test, issueID) != "" {
			test.Fatalf("status = %s marker = %q, want todo and no marker", got, holdMarker(test, issueID))
		}
	})
	test.Run("a person's create is not held", func(test *testing.T) {
		response := holdCreateAs(test, map[string]any{"title": "feat(x): by a person", "project_id": fixture.projectID, "status": "todo"}, false)
		response.Want(http.StatusCreated)
		if got := holdMarker(test, response.Map()["id"].(string)); got != "" {
			test.Fatalf("marker = %q, want none", got)
		}
	})
	test.Run("a child issue is not held", func(test *testing.T) {
		parent := fixture.issue(test, "feat(x): parent", testutil.Cols{"status": "in_progress"})
		response := holdCreateAs(test, map[string]any{"title": "feat(x): child", "project_id": fixture.projectID, "status": "todo", "parent_issue_id": parent}, true)
		response.Want(http.StatusCreated)
		if got := holdMarker(test, response.Map()["id"].(string)); got != "" {
			test.Fatalf("marker = %q, want none", got)
		}
	})
	if !strings.Contains(logged.String(), "intake_hold_exempt") {
		test.Fatalf("no intake_hold_exempt audit line in %q", logged.String())
	}
}

// 8 (a): a person's move succeeds, then agents may move the issue, and a later
// return to backlog does not hold it again.
func TestIntakeHoldPersonMoveReleases(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, nil)
	issueID := fixture.issue(test, "feat(x): wait")
	wantIntakeRefusal(test, holdAgentPut(test, issueID, "in_progress"))

	putIssue(test, issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)
	holdAgentPut(test, issueID, "in_progress").Want(http.StatusOK)
	holdAgentPut(test, issueID, "backlog").Want(http.StatusOK)
	wantHold(test, issueID, false, false, "released_by_member")
	holdAgentPut(test, issueID, "in_progress").Want(http.StatusOK)
}

// 8 (c): a machine credential is never a person, even when it resolves to a member.
func TestIntakeHoldMachineCredentialsAreRefused(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, nil)
	asCloudPAT := asSource("cloud_pat")

	test.Run("from backlog", func(test *testing.T) {
		issueID := fixture.issue(test, "feat(x): pat")
		wantIntakeRefusal(test, putIssue(test, issueID, map[string]any{"status": "todo"}, asCloudPAT))
		wantIntakeRefusal(test, holdAgentPut(test, issueID, "todo"))
		wantHold(test, issueID, true, true, "held")
		if holdStatusRows(test, issueID) != 0 {
			test.Fatalf("a refused move wrote a status row")
		}
	})
	test.Run("from the parked state", func(test *testing.T) {
		issueID := fixture.issue(test, "feat(x): parked pat")
		holdPark(test, issueID)
		rows := holdStatusRows(test, issueID)
		wantIntakeRefusal(test, putIssue(test, issueID, map[string]any{"status": "in_progress"}, asCloudPAT))
		wantIntakeRefusal(test, holdAgentPut(test, issueID, "in_progress"))
		wantHold(test, issueID, true, true, "held")
		if got := holdStatusRows(test, issueID); got != rows {
			test.Fatalf("status rows = %d, want %d", got, rows)
		}
	})
}

// holdPark is the platform's own park: the issue goes to need_decision
// and the park row remembers backlog.
func holdPark(test *testing.T, issueID string) {
	test.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		test.Fatal(err)
	}
	parked, _, err := parkIssueForDecision(context.Background(), testHandler.Queries, issue)
	if err != nil || parked.Status == "backlog" {
		test.Fatalf("park: status %q, err %v", parked.Status, err)
	}
}

// 8 (d): a cloud-PAT move made before the entry was stored is no release.
func TestIntakeHoldMachineMoveBeforeEntryIsNoRelease(test *testing.T) {
	seedTestCatalog(test)
	projectID := dbfx.Project(test, "intake hold before entry")
	issueID := dbfx.Issue(test, "feat(x): early", testutil.Cols{"status": "backlog", "project_id": projectID})
	putIssue(test, issueID, map[string]any{"status": "todo"}, asSource("cloud_pat")).Want(http.StatusOK)

	now := time.Now().UTC()
	newHoldFixtureWith(test, holdFixture{projectID, map[string]any{
		"project_id": projectID, "mode": "block", "source": "CHE-6911",
		"starts_at": now.Add(-time.Hour).Format(time.RFC3339), "ends_at": now.Add(time.Hour).Format(time.RFC3339),
	}}, func(map[string]any) {})
	holdAgentPut(test, issueID, "backlog").Want(http.StatusOK)
	wantHold(test, issueID, true, true, "held")
	wantIntakeRefusal(test, holdAgentPut(test, issueID, "in_progress"))
}

// 8 (e): a person's answer holds the issue until its wake_at.
func TestIntakeHoldAnswerHoldWithoutPark(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, func(entry map[string]any) { entry["exempt_escalation_classes"] = []string{"spend"} })

	test.Run("the move passes after wake_at", func(test *testing.T) {
		issueID := fixture.issue(test, "feat(x): paid")
		escalationAnswer(test, issueID, "spend", "now() + interval '10 minutes'")
		wantIntakeRefusal(test, holdAgentPut(test, issueID, "in_progress"))
		dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, issueID)
		holdAgentPut(test, issueID, "in_progress").Want(http.StatusOK)
	})
	test.Run("a withdrawn answer never passes", func(test *testing.T) {
		issueID := fixture.issue(test, "feat(x): withdrawn")
		escalationAnswer(test, issueID, "spend", "now() + interval '10 minutes'")
		dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = now(), wake_at = NULL WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, issueID)
		wantIntakeRefusal(test, holdAgentPut(test, issueID, "in_progress"))
		dbfx.Exec(test, `UPDATE decision_request SET created_at = now() - interval '1 hour' WHERE issue_id = $1`, issueID)
		wantIntakeRefusal(test, holdAgentPut(test, issueID, "todo"))
	})
}

// 8 (f): what the hold does not refuse.
func TestIntakeHoldNotRefused(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, nil)

	test.Run("a move to cancelled", func(test *testing.T) {
		issueID := fixture.issue(test, "feat(x): drop")
		holdAgentPut(test, issueID, "cancelled").Want(http.StatusOK)
	})
	test.Run("an issue outside backlog before the entry was stored", func(test *testing.T) {
		issueID := fixture.issue(test, "feat(x): running", testutil.Cols{"status": "in_progress"})
		holdAgentPut(test, issueID, "todo").Want(http.StatusOK)
	})
	test.Run("a platform park and unpark", func(test *testing.T) {
		issueID := fixture.issue(test, "feat(x): card")
		holdPark(test, issueID)
		wantIntakeRefusal(test, holdAgentPut(test, issueID, "in_progress"))
		issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
		if err != nil {
			test.Fatal(err)
		}
		if back, _, err := unparkIssueAfterDecision(context.Background(), testHandler.Queries, issue, nil); err != nil || back.Status != "backlog" {
			test.Fatalf("unpark: status %q, err %v", back.Status, err)
		}
	})
	test.Run("an issue with a parent", func(test *testing.T) {
		parent := fixture.issue(test, "feat(x): parent", testutil.Cols{"status": "in_progress"})
		child := fixture.issue(test, "feat(x): child", testutil.Cols{"parent_issue_id": parent})
		holdAgentPut(test, child, "in_progress").Want(http.StatusOK)
	})
}

// Mode warn writes one line and refuses nothing.
func TestIntakeHoldWarnMode(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, func(entry map[string]any) { entry["mode"] = "warn" })
	logged := captureHoldLog(test)
	issueID := fixture.issue(test, "feat(x): warn")
	holdAgentPut(test, issueID, "in_progress").Want(http.StatusOK)
	response := holdCreateAs(test, map[string]any{"title": "feat(x): warn create", "project_id": fixture.projectID, "status": "todo"}, true).Want(http.StatusCreated)
	if got := holdIssueStatus(test, response.Map()["id"].(string)); got != "todo" {
		test.Fatalf("a warn-mode create landed in %s", got)
	}
	if strings.Count(logged.String(), "intake_hold_warn") < 2 {
		test.Fatalf("want an intake_hold_warn line for the move and the create, got %q", logged.String())
	}
	if strings.Contains(logged.String(), "msg=intake_hold ") {
		test.Fatalf("warn mode wrote a refusal line: %q", logged.String())
	}
}

// 8 (a), the run: a person's move into a step with a handler queues the
// handler's run; an agent could not make that move before it.
func TestIntakeHoldPersonMoveStartsHandlerRun(test *testing.T) {
	seedTestCatalog(test)
	agentID := seededReadyAgentID(test)
	fixture := newHoldFixture(test, nil)
	setColumnHandoff(test, fixture.projectID, "in_progress", handToAgent(agentID, "Go.")).Want(http.StatusOK)
	issueID := fixture.issue(test, "feat(x): handled")

	wantIntakeRefusal(test, holdAgentPut(test, issueID, "in_progress"))
	if queued := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, issueID); queued != 0 {
		test.Fatalf("a refused move queued %d runs", queued)
	}
	putIssue(test, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	if queued := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, issueID, agentID); queued != 1 {
		test.Fatalf("queued handler runs after the person's move = %d, want 1", queued)
	}
	holdAgentPut(test, issueID, "backlog").Want(http.StatusOK)
	wantHold(test, issueID, false, false, "released_by_member")
	holdAgentPut(test, issueID, "in_progress").Want(http.StatusOK)
}

// 7: in a workflow whose todo step has no handler (as Design's), an agent
// create with status todo is refused, and one with no status lands in backlog.
func TestIntakeHoldCreateInWorkflowWithoutHandler(test *testing.T) {
	seedTestCatalog(test)
	fixture := newHoldFixture(test, nil)
	workflow := createTestWorkflow(test, "hold handlerless", "todo", "in_progress", "in_review")
	setProjectWorkflow(test, fixture.projectID, map[string]any{"workflow_id": workflow.ID}).Want(http.StatusOK)

	wantIntakeRefusal(test, holdCreateAs(test, map[string]any{"title": "plan(x): design", "project_id": fixture.projectID, "status": "todo"}, true))
	response := holdCreateAs(test, map[string]any{"title": "plan(x): design no status", "project_id": fixture.projectID}, true).Want(http.StatusCreated)
	issueID := response.Map()["id"].(string)
	if got := holdIssueStatus(test, issueID); got != "backlog" || holdMarker(test, issueID) != "CHE-6911" {
		test.Fatalf("status = %s marker = %q, want backlog and the entry source", got, holdMarker(test, issueID))
	}
}

// 8 (e), through the real request and answer path: a DM task sends a spend
// request on a held issue. A DM request does not park the issue, so it stays
// in backlog. The person answers the card; the agent move passes only once the
// answer's wake_at has passed, and never after a withdrawal.
func TestIntakeHoldAnswerHoldRealPath(test *testing.T) {
	seedTestCatalog(test)
	hold := newHoldFixture(test, func(entry map[string]any) { entry["exempt_escalation_classes"] = []string{"spend"} })
	for _, withdraw := range []bool{false, true} {
		name := "the answer takes effect at wake_at"
		if withdraw {
			name = "a withdrawn answer never passes"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newDMDecisionFixture(test)
			dbfx.Exec(test, `UPDATE issue SET project_id = $1, status = 'backlog', created_at = now(), assignee_type = NULL, assignee_id = NULL WHERE id = $2`, hold.projectID, fixture.issueID)
			body := fixture.dmBody()
			body["escalation_class"] = "spend"
			var view dmDecisionView
			testutil.Call(test, testHandler.RequestDecision, fixture.request(body)).Want(http.StatusCreated).JSON(&view)
			if got := holdIssueStatus(test, fixture.issueID); got != "backlog" {
				test.Fatalf("the DM request moved the issue to %s, want it to stay in backlog", got)
			}
			if parks := dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1`, fixture.issueID); parks != 0 {
				test.Fatalf("the DM request wrote %d park rows, want 0", parks)
			}
			wantIntakeRefusal(test, holdAgentPut(test, fixture.issueID, "in_progress"))

			issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
			if err != nil {
				test.Fatal(err)
			}
			request := fixture.request(body)
			answer, err := testHandler.answerDecisionRequestTx(request, issue.WorkspaceID, parseUUID(view.ID), parseUUID(fixture.humanID), decisionCardAnswerInput{Text: "yes"})
			if err != nil {
				test.Fatal(err)
			}
			if !answer.Answer.WakeAt.Valid {
				test.Fatal("the card answer has no wake_at")
			}
			wantIntakeRefusal(test, holdAgentPut(test, fixture.issueID, "in_progress"))
			if withdraw {
				if _, err := testHandler.disclaimDecisionAnswerTx(request, issue.WorkspaceID, answer.Answer.ID, parseUUID(fixture.humanID)); err != nil {
					test.Fatal(err)
				}
				dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() - interval '1 minute' WHERE id = $1`, answer.Answer.ID)
				wantIntakeRefusal(test, holdAgentPut(test, fixture.issueID, "in_progress"))
				return
			}
			dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, answer.Answer.ID)
			holdAgentPut(test, fixture.issueID, "in_progress").Want(http.StatusOK)
		})
	}
}

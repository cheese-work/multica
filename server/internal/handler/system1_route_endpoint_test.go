package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4551 (S3 of CHE-4439): POST /api/issues/{id}/route, status only.

type routeCase struct {
	issueID, projectID, handlerID, runtimeID string
}

// newRouteCase is an in_progress issue in a project whose workflow hands
// in_review to the handler agent.
func newRouteCase(t *testing.T) routeCase {
	t.Helper()
	seedTestCatalog(t)
	handlerID := seededReadyAgentID(t)
	c := routeCase{handlerID: handlerID}
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, handlerID).Scan(&c.runtimeID)
	c.projectID = createWorkflowTestProject(t, "Route endpoint project "+workflowTestSuffix())
	setColumnHandoff(t, c.projectID, "in_review", handToAgent(handlerID, "Review it.")).Want(http.StatusOK)
	c.issueID = dbfx.Issue(t, "route endpoint", testutil.Cols{"project_id": c.projectID, "status": "in_progress"})
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM system1_observation WHERE workspace_id = $1 AND subject_id = $2`, testWorkspaceID, c.issueID)
		dbfx.Exec(t, `DELETE FROM activity_log WHERE issue_id = $1`, c.issueID)
	})
	return c
}

func routeBody(status string, over map[string]any) map[string]any {
	body := map[string]any{"status": status, "reason": "ready for review", "confidence": 0.8}
	for k, v := range over {
		body[k] = v
	}
	return body
}

// route is the member's call; headers turn it into an agent's.
func (c routeCase) route(t *testing.T, body map[string]any, headers map[string]string) *testutil.Response {
	t.Helper()
	req := withURLParam(workflowRequest("POST", "/api/issues/"+c.issueID+"/route", body), "id", c.issueID)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return testutil.Call(t, testHandler.RouteIssue, req)
}

// agentRun plants a running run of agentID on the issue at step and returns
// the headers an agent call from inside it carries.
func (c routeCase) agentRun(t *testing.T, agentID, step string) map[string]string {
	t.Helper()
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": c.issueID, "runtime_id": c.runtimeID, "status": "running", "workflow_step": step,
	})
	return map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": agentID, "X-Task-ID": taskID}
}

func (c routeCase) otherAgent(t *testing.T) string {
	t.Helper()
	return dbfx.Agent(t, "route other agent "+workflowTestSuffix(), c.runtimeID)
}

func (c routeCase) routeRows(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID)
}

func (c routeCase) activityRows(t *testing.T, action string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = $2`, c.issueID, action)
}

// wantNothingWritten: the issue stays put, with no label, no activity row for
// the route and no run for the handler.
func (c routeCase) wantNothingWritten(t *testing.T, wantStatus string) {
	t.Helper()
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != wantStatus {
		t.Fatalf("status = %q, want %q", status, wantStatus)
	}
	if n := c.routeRows(t); n != 0 {
		t.Fatalf("route rows = %d, want 0", n)
	}
	if n := c.activityRows(t, "routed"); n != 0 {
		t.Fatalf("routed activity rows = %d, want 0", n)
	}
	if n := c.activityRows(t, "status_changed"); n != 0 {
		t.Fatalf("status_changed activity rows = %d, want 0", n)
	}
	if runs := pendingStepRuns(t, c.issueID, c.handlerID); len(runs) != 0 {
		t.Fatalf("pending handler runs = %v, want none", runs)
	}
}

type routeRun struct {
	Started bool   `json:"started"`
	Refused string `json:"refused"`
	Reason  string `json:"reason"`
}

type routeResponse struct {
	ObservationID string   `json:"observation_id"`
	EventID       string   `json:"event_id"`
	Run           routeRun `json:"run"`
}

func TestRouteEndpointMovesTheIssueAndWritesOneLabel(t *testing.T) {
	c := newRouteCase(t)

	resp := c.route(t, routeBody("in_review", nil), nil).Want(http.StatusOK)
	raw := resp.Body.String()
	var out routeResponse
	resp.JSON(&out)

	if status, _, assignee := issueStatusAndAssignee(t, c.issueID); status != "in_review" || assignee != c.handlerID {
		t.Fatalf("issue = %q / %q, want in_review handed to %s", status, assignee, c.handlerID)
	}
	if steps := pendingStepRuns(t, c.issueID, c.handlerID); len(steps) != 1 || steps[0] != "in_review" {
		t.Fatalf("handler runs = %v, want one stamped in_review", steps)
	}
	if !out.Run.Started || out.Run.Refused != "" {
		t.Fatalf("run = %+v, want started", out.Run)
	}
	if n := c.routeRows(t); n != 1 {
		t.Fatalf("route rows = %d, want 1", n)
	}
	var obsID, obsEvent, via, source, observed string
	dbfx.QueryRow(t, `SELECT id::text, event_id::text, inputs->>'via', decision_source, inputs->>'observed_status' FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID).
		Scan(&obsID, &obsEvent, &via, &source, &observed)
	var statusChangedID string
	dbfx.QueryRow(t, `SELECT id::text FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, c.issueID).Scan(&statusChangedID)
	if via != "route" || source != "human" || observed != "in_progress" {
		t.Fatalf("label via/source/observed = %s/%s/%s, want route/human/in_progress", via, source, observed)
	}
	if obsEvent != statusChangedID || out.EventID != statusChangedID || out.ObservationID != obsID {
		t.Fatalf("ids: label event %s, response event %s / observation %s, status_changed %s, label %s",
			obsEvent, out.EventID, out.ObservationID, statusChangedID, obsID)
	}
	var reason, event, confidence string
	dbfx.QueryRow(t, `SELECT details->>'reason', details->>'event_id', details->>'confidence' FROM activity_log WHERE issue_id = $1 AND action = 'routed'`, c.issueID).
		Scan(&reason, &event, &confidence)
	if reason != "ready for review" || event != statusChangedID || confidence != "0.8" {
		t.Fatalf("routed activity = %q / %q / %q", reason, event, confidence)
	}
	if strings.Contains(strings.ToLower(raw), "predict") {
		t.Fatalf("response leaks the prediction: %s", raw)
	}
}

func TestRouteEndpointRecordsAnAgentCallerAsAgentDecision(t *testing.T) {
	c := newRouteCase(t)
	caller := c.otherAgent(t)

	headers := c.agentRun(t, caller, "in_progress")
	c.route(t, routeBody("in_review", nil), headers).Want(http.StatusOK)

	var source, taskID string
	dbfx.QueryRow(t, `SELECT decision_source, COALESCE(inputs->>'task_id', '') FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID).Scan(&source, &taskID)
	if source != "agent" || taskID != headers["X-Task-ID"] {
		t.Fatalf("label source/task = %s/%s, want agent/%s", source, taskID, headers["X-Task-ID"])
	}
}

func TestRouteEndpointRefusesAnExitTheStepDoesNotAllow(t *testing.T) {
	c := newRouteCase(t)
	setColumnHandoff(t, c.projectID, "in_progress", map[string]any{
		"handler": map[string]any{"type": "agent", "id": c.handlerID}, "instructions": "Implement it.", "next_status_key": "in_review",
	}).Want(http.StatusOK)
	headers := c.agentRun(t, c.handlerID, "in_progress")

	body := c.route(t, routeBody("done", nil), headers).Want(http.StatusBadRequest).Map()
	if body["code"] != "status_move_not_allowed" {
		t.Fatalf("refusal = %v", body)
	}
	c.wantNothingWritten(t, "in_progress")
}

func TestRouteEndpointRefusesAStaleReviewVerdict(t *testing.T) {
	f := reviewIssue(t)
	pushHead(t, f.prID, reviewHeadB)
	c := routeCase{issueID: f.issueID, handlerID: f.agentID, runtimeID: f.runtimeID}
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM system1_observation WHERE workspace_id = $1 AND subject_id = $2`, testWorkspaceID, c.issueID)
	})

	body := c.route(t, routeBody("blocked", nil), map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": f.agentID, "X-Task-ID": f.review}).
		Want(http.StatusConflict).Map()
	if body["code"] != "review_verdict_stale" {
		t.Fatalf("refusal = %v", body)
	}
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "in_review" {
		t.Fatalf("status = %q, the refused verdict must not move the issue", status)
	}
	if n := c.routeRows(t); n != 0 {
		t.Fatalf("route rows = %d, want 0", n)
	}
}

func TestRouteEndpointRefusesARunWhoseStepMoved(t *testing.T) {
	c := newRouteCase(t)
	headers := c.agentRun(t, c.handlerID, "in_progress")
	// A member parks the issue while the run is going; handler tests run
	// without the listener that records the activity row.
	updateIssueForTest(t, c.issueID, map[string]any{"status": "blocked"}).Want(http.StatusOK)
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'member', $3, 'status_changed', '{"from":"in_progress","to":"blocked"}')`, testWorkspaceID, c.issueID, testUserID)
	before := c.activityRows(t, "status_changed")

	body := c.route(t, routeBody("in_review", nil), headers).Want(http.StatusConflict).Map()
	if body["code"] != "workflow_step_moved" {
		t.Fatalf("refusal = %v", body)
	}
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "blocked" {
		t.Fatalf("status = %q, the refused move must not apply", status)
	}
	if n := c.routeRows(t); n != 0 || c.activityRows(t, "status_changed") != before || c.activityRows(t, "routed") != 0 {
		t.Fatalf("a refused route left rows: labels %d", n)
	}
}

// The workflow's handler is the owner this route writes. A caller who may not
// invoke it is refused before anything is captured or written.
func TestRouteEndpointRefusesAHandlerTheCallerCannotInvoke(t *testing.T) {
	c := newRouteCase(t)
	_, ownerID, memberID := privateAgentTestFixture(t)
	private := dbfx.Agent(t, "route private handler "+workflowTestSuffix(), c.runtimeID)
	setColumnHandoff(t, c.projectID, "in_review", handToAgent(private, "Review it.")).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE agent SET visibility = 'private', owner_id = $2 WHERE id = $1`, private, ownerID)
	c.handlerID = private

	req := withURLParam(workflowRequest("POST", "/api/issues/"+c.issueID+"/route", routeBody("in_review", nil)), "id", c.issueID)
	req.Header.Set("X-User-ID", memberID)
	resp := testutil.Call(t, testHandler.RouteIssue, req).Want(http.StatusForbidden)
	assertDenialReason(t, resp, "you do not have permission to assign work to this agent")

	c.wantNothingWritten(t, "in_progress")
}

func TestRouteEndpointRefusesAStaleRevision(t *testing.T) {
	c := newRouteCase(t)
	dbfx.Exec(t, `UPDATE issue SET revision = revision + 2 WHERE id = $1`, c.issueID)
	var revision int64
	dbfx.QueryRow(t, `SELECT revision FROM issue WHERE id = $1`, c.issueID).Scan(&revision)

	body := c.route(t, routeBody("in_review", map[string]any{"expected_revision": revision - 1}), nil).Want(http.StatusConflict).Map()
	if body["code"] != "revision_conflict" {
		t.Fatalf("refusal = %v", body)
	}
	c.wantNothingWritten(t, "in_progress")

	c.route(t, routeBody("in_review", map[string]any{"expected_revision": revision}), nil).Want(http.StatusOK)
}

// A refused run does not undo the routing decision: the move and the label
// commit, and the response says why no run started.
func TestRouteEndpointCommitsTheMoveWhenTheRunIsRefused(t *testing.T) {
	cases := []struct {
		name, refused string
		setup         func(t *testing.T, c routeCase)
		asAgent       bool
	}{
		{"run budget exhausted", "run_budget_exhausted", func(t *testing.T, c routeCase) {
			dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 1, run_budget_since = now() - interval '1 hour' WHERE id = $1`, projectWorkflowID(t, c.projectID))
			// More spent runs than the budget plus the grant an agent's first entry into a status earns.
			for range 1 + service.RunBudgetGrantRuns + 1 {
				dbfx.Task(t, c.handlerID, testutil.Cols{
					"issue_id": c.issueID, "runtime_id": c.runtimeID, "status": "completed", "workflow_step": "in_progress",
					"completed_at": testutil.Raw("now()"),
				})
			}
		}, true},
		{"a run already holds the slot starts no second run", "", func(t *testing.T, c routeCase) {
			dbfx.Task(t, c.handlerID, testutil.Cols{"issue_id": c.issueID, "runtime_id": c.runtimeID, "status": "queued"})
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newRouteCase(t)
			tc.setup(t, c)

			// A person's move opens a fresh budget window, so only an agent's is refused by it.
			var headers map[string]string
			if tc.asAgent {
				headers = c.agentRun(t, c.otherAgent(t), "in_progress")
			}
			var out routeResponse
			c.route(t, routeBody("in_review", nil), headers).Want(http.StatusOK).JSON(&out)

			if out.Run.Started || out.Run.Refused != tc.refused {
				t.Fatalf("run = %+v, want not started, refusal %q", out.Run, tc.refused)
			}
			if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "in_review" {
				t.Fatalf("status = %q, the move must commit", status)
			}
			if n := c.routeRows(t); n != 1 {
				t.Fatalf("route rows = %d, want 1", n)
			}
			if out.ObservationID == "" || out.EventID == "" {
				t.Fatalf("response ids = %+v", out)
			}
		})
	}
}

// Any other failure after the write takes the whole route back.
func TestRouteEndpointRollsBackWhenTheStopIsNotConfirmed(t *testing.T) {
	c := newRouteCase(t)
	supersededRun(t, c.handlerID, c.runtimeID, c.issueID, "in_review")

	body := c.route(t, routeBody("in_review", nil), nil).Want(http.StatusConflict).Map()
	if body["code"] != "stop_not_confirmed" {
		t.Fatalf("refusal = %v", body)
	}
	c.wantNothingWritten(t, "in_progress")
}

func TestRouteEndpointNoStartSkipsTheRunOfADifferentHandler(t *testing.T) {
	c := newRouteCase(t)
	var out routeResponse
	c.route(t, routeBody("in_review", map[string]any{"suppress_run": true}), nil).Want(http.StatusOK).JSON(&out)

	if out.Run.Started {
		t.Fatalf("run = %+v, want no run", out.Run)
	}
	if steps := pendingStepRuns(t, c.issueID, c.handlerID); len(steps) != 0 {
		t.Fatalf("handler runs = %v, want none", steps)
	}
}

func TestRouteEndpointNoStartSkipsTheRunOfTheCaller(t *testing.T) {
	c := newRouteCase(t)
	headers := c.agentRun(t, c.handlerID, "in_progress")

	var out routeResponse
	c.route(t, routeBody("in_review", map[string]any{"suppress_run": true}), headers).Want(http.StatusOK).JSON(&out)

	if out.Run.Started {
		t.Fatalf("run = %+v, want none: the caller is the handler", out.Run)
	}
	if steps := pendingStepRuns(t, c.issueID, c.handlerID); len(steps) != 0 {
		t.Fatalf("handler runs = %v, want none", steps)
	}
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "in_review" {
		t.Fatalf("status = %q, want in_review", status)
	}
	if n := c.routeRows(t); n != 1 {
		t.Fatalf("route rows = %d, want 1", n)
	}
}

func TestRouteEndpointRejectsABadRequest(t *testing.T) {
	c := newRouteCase(t)
	cases := map[string]map[string]any{
		"no status":          routeBody("", nil),
		"no reason":          routeBody("in_review", map[string]any{"reason": ""}),
		"reason too long":    routeBody("in_review", map[string]any{"reason": strings.Repeat("r", 501)}),
		"no confidence":      {"status": "in_review", "reason": "why"},
		"confidence over 1":  routeBody("in_review", map[string]any{"confidence": 1.01}),
		"confidence below 0": routeBody("in_review", map[string]any{"confidence": -0.1}),
		"unknown status":     routeBody("no_such_status", nil),
		"zero revision":      routeBody("in_review", map[string]any{"expected_revision": 0}),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c.route(t, body, nil).Want(http.StatusBadRequest)
			c.wantNothingWritten(t, "in_progress")
		})
	}
}

func TestRouteEndpointRefusesANoOp(t *testing.T) {
	c := newRouteCase(t)
	c.route(t, routeBody("in_progress", nil), nil).Want(http.StatusConflict)
	c.wantNothingWritten(t, "in_progress")
}

// A squad's leader runs on the authority of the member who routed, not on the
// issue creator's: a member who cannot invoke the leader starts no run by
// routing an issue its creator could have started.
func TestRouteEndpointJudgesTheSquadLeaderByTheRequestingMember(t *testing.T) {
	c := newRouteCase(t)
	leader, ownerID, memberID := privateAgentTestFixture(t)
	squadID := createSquadInWorkspace(t, ownerID, testWorkspaceID, "route squad "+workflowTestSuffix(), leader)
	dbfx.Exec(t, `UPDATE issue SET status = 'backlog', creator_type = 'member', creator_id = $2, assignee_type = 'squad', assignee_id = $3 WHERE id = $1`, c.issueID, ownerID, squadID)

	got := c.route(t, routeBody("in_progress", nil), map[string]string{"X-User-ID": memberID}).Want(http.StatusOK)
	var resp routeResponse
	got.JSON(&resp)
	if resp.Run.Started || resp.Run.Refused != "squad_leader_not_admitted" {
		t.Fatalf("run = %+v, want refused squad_leader_not_admitted: %s", resp.Run, got.Text())
	}
	if runs := pendingStepRuns(t, c.issueID, leader); len(runs) != 0 {
		t.Fatalf("pending leader runs = %v, want none", runs)
	}
}

// A run that carries taint cannot route work into an untainted run: the run
// the move starts inherits the caller's taint.
func TestRouteEndpointPassesTheCallersTaintToTheRunItStarts(t *testing.T) {
	c := newRouteCase(t)
	headers := c.agentRun(t, c.otherAgent(t), "in_progress")
	dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['intake-1'] WHERE id = $1`, headers["X-Task-ID"])

	c.route(t, routeBody("in_review", nil), headers).Want(http.StatusOK)

	var taint []string
	dbfx.QueryRow(t, `SELECT COALESCE(taint_ids, '{}') FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched')`, c.issueID, c.handlerID).Scan(&taint)
	if len(taint) != 1 || taint[0] != "intake-1" {
		t.Fatalf("started run taint = %v, want [intake-1]", taint)
	}
}

// The reply is a read of the issue: an elevated run gets a stub, not the text,
// of a description an external source may have written.
func TestRouteEndpointStubsAMarkedDescriptionForAnElevatedRun(t *testing.T) {
	c := newRouteCase(t)
	f := claimElevated(t, "multica_cross_issue")
	dbfx.Exec(t, `UPDATE issue SET description = $2, description_external_read = true WHERE id = $1`, c.issueID, hostileBody)

	req := withURLParam(workflowRequest("POST", "/api/issues/"+c.issueID+"/route", routeBody("in_review", nil)), "id", c.issueID)
	res := testutil.Call(t, testHandler.RouteIssue, testutil.WithHeaders(asElevatedToken(req, f.taskID), "X-Agent-ID", c.otherAgent(t)))
	if res.Code != http.StatusOK {
		t.Fatalf("route status = %d: %s", res.Code, res.Text())
	}
	if strings.Contains(res.Text(), hostileBody) {
		t.Fatalf("route reply leaks a marked description (status %d): %s", res.Code, res.Text())
	}
}

// Promoting a backlog issue that sits on another member's private agent starts
// no run for a member who cannot invoke that agent, handoff or not.
func TestRouteEndpointJudgesTheDirectAgentByTheRequestingMember(t *testing.T) {
	c := newRouteCase(t)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	dbfx.Exec(t, `UPDATE issue SET status = 'backlog', creator_type = 'member', creator_id = $2, assignee_type = 'agent', assignee_id = $3 WHERE id = $1`, c.issueID, ownerID, agentID)

	got := c.route(t, routeBody("in_progress", nil), map[string]string{"X-User-ID": memberID}).Want(http.StatusOK)
	var resp routeResponse
	got.JSON(&resp)
	if resp.Run.Started || resp.Run.Refused != "invocation_not_allowed" {
		t.Fatalf("run = %+v, want refused invocation_not_allowed: %s", resp.Run, got.Text())
	}
	if runs := pendingStepRuns(t, c.issueID, agentID); len(runs) != 0 {
		t.Fatalf("pending runs = %v, want none", runs)
	}
}

// The owner of the private agent still starts the run by the same move.
func TestRouteEndpointStartsTheDirectAgentRunForItsOwner(t *testing.T) {
	c := newRouteCase(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	dbfx.Exec(t, `UPDATE issue SET status = 'backlog', creator_type = 'member', creator_id = $2, assignee_type = 'agent', assignee_id = $3 WHERE id = $1`, c.issueID, ownerID, agentID)

	got := c.route(t, routeBody("in_progress", nil), map[string]string{"X-User-ID": ownerID}).Want(http.StatusOK)
	var resp routeResponse
	got.JSON(&resp)
	if !resp.Run.Started {
		t.Fatalf("run = %+v, want started: %s", resp.Run, got.Text())
	}
}

// A member's edit that lands between the route's snapshot read and the issue
// lock must survive the move: the route writes the status, not the snapshot.
func TestRouteEndpointKeepsAnEditThatLandsBeforeTheLock(t *testing.T) {
	c := newRouteCase(t)
	t.Cleanup(func() { routeLockGapHook = func() {} })
	routeLockGapHook = func() {
		dbfx.Exec(t, `UPDATE issue SET due_date = '2031-02-03', stage = 7, revision = revision + 1 WHERE id = $1`, c.issueID)
	}

	c.route(t, routeBody("in_review", nil), nil).Want(http.StatusOK)

	var due string
	var stage int
	dbfx.QueryRow(t, `SELECT COALESCE(due_date::text, ''), COALESCE(stage, 0) FROM issue WHERE id = $1`, c.issueID).Scan(&due, &stage)
	if due != "2031-02-03" || stage != 7 {
		t.Fatalf("due_date/stage = %q/%d after the move, want the concurrent edit 2031-02-03/7", due, stage)
	}
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "in_review" {
		t.Fatalf("status = %q, want in_review", status)
	}
}

// The workflow changing between the snapshot and the lock refuses the call and
// writes nothing.
func TestRouteEndpointRefusesAHandlerChangedUnderTheLock(t *testing.T) {
	c := newRouteCase(t)
	t.Cleanup(func() { routeLockGapHook = func() {} })
	other := c.otherAgent(t)
	routeLockGapHook = func() {
		setColumnHandoff(t, c.projectID, "in_review", handToAgent(other, "Review it.")).Want(http.StatusOK)
	}

	body := c.route(t, routeBody("in_review", nil), nil).Want(http.StatusConflict).Map()
	if body["code"] != "workflow_changed" {
		t.Fatalf("refusal = %v", body)
	}
	c.wantNothingWritten(t, "in_progress")
}

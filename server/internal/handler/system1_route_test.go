package handler

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-4548 (S1 of CHE-4439): route observation rows and the routing_exit mark.

func TestRoutingExitPredicate(t *testing.T) {
	cfg := service.RoutingAgentConfig{
		AgentID:   "router",
		Statuses:  []string{"todo", "blocked"},
		Overrides: map[string]string{service.OverrideKey("wf", "blocked"): ""},
	}
	exit := routingExitInput{AgentRun: true, RunStep: "todo", From: "todo", To: "in_progress"}
	cases := []struct {
		name  string
		scope string
		edit  func(*routingExitInput)
		want  bool
	}{
		{"routing step, agent run on the step", "wf", func(*routingExitInput) {}, true},
		{"non-routing step", "wf", func(in *routingExitInput) { in.RunStep, in.From = "in_progress", "in_progress" }, false},
		{"workflow override maps the step to empty", "wf", func(in *routingExitInput) { in.RunStep, in.From = "blocked", "blocked" }, false},
		{"same step without that workflow scope is still a routing step", "", func(in *routingExitInput) { in.RunStep, in.From = "blocked", "blocked" }, true},
		{"run step differs from the locked from-status", "wf", func(in *routingExitInput) { in.RunStep = "blocked" }, false},
		{"run has no step", "wf", func(in *routingExitInput) { in.RunStep = "" }, false},
		{"status unchanged", "wf", func(in *routingExitInput) { in.To = "todo" }, false},
		{"caller is not an agent run", "wf", func(in *routingExitInput) { in.AgentRun = false }, false},
	}
	for _, tc := range cases {
		in := exit
		tc.edit(&in)
		if got := routingExit(cfg, tc.scope, in); got != tc.want {
			t.Errorf("%s: routingExit = %v, want %v", tc.name, got, tc.want)
		}
	}
	if routingExit(service.RoutingAgentConfig{}, "wf", exit) {
		t.Error("routing off (no agent): not an exit")
	}
}

func routeRowFixture() routeRowInput {
	ws, issue, project, router, task, owner := uuidFor(1), uuidFor(2), uuidFor(3), uuidFor(4), uuidFor(5), uuidFor(6)
	return routeRowInput{
		WorkspaceID: ws, IssueID: issue, ProjectID: project, Workflow: "Delivery",
		From: "todo", To: "in_progress", Observed: "todo", Priority: "high",
		FromAssignee: "agent:" + uuidToString(router), OwnerType: "agent", OwnerID: owner,
		RouterAgentID: router, TaskID: task, OwnerExplicit: true, Via: "route", DecisionSource: "agent",
		Confidence: pgtype.Float4{Float32: 0.8, Valid: true}, RoutingExit: true,
		RecentStatuses: []string{"todo", "in_review", "in_progress"}, EventID: uuidFor(7),
	}
}

func uuidFor(n byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{15: n}, Valid: true}
}

func TestBuildRouteObservation(t *testing.T) {
	in := routeRowFixture()
	precedents := slices.Repeat([]string{"in_progress|agent:" + uuidToString(in.OwnerID)}, 6)
	got := buildRouteObservation(in, precedents)

	if got.Kind != "route" || got.SubjectID != in.IssueID || got.WorkspaceID != in.WorkspaceID || got.Class != "Delivery/todo" {
		t.Fatalf("identity: kind=%q class=%q", got.Kind, got.Class)
	}
	if want := "in_progress|agent:" + uuidToString(in.OwnerID); got.ChoiceKey.String != want || !got.ChoiceKey.Valid {
		t.Errorf("choice_key = %+v, want %q", got.ChoiceKey, want)
	}
	if got.DecisionSource.String != "agent" || got.Outcome.String != "applied" || !got.Outcome.Valid {
		t.Errorf("source/outcome = %+v/%+v", got.DecisionSource, got.Outcome)
	}
	if got.EventID != in.EventID {
		t.Errorf("event_id = %v, want %v", got.EventID, in.EventID)
	}
	if !got.AgentConfidence.Valid || got.AgentConfidence.Float32 != 0.8 {
		t.Errorf("confidence = %+v", got.AgentConfidence)
	}
	if got.PredictedKey.String != got.ChoiceKey.String || got.PredictedPrecedents.Int32 != 6 || !got.PredictedConfidence.Valid {
		t.Errorf("prediction = key %q precedents %d", got.PredictedKey.String, got.PredictedPrecedents.Int32)
	}

	var inputs map[string]any
	if err := json.Unmarshal(got.Inputs, &inputs); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"from_status", "to_status", "observed_status", "workflow", "project_id", "priority", "from_assignee", "router_agent_id", "task_id", "owner_explicit", "via", "routing_exit", "recent_statuses"}
	if len(inputs) != len(wantKeys) {
		t.Errorf("inputs has %d keys, want exactly %d: %v", len(inputs), len(wantKeys), inputs)
	}
	for _, key := range wantKeys {
		if _, ok := inputs[key]; !ok {
			t.Errorf("inputs lacks %q", key)
		}
	}
	if inputs["observed_status"] != "todo" || inputs["from_status"] != "todo" || inputs["to_status"] != "in_progress" || inputs["via"] != "route" || inputs["routing_exit"] != true || inputs["owner_explicit"] != true {
		t.Errorf("inputs = %v", inputs)
	}
}

func TestBuildRouteObservationEdgeCases(t *testing.T) {
	// A status write (the fallback) carries no confidence and an issue with no
	// workflow uses the none/ class.
	in := routeRowFixture()
	in.Via, in.Workflow, in.Confidence, in.OwnerExplicit = "status", "", pgtype.Float4{}, false
	got := buildRouteObservation(in, nil)
	if got.AgentConfidence.Valid || got.Class != "none/todo" {
		t.Errorf("status row: confidence %+v class %q", got.AgentConfidence, got.Class)
	}
	if got.PredictedKey.String != "" || got.PredictedPrecedents.Int32 != 0 || got.PredictedConfidence.Valid {
		t.Errorf("no precedents must abstain: %+v", got)
	}
	// An owner-only route has no status change and so no event id.
	in = routeRowFixture()
	in.EventID = pgtype.UUID{}
	if got := buildRouteObservation(in, nil); got.EventID.Valid {
		t.Error("owner-only row must keep event_id NULL")
	}
	// No owner after the write: the label still has two parts.
	in = routeRowFixture()
	in.OwnerType, in.OwnerID = "", pgtype.UUID{}
	if got := buildRouteObservation(in, nil); got.ChoiceKey.String != "in_progress|:" {
		t.Errorf("choice_key without owner = %q", got.ChoiceKey.String)
	}
}

// routeWriteCase seeds an issue that sits in `todo` with an agent run on step
// `todo`, then runs a status write with the capture hook armed.
type routeWriteCase struct {
	issueID, agentID, taskID string
	rec                      *statusRecord
	hook                     *routeCapture
}

func newRouteWriteCase(t *testing.T) routeWriteCase {
	t.Helper()
	id := uuidToString(dbid.NewV7())
	runtimeID := dbfx.Runtime(t, "route-capture-runtime-"+id)
	agentID := dbfx.Agent(t, "route-capture-agent-"+id, runtimeID)
	issueID := dbfx.Issue(t, "route capture issue", map[string]any{"status": "todo"})
	taskID := dbfx.Task(t, agentID, map[string]any{"issue_id": issueID, "status": "running", "workflow_step": "todo", "runtime_id": runtimeID})
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM system1_observation WHERE workspace_id = $1 AND subject_id = $2`, testWorkspaceID, issueID)
		dbfx.Exec(t, `DELETE FROM activity_log WHERE issue_id = $1`, issueID)
	})
	run := &db.AgentTaskQueue{ID: parseUUID(taskID), AgentID: parseUUID(agentID), WorkflowStep: pgtype.Text{String: "todo", Valid: true}}
	return routeWriteCase{
		issueID: issueID, agentID: agentID, taskID: taskID,
		rec: &statusRecord{workspaceID: parseUUID(testWorkspaceID), trigger: statusTriggerFromSystem("test")},
		hook: &routeCapture{
			Via: "status", DecisionSource: "agent", Caller: workflowCaller{run: run, agent: true}, RunStep: "todo",
		},
	}
}

func (c routeWriteCase) write(t *testing.T, to string, mutate func(*db.UpdateIssueParams)) (db.Issue, error) {
	t.Helper()
	ctx := withRouteCapture(withStatusRecord(context.Background(), c.rec), c.hook)
	params := db.UpdateIssueParams{ID: parseUUID(c.issueID), Status: pgtype.Text{String: to, Valid: true}}
	if mutate != nil {
		mutate(&params)
	}
	return testHandler.updateIssueWithStatusGuard(ctx, parseUUID(testWorkspaceID), "", params, nil)
}

func TestCaptureRouteLabelEventIDSharedWithStatusChange(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	var obsEvent, activityID string
	var observed, via string
	dbfx.QueryRow(t, `SELECT event_id::text, inputs->>'observed_status', inputs->>'via' FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID).Scan(&obsEvent, &observed, &via)
	dbfx.QueryRow(t, `SELECT id::text FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, c.issueID).Scan(&activityID)
	if obsEvent == "" || obsEvent != activityID {
		t.Fatalf("event_id %q must equal the status_changed activity id %q", obsEvent, activityID)
	}
	if observed != "todo" || via != "status" {
		t.Errorf("observed_status = %q via = %q, want todo/status (the status the write replaces)", observed, via)
	}
	if c.rec.activity == nil || uuidToString(c.rec.activity.ID) != activityID {
		t.Errorf("statusRecord.activity does not name the activity row")
	}
}

func TestCaptureRouteLabelMarksRoutingExitOnTheActivityRow(t *testing.T) {
	// routing_exit is on both rows when the run leaves a routing step, and on
	// neither when routing is off for the workspace.
	for _, tc := range []struct {
		name    string
		routing bool
	}{{"routing step", true}, {"routing off", false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRouteWriteCase(t)
			if tc.routing {
				var before []byte
				dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&before)
				t.Cleanup(func() { dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, before, testWorkspaceID) })
				dbfx.Exec(t, `UPDATE workspace SET settings = coalesce(settings, '{}'::jsonb) || jsonb_build_object('routing_agent', jsonb_build_object('agent_id', $1::text, 'statuses', jsonb_build_array('todo'))) WHERE id = $2`, c.agentID, testWorkspaceID)
			}
			if _, err := c.write(t, "in_progress", nil); err != nil {
				t.Fatal(err)
			}
			var activityExit, rowExit *bool
			dbfx.QueryRow(t, `SELECT (details->>'routing_exit')::boolean FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, c.issueID).Scan(&activityExit)
			dbfx.QueryRow(t, `SELECT (inputs->>'routing_exit')::boolean FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID).Scan(&rowExit)
			if tc.routing {
				if activityExit == nil || !*activityExit || rowExit == nil || !*rowExit {
					t.Errorf("routing_exit activity/observation = %v/%v, want true/true", activityExit, rowExit)
				}
			} else if activityExit != nil || rowExit == nil || *rowExit {
				t.Errorf("routing off: activity routing_exit = %v (want absent), observation = %v (want false)", activityExit, rowExit)
			}
		})
	}
}

func TestCaptureRouteLabelTwoExitsTwoEventIDs(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	// The run returns to the step and leaves it again: a second status change.
	c.hook = &routeCapture{Via: "status", DecisionSource: "agent", Caller: c.hook.Caller, RunStep: "todo"}
	if _, err := c.write(t, "todo", nil); err != nil {
		t.Fatal(err)
	}
	c.hook = &routeCapture{Via: "status", DecisionSource: "agent", Caller: c.hook.Caller, RunStep: "todo"}
	c.rec = &statusRecord{workspaceID: parseUUID(testWorkspaceID), trigger: statusTriggerFromSystem("test")}
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	var rows, distinct int
	dbfx.QueryRow(t, `SELECT count(*), count(DISTINCT event_id) FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID).Scan(&rows, &distinct)
	if rows != 3 || distinct != 3 {
		t.Errorf("rows/distinct event ids = %d/%d, want 3/3 (each status change has its own id)", rows, distinct)
	}
	var ids int
	dbfx.QueryRow(t, `SELECT count(*) FROM system1_observation o JOIN activity_log a ON a.id = o.event_id WHERE o.kind = 'route' AND o.subject_id = $1`, c.issueID).Scan(&ids)
	if ids != 3 {
		t.Errorf("%d of 3 labels join to a status_changed row", ids)
	}
}

func TestCaptureRouteLabelHookIsSingleUse(t *testing.T) {
	c := newRouteWriteCase(t)
	ctx := withRouteCapture(withStatusRecord(context.Background(), c.rec), c.hook)
	params := db.UpdateIssueParams{ID: parseUUID(c.issueID), Status: pgtype.Text{String: "in_progress", Valid: true}}
	if _, err := testHandler.updateIssueWithStatusGuard(ctx, parseUUID(testWorkspaceID), "", params, nil); err != nil {
		t.Fatal(err)
	}
	params.Status = pgtype.Text{String: "in_review", Valid: true}
	if _, err := testHandler.updateIssueWithStatusGuard(ctx, parseUUID(testWorkspaceID), "", params, nil); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID); n != 1 {
		t.Errorf("route rows = %d, want 1 (the hook is single-use)", n)
	}
}

func TestCaptureRouteLabelNeedsStatusRecordAndStatusChange(t *testing.T) {
	c := newRouteWriteCase(t)
	// No statusRecord: the hook is not armed (decision 11).
	ctx := withRouteCapture(context.Background(), c.hook)
	params := db.UpdateIssueParams{ID: parseUUID(c.issueID), Status: pgtype.Text{String: "in_progress", Valid: true}}
	if _, err := testHandler.updateIssueWithStatusGuard(ctx, parseUUID(testWorkspaceID), "", params, nil); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID); n != 0 {
		t.Errorf("route rows without a statusRecord = %d, want 0", n)
	}
	// Status unchanged on the status path (via = status): nothing to label.
	c2 := newRouteWriteCase(t)
	if _, err := c2.write(t, "todo", func(p *db.UpdateIssueParams) { p.Priority = pgtype.Text{String: "high", Valid: true} }); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c2.issueID); n != 0 {
		t.Errorf("route rows for an unchanged status = %d, want 0", n)
	}
}

func TestCaptureRouteLabelOwnerOnlyRouteHasNoEventID(t *testing.T) {
	c := newRouteWriteCase(t)
	c.hook.Via = "route"
	if _, err := c.write(t, "todo", func(p *db.UpdateIssueParams) {
		p.AssigneeType = pgtype.Text{String: "agent", Valid: true}
		p.AssigneeID = parseUUID(c.agentID)
	}); err != nil {
		t.Fatal(err)
	}
	var withEvent, total int
	dbfx.QueryRow(t, `SELECT count(*) FILTER (WHERE event_id IS NOT NULL), count(*) FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID).Scan(&withEvent, &total)
	if total != 1 || withEvent != 0 {
		t.Errorf("owner-only route rows total/with event = %d/%d, want 1/0", total, withEvent)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, c.issueID); n != 0 {
		t.Errorf("owner-only route wrote %d status_changed rows", n)
	}
}

func TestCaptureRouteLabelStrictFailureRollsBackTheWrite(t *testing.T) {
	c := newRouteWriteCase(t)
	c.hook.DecisionSource = "bogus" // the decision_source CHECK refuses it
	if _, err := c.write(t, "in_progress", nil); err == nil {
		t.Fatal("a capture error must fail the write in strict mode")
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, c.issueID).Scan(&status)
	if status != "todo" {
		t.Errorf("status = %q after a failed capture, want todo", status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID); n != 0 {
		t.Errorf("route rows after rollback = %d", n)
	}
}

func TestCaptureRouteLabelOnTheAtomicBranch(t *testing.T) {
	c := newRouteWriteCase(t)
	ctx := withRouteCapture(withStatusRecord(context.Background(), c.rec), c.hook)
	desc := "new description"
	issue, _, _, err := testHandler.updateIssueAtomically(ctx, parseUUID(testWorkspaceID),
		db.UpdateIssueParams{ID: parseUUID(c.issueID), Status: pgtype.Text{String: "in_progress", Valid: true}, Description: pgtype.Text{String: desc, Valid: true}},
		nil, nil, nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Status != "in_progress" {
		t.Fatalf("status = %q", issue.Status)
	}
	var observed, obsEvent, activityID string
	dbfx.QueryRow(t, `SELECT inputs->>'observed_status', event_id::text FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID).Scan(&observed, &obsEvent)
	dbfx.QueryRow(t, `SELECT id::text FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, c.issueID).Scan(&activityID)
	if observed != "todo" || obsEvent != activityID {
		t.Errorf("atomic branch: observed_status=%q event %q vs activity %q", observed, obsEvent, activityID)
	}
}

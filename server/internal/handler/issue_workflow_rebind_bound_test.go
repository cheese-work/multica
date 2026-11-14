package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5228: an agent widens an issue's run budget (a workflow override on an
// update, a wider workflow on a create) with a decision-bound write for the
// issue.workflow field. The hash recipe is restated here on purpose: it is the
// contract an agent follows. sha256 of the canonical JSON
// {"issue_id", "project_id", "workflow_id"}; any of the three is null when the
// issue, project or override does not exist.
func issueWorkflowDigest(issueID, projectID, workflowID string) string {
	nullable := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	canonical, _ := json.Marshal(struct {
		IssueID    *string `json:"issue_id"`
		ProjectID  *string `json:"project_id"`
		WorkflowID *string `json:"workflow_id"`
	}{nullable(issueID), nullable(projectID), nullable(workflowID)})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

type boundRebind struct {
	askedRecordFixture
	rebindFixture
	before, after string
}

// newBoundRebind answers an approval card for the exhausted issue (update) or
// the project (create, issue id empty) moving onto the wide workflow.
func newBoundRebind(t *testing.T, installationID int64, create, delegate bool) boundRebind {
	t.Helper()
	resetDecisionLease(t)
	f := newRebindFixture(t, installationID)
	issueID := f.c.issueID
	if create {
		issueID = ""
	}
	before := issueWorkflowDigest(issueID, f.narrowProj, "")
	after := issueWorkflowDigest(issueID, f.narrowProj, f.wide)
	fixture := approvedBoundWriteCard(t, "issue.workflow", before, after)
	// The card fixture inserts an issue with its own number; move the counter
	// past it so a create does not collide.
	dbfx.Exec(t, `UPDATE workspace SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) WHERE id = $1`, testWorkspaceID)

	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	if delegate {
		setting, _ := json.Marshal(map[string]any{"allowed_agents": []string{fixture.agentID}, "allowed_fields": []string{"issue.workflow"}, "requires_human_decision": true})
		dbfx.Exec(t, `UPDATE workspace SET settings = settings || jsonb_build_object('delegated_governed_writes', $2::jsonb) WHERE id = $1`, testWorkspaceID, string(setting))
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(previous))
		dbfx.Exec(t, `DELETE FROM decision_bound_write WHERE decision_id = $1`, fixture.decision.ID)
	})
	return boundRebind{askedRecordFixture: fixture, rebindFixture: f, before: before, after: after}
}

func (b boundRebind) binding(extra map[string]any) map[string]any {
	body := map[string]any{
		"decision_id":   uuidToString(b.decision.ID),
		"before_sha256": b.before,
		"after_sha256":  b.after,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (b boundRebind) asAgent(req *http.Request) *http.Request {
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", b.agentID)
	return req
}

func (b boundRebind) set(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("PUT", "/api/issues/"+b.c.issueID+"/workflow", body), "id", b.c.issueID)
	return testutil.Call(t, testHandler.SetIssueWorkflow, b.asAgent(req))
}

func (b boundRebind) create(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	body["title"] = "CHE-5228 bound create " + workflowTestSuffix()
	body["project_id"] = b.narrowProj
	body["workflow_id"] = b.wide
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)
	return testutil.Call(t, testHandler.CreateIssue, b.asAgent(req))
}

func (b boundRebind) override(t *testing.T) string {
	t.Helper()
	var s string
	dbfx.QueryRow(t, `SELECT COALESCE(workflow_id::text, '-') FROM issue WHERE id = $1`, b.c.issueID).Scan(&s)
	return s
}

func TestBoundIssueWorkflowSetAcceptsApprovedWidening(t *testing.T) {
	b := newBoundRebind(t, 5228101, false, true)
	b.set(t, b.binding(map[string]any{"workflow_id": b.wide})).Want(http.StatusOK)
	if got := b.override(t); got != b.wide {
		t.Fatalf("override = %s, want the wide workflow", got)
	}
	var field string
	dbfx.QueryRow(t, `SELECT field FROM decision_bound_write WHERE decision_id = $1`, b.decision.ID).Scan(&field)
	if field != "issue.workflow" {
		t.Fatalf("audit field = %q", field)
	}
}

func TestBoundIssueWorkflowSetRefusals(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		delegate bool
		body     func(b boundRebind) map[string]any
		want     int
	}{
		{name: "no decision", delegate: true, want: http.StatusAccepted, // opens a card (CHE-5505)
			body: func(b boundRebind) map[string]any { return map[string]any{"workflow_id": b.wide} }},
		{name: "agent not delegated", delegate: false, want: http.StatusForbidden,
			body: func(b boundRebind) map[string]any { return b.binding(map[string]any{"workflow_id": b.wide}) }},
		{name: "stale before digest", delegate: true, want: http.StatusConflict,
			body: func(b boundRebind) map[string]any {
				return b.binding(map[string]any{"workflow_id": b.wide, "before_sha256": issueWorkflowDigest(b.c.issueID, b.narrowProj, b.narrow)})
			}},
		{name: "after digest names another workflow", delegate: true, want: http.StatusForbidden,
			body: func(b boundRebind) map[string]any {
				return b.binding(map[string]any{"workflow_id": b.wide, "after_sha256": issueWorkflowDigest(b.c.issueID, b.narrowProj, b.narrow)})
			}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			b := newBoundRebind(t, 5228110, false, scenario.delegate)
			res := b.set(t, scenario.body(b))
			if res.Code != scenario.want {
				t.Fatalf("status = %d, want %d: %s", res.Code, scenario.want, res.Text())
			}
			if got := b.override(t); got != "-" {
				t.Fatalf("refused write set the override to %s", got)
			}
			if rows := dbfx.Count(t, `SELECT count(*) FROM decision_bound_write WHERE decision_id = $1`, b.decision.ID); rows != 0 {
				t.Fatalf("refused write recorded %d audit rows", rows)
			}
		})
	}
}

func TestBoundIssueWorkflowSetDecisionIsSingleUse(t *testing.T) {
	b := newBoundRebind(t, 5228120, false, true)
	b.set(t, b.binding(map[string]any{"workflow_id": b.wide})).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE issue SET workflow_id = NULL WHERE id = $1`, b.c.issueID)
	b.set(t, b.binding(map[string]any{"workflow_id": b.wide})).Want(http.StatusConflict)
}

func TestBoundIssueCreateAcceptsApprovedWidening(t *testing.T) {
	b := newBoundRebind(t, 5228130, true, true)
	var created struct {
		ID         string  `json:"id"`
		WorkflowID *string `json:"workflow_id"`
	}
	b.create(t, b.binding(nil)).Want(http.StatusCreated).JSON(&created)
	if created.WorkflowID == nil || *created.WorkflowID != b.wide {
		t.Fatalf("created issue workflow = %v, want the wide workflow", created.WorkflowID)
	}
	// Single use: the same card does not open a second widening create.
	b.create(t, b.binding(nil)).Want(http.StatusConflict)
}

func TestBoundIssueCreateRefusals(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		delegate bool
		body     func(b boundRebind) map[string]any
		want     int
	}{
		{name: "no decision", delegate: true, want: http.StatusForbidden,
			body: func(b boundRebind) map[string]any { return map[string]any{} }},
		{name: "agent not delegated", delegate: false, want: http.StatusForbidden,
			body: func(b boundRebind) map[string]any { return b.binding(nil) }},
		{name: "after digest names another workflow", delegate: true, want: http.StatusForbidden,
			body: func(b boundRebind) map[string]any {
				return b.binding(map[string]any{"after_sha256": issueWorkflowDigest("", b.narrowProj, b.narrow)})
			}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			b := newBoundRebind(t, 5228140, true, scenario.delegate)
			res := b.create(t, scenario.body(b))
			if res.Code != scenario.want {
				t.Fatalf("status = %d, want %d: %s", res.Code, scenario.want, res.Text())
			}
			if rows := dbfx.Count(t, `SELECT count(*) FROM decision_bound_write WHERE decision_id = $1`, b.decision.ID); rows != 0 {
				t.Fatalf("refused create recorded %d audit rows", rows)
			}
		})
	}
}

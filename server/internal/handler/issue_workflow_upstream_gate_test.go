package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// upstream_checked create gate (CHE-5583, CHE-5574 step 6). The gate rides the
// create PropertyGate; these tests drive a real create over HTTP.

type upstreamCase struct {
	gatedProject string // the project the gate names
	otherProject string // a second project on the same workflow, not named
	propertyID   string
}

func newUpstreamCase(t *testing.T, mode string) upstreamCase {
	t.Helper()
	seedTestCatalog(t)
	gated := createWorkflowTestProject(t, "Upstream gated")
	other := createWorkflowTestProject(t, "Upstream other")
	dbfx.Exec(t, `DELETE FROM issue_property WHERE workspace_id = $1 AND name = 'Upstream check'`, testWorkspaceID)
	property := createTestProperty(t, map[string]any{"name": "Upstream check", "type": "text"})
	steps := []map[string]any{
		{"status_key": "backlog", "entry": []map[string]any{{"guard": "upstream_checked", "mode": mode, "member_override": true, "projects": []string{gated}}}},
		{"status_key": "todo"},
		{"status_key": "in_progress"},
		{"status_key": "done"},
	}
	wf := createTestWorkflowSteps(t, "Upstream "+workflowTestSuffix(), steps)
	setProjectWorkflow(t, gated, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setProjectWorkflow(t, other, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	return upstreamCase{gatedProject: gated, otherProject: other, propertyID: property.ID}
}

func (u upstreamCase) create(t *testing.T, project, title, value string) *testutil.Response {
	t.Helper()
	body := map[string]any{"title": title + " " + workflowTestSuffix(), "project_id": project}
	if value != "" {
		body["properties"] = map[string]any{u.propertyID: value}
	}
	return testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body))
}

func (u upstreamCase) wantRefused(t *testing.T, res *testutil.Response, parts ...string) {
	t.Helper()
	res.Want(http.StatusConflict)
	if code, _ := res.Map()["code"].(string); code != "entry_gate_failed" {
		t.Fatalf("refusal code = %q, body %s", code, res.Text())
	}
	msg, _ := res.Map()["error"].(string)
	if !strings.HasPrefix(msg, "upstream check missing: ") {
		t.Fatalf("message must start with the guard name: %q", msg)
	}
	for _, p := range parts {
		if !strings.Contains(msg, p) {
			t.Fatalf("message %q lacks %q", msg, p)
		}
	}
}

func TestUpstreamGateRefusesFeatWithoutProperty(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	u := newUpstreamCase(t, "block")
	u.wantRefused(t, u.create(t, u.gatedProject, "feat(x): a thing", ""),
		"Upstream check", "github.com/multica-ai/multica", "none found: <query>", "not multica: <repo>", "Fix: add --property")
	u.wantRefused(t, u.create(t, u.gatedProject, "plan(x): a plan", ""))
}

func TestUpstreamGateAcceptsValidValues(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	u := newUpstreamCase(t, "block")
	for _, v := range []string{
		"https://github.com/multica-ai/multica/pull/1",
		"none found: multica notification digest",
		"not multica: dotfiles-agents",
	} {
		u.create(t, u.gatedProject, "feat(x): a thing", v).Want(http.StatusCreated)
	}
}

func TestUpstreamGateRefusesInvalidValues(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	u := newUpstreamCase(t, "block")
	for _, v := range []string{"none found", "none found:", "none found: ab", "not multica:", "https://github.com/other-org/multica/pull/1", "whatever"} {
		u.wantRefused(t, u.create(t, u.gatedProject, "feat(x): a thing", v))
	}
}

func TestUpstreamGateExemptsChoreAndFix(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	u := newUpstreamCase(t, "block")
	u.create(t, u.gatedProject, "chore(x): a thing", "").Want(http.StatusCreated)
	u.create(t, u.gatedProject, "fix(x): a thing", "").Want(http.StatusCreated)
}

func TestUpstreamGateRefusesUntypedTitle(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	u := newUpstreamCase(t, "block")
	u.wantRefused(t, u.create(t, u.gatedProject, "no prefix here", ""), "title has no type prefix", "feat(...), plan(...), chore(...) or fix(...)")
	u.wantRefused(t, u.create(t, u.gatedProject, "no prefix here", "none found: some query"), "title has no type prefix")
}

func TestUpstreamGateLeavesASecondProjectOnTheSameWorkflowAlone(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	u := newUpstreamCase(t, "block")
	u.create(t, u.otherProject, "feat(x): a thing", "").Want(http.StatusCreated)
	u.create(t, u.otherProject, "no prefix here", "").Want(http.StatusCreated)
}

func TestUpstreamGateWarnAndOffDoNotRefuse(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, mode := range []string{"warn", "off"} {
		u := newUpstreamCase(t, mode)
		u.create(t, u.gatedProject, "feat(x): a thing", "").Want(http.StatusCreated)
	}
}

func TestUpstreamGateWithoutGateInWorkflow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	project := createWorkflowTestProject(t, "Upstream no gate")
	wf := createTestWorkflowSteps(t, "NoGate "+workflowTestSuffix(), []map[string]any{{"status_key": "backlog"}, {"status_key": "todo"}})
	setProjectWorkflow(t, project, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID,
		map[string]any{"title": "feat(x): ungated " + workflowTestSuffix(), "project_id": project})).Want(http.StatusCreated)
}

func TestUpstreamGateRejectsBadProjectsOnSave(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	withFeatureFlag(t, testHandler, "project_workflows_v1", true)
	for _, projects := range []any{[]string{}, []string{"multica"}, nil} {
		gate := map[string]any{"guard": "upstream_checked", "mode": "block"}
		if projects != nil {
			gate["projects"] = projects
		}
		steps := []map[string]any{{"status_key": "backlog", "entry": []map[string]any{gate}}, {"status_key": "todo"}}
		res := testutil.Call(t, testHandler.CreateIssueWorkflow, workflowRequest("POST", "/api/issue-workflows", map[string]any{"name": "Bad " + workflowTestSuffix(), "steps": steps}))
		if res.Code < 400 || res.Code >= 500 || !strings.Contains(res.Text(), "projects") {
			t.Fatalf("projects %v: status %d body %s", projects, res.Code, res.Text())
		}
	}
}

// A property read that fails refuses the create: the gate fails closed.
func TestUpstreamGateFailsClosedOnReadError(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	project := parseUUID(createWorkflowTestProject(t, "Upstream read error"))
	def := &issueworkflow.Definition{Steps: []issueworkflow.Step{{StatusKey: "backlog", Entry: []issueworkflow.EntryGate{
		{Guard: "upstream_checked", Mode: "block", Projects: []string{uuidToString(project)}},
	}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := testHandler.createPropertyGate(parseUUID(testWorkspaceID))(ctx, testHandler.Queries, def, nil, project, "feat(x): a thing")
	if err == nil {
		t.Fatal("a property read error let the create through")
	}
}

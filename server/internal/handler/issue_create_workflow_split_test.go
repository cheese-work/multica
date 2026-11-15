package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Issue create assigns a new root issue to arm A or B of the project's split by
// issue-number parity (CHE-5264, S1b of the CHE-5238 A/B). Even = arm B.

type createSplitFixture struct {
	projectID, armA, armB, agentA, agentB string
	androidSquad, excludedAgent           string
}

func newCreateSplitFixture(t *testing.T) createSplitFixture {
	t.Helper()
	seedTestCatalog(t)
	runtimeID := ""
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, seededReadyAgentID(t)).Scan(&runtimeID)
	f := createSplitFixture{
		agentA:        dbfx.Agent(t, "Split arm A "+workflowTestSuffix(), runtimeID),
		agentB:        dbfx.Agent(t, "Split arm B "+workflowTestSuffix(), runtimeID),
		excludedAgent: dbfx.Agent(t, "Split excluded "+workflowTestSuffix(), runtimeID),
	}
	f.androidSquad = dbfx.Squad(t, "Split android "+workflowTestSuffix(), f.agentA)
	armA := createTestWorkflow(t, "split-create-a-"+workflowTestSuffix(), "in_progress", "in_review")
	armB := createTestWorkflowSteps(t, "split-create-b-"+workflowTestSuffix(), gateSteps(gate("pr.ready", "block")))
	f.armA, f.armB = armA.ID, armB.ID
	setWorkflowHandoff(t, f.armA, "in_progress", handToAgent(f.agentA, "arm A")).Want(http.StatusOK)
	setWorkflowHandoff(t, f.armB, "in_progress", handToAgent(f.agentB, "arm B")).Want(http.StatusOK)
	f.projectID = createWorkflowTestProject(t, "Split create project")
	setProjectWorkflow(t, f.projectID, map[string]any{"workflow_id": f.armA}).Want(http.StatusOK)
	f.setSplit(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, util.MustParseUUID(f.androidSquad))
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = ANY($1)`, []string{f.agentA, f.agentB, f.excludedAgent})
	})
	return f
}

func (f createSplitFixture) setSplit(t *testing.T, from, to time.Time) {
	t.Helper()
	raw, _ := json.Marshal(ProjectWorkflowSplit{
		ArmBWorkflowID: f.armB, StartsAt: from, EndsAt: to,
		ExcludedAssigneeIDs: []string{f.excludedAgent}, ExcludedSquadIDs: []string{f.androidSquad},
	})
	dbfx.Exec(t, `UPDATE project SET workflow_split = $2::jsonb WHERE id = $1`, f.projectID, string(raw))
}

// nextNumber makes the next issue number of the workspace even or odd.
func nextNumber(t *testing.T, even bool) {
	t.Helper()
	syncIssueCounter(t)
	want := 1
	if even {
		want = 0
	}
	dbfx.Exec(t, `UPDATE workspace SET issue_counter = issue_counter + CASE WHEN (issue_counter + 1) % 2 = $2 THEN 0 ELSE 1 END WHERE id = $1`, testWorkspaceID, want)
}

type createdSplitIssue struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
}

func (f createSplitFixture) create(t *testing.T, even bool, over map[string]any) createdSplitIssue {
	t.Helper()
	nextNumber(t, even)
	body := map[string]any{"title": "split create " + workflowTestSuffix(), "status": "in_progress", "project_id": f.projectID}
	for k, v := range over {
		body[k] = v
	}
	var created createdSplitIssue
	testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)).Want(http.StatusCreated).JSON(&created)
	return created
}

// createFromComment makes a sub-issue of parentID through the captured-comment
// create. Its number is whatever is next; a child never draws parity.
func (f createSplitFixture) createFromComment(t *testing.T, parentID string) createdSplitIssue {
	t.Helper()
	anchorID := dbfx.Comment(t, parentID, "anchor comment")
	syncIssueCounter(t)
	build, err := service.BuildSourceContext(context.Background(), testHandler.Queries, util.MustParseUUID(testWorkspaceID), util.MustParseUUID(anchorID))
	if err != nil {
		t.Fatal(err)
	}
	req := withURLParam(newRequest("POST", "/api/comments/"+anchorID+"/sub-issues?workspace_id="+testWorkspaceID, map[string]any{
		"mode": "manual", "capture_token": build.Token,
		"issue": map[string]any{"title": "comment child " + workflowTestSuffix(), "status": "in_progress", "project_id": f.projectID},
	}), "commentId", anchorID)
	var created createdSplitIssue
	testutil.Call(t, testHandler.CreateCommentSubIssue, req).Want(http.StatusCreated).JSON(&created)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_source_context WHERE issue_id = $1`, util.MustParseUUID(created.ID))
	})
	return created
}

// wantArm checks the override, the assignee and the run of the issue's step.
func (f createSplitFixture) wantArm(t *testing.T, issueID string, b bool) {
	t.Helper()
	override, agent := "", f.agentA
	if b {
		override, agent = f.armB, f.agentB
	}
	if got := issueOverride(t, issueID); got != override {
		t.Fatalf("workflow override = %q, want %q", got, override)
	}
	if got := createdIssueAssignee(t, issueID); got != agent {
		t.Fatalf("assignee = %q, want %q", got, agent)
	}
	if steps := pendingStepRuns(t, issueID, agent); len(steps) != 1 || steps[0] != "in_progress" {
		t.Fatalf("pending runs for %s = %v, want one for in_progress", agent, steps)
	}
}

func TestCreateSplitEvenRootTakesArmBOddRootTakesArmA(t *testing.T) {
	f := newCreateSplitFixture(t)
	even := f.create(t, true, nil)
	if even.Number%2 != 0 {
		t.Fatalf("number %d is not even", even.Number)
	}
	f.wantArm(t, even.ID, true)
	if n := activityCount(t, even.ID, "workflow_split"); n != 1 {
		t.Fatalf("%d split activity rows, want 1", n)
	}
	odd := f.create(t, false, nil)
	if odd.Number%2 != 1 {
		t.Fatalf("number %d is not odd", odd.Number)
	}
	f.wantArm(t, odd.ID, false)
}

func TestCreateSplitKeepsAnExplicitWorkflow(t *testing.T) {
	f := newCreateSplitFixture(t)
	explicit := createTestWorkflow(t, "split-explicit-"+workflowTestSuffix(), "in_progress", "in_review")
	nextNumber(t, true)
	body := map[string]any{"title": "explicit " + workflowTestSuffix(), "status": "in_progress", "project_id": f.projectID, "workflow_id": explicit.ID}
	var got createdSplitIssue
	req := withActorSource(newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body), "interactive_jwt")
	testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated).JSON(&got)
	if o := issueOverride(t, got.ID); o != explicit.ID {
		t.Fatalf("override = %q, want the explicit workflow %s", o, explicit.ID)
	}
}

func TestCreateSplitExcludesTheAndroidSquadAndExcludedAgents(t *testing.T) {
	f := newCreateSplitFixture(t)
	for name, assignee := range map[string]map[string]any{
		"android squad":  {"assignee_type": "squad", "assignee_id": f.androidSquad},
		"excluded agent": {"assignee_type": "agent", "assignee_id": f.excludedAgent},
	} {
		t.Run(name, func(t *testing.T) {
			got := f.create(t, true, assignee)
			if o := issueOverride(t, got.ID); o != "" {
				t.Fatalf("excluded root got override %q", o)
			}
			if n := activityCount(t, got.ID, "workflow_split_excluded"); n != 1 {
				t.Fatalf("%d exclusion activity rows, want 1", n)
			}
		})
	}
}

func TestCreateSplitChildrenFollowTheirParent(t *testing.T) {
	f := newCreateSplitFixture(t)
	b := f.create(t, true, nil)
	odd := f.create(t, false, nil)
	excluded := f.create(t, true, map[string]any{"assignee_type": "squad", "assignee_id": f.androidSquad})

	// Ordinary create: the child's own parity never counts.
	for _, even := range []bool{true, false} {
		f.wantArm(t, f.create(t, even, map[string]any{"parent_issue_id": b.ID}).ID, true)
		f.wantArm(t, f.create(t, even, map[string]any{"parent_issue_id": odd.ID}).ID, false)
	}
	child := f.create(t, true, map[string]any{"parent_issue_id": excluded.ID, "assignee_type": "agent", "assignee_id": f.agentA})
	grandchild := f.create(t, true, map[string]any{"parent_issue_id": child.ID, "assignee_type": "agent", "assignee_id": f.agentA})
	for _, id := range []string{child.ID, grandchild.ID} {
		if o := issueOverride(t, id); o != "" {
			t.Fatalf("descendant of an excluded root got override %q", o)
		}
	}

	// Captured-comment create: always a child.
	f.wantArm(t, f.createFromComment(t, b.ID).ID, true)
	f.wantArm(t, f.createFromComment(t, odd.ID).ID, false)
	if o := issueOverride(t, f.createFromComment(t, excluded.ID).ID); o != "" {
		t.Fatalf("comment child of an excluded root got override %q", o)
	}
}

func TestCreateSplitOutsideTheWindowAndOtherProjects(t *testing.T) {
	f := newCreateSplitFixture(t)
	f.setSplit(t, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	f.wantArm(t, f.create(t, true, nil).ID, false)
	f.setSplit(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	f.wantArm(t, f.create(t, true, nil).ID, false)

	f.setSplit(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	other := createWorkflowTestProject(t, "Split other project")
	setProjectWorkflow(t, other, map[string]any{"workflow_id": f.armA}).Want(http.StatusOK)
	got := f.create(t, true, map[string]any{"project_id": other})
	if o := issueOverride(t, got.ID); o != "" {
		t.Fatalf("project without a split got override %q", o)
	}
}

func TestCreateSplitEntryGateRefusalUsesNoNumber(t *testing.T) {
	f := newCreateSplitFixture(t)
	nextNumber(t, true)
	counter := func() int {
		var n int
		dbfx.QueryRow(t, `SELECT issue_counter FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&n)
		return n
	}
	before := counter()
	body := map[string]any{"title": "refused by arm B " + workflowTestSuffix(), "status": "in_review", "project_id": f.projectID}
	testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)).Want(http.StatusConflict)
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE project_id = $1`, f.projectID); n != 0 {
		t.Fatalf("a refused create left %d issues", n)
	}
	if after := counter(); after != before {
		t.Fatalf("issue_counter %d -> %d, a refused create used a number", before, after)
	}
}

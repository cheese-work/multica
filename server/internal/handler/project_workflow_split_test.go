package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Per-project workflow split setting (CHE-5262, S1a of the CHE-5238 A/B).

type splitFixture struct {
	projectID, armB, lead, admin, member string
}

func newSplitFixture(t *testing.T) splitFixture {
	t.Helper()
	armA := createTestWorkflow(t, "split-a-"+workflowTestSuffix(), "in_review")
	armB := createTestWorkflow(t, "split-b-"+workflowTestSuffix(), "in_review", "in_progress")
	f := splitFixture{
		lead:   hicMember(t, "member"),
		admin:  hicMember(t, "admin"),
		member: hicMember(t, "member"),
		armB:   armB.ID,
	}
	f.projectID = createWorkflowTestProject(t, "split project")
	dbfx.Exec(t, `UPDATE project SET workflow_id = $1, lead_type = 'member', lead_id = $2 WHERE id = $3`, armA.ID, f.lead, f.projectID)
	return f
}

func (f splitFixture) body(over map[string]any) map[string]any {
	b := map[string]any{
		"arm_b_workflow_id":     f.armB,
		"starts_at":             "2026-11-14T00:00:00Z",
		"ends_at":               time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"excluded_assignee_ids": []string{uuid.NewString()},
		"excluded_squad_ids":    []string{uuid.NewString()},
	}
	for k, v := range over {
		b[k] = v
	}
	return b
}

func (f splitFixture) put(t *testing.T, userID string, body map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(workflowRequest("PUT", "/api/projects/"+f.projectID+"/workflow-split", body), "id", f.projectID)
	req.Header.Set("X-User-ID", userID)
	return testutil.Call(t, testHandler.SetProjectWorkflowSplit, req)
}

func (f splitFixture) clear(t *testing.T, userID string) *testutil.Response {
	t.Helper()
	req := withURLParam(workflowRequest("DELETE", "/api/projects/"+f.projectID+"/workflow-split", nil), "id", f.projectID)
	req.Header.Set("X-User-ID", userID)
	return testutil.Call(t, testHandler.ClearProjectWorkflowSplit, req)
}

func (f splitFixture) read(t *testing.T) ProjectResponse {
	t.Helper()
	req := withURLParam(workflowRequest("GET", "/api/projects/"+f.projectID, nil), "id", f.projectID)
	var got ProjectResponse
	testutil.Call(t, testHandler.GetProject, req).Want(http.StatusOK).JSON(&got)
	return got
}

func TestProjectWorkflowSplitSetReadClear(t *testing.T) {
	f := newSplitFixture(t)
	if got := f.read(t); got.WorkflowSplit != nil {
		t.Fatalf("new project has split %+v, want none", got.WorkflowSplit)
	}
	for _, who := range []string{testUserID, f.admin, f.lead} {
		f.put(t, who, f.body(nil)).Want(http.StatusOK)
		got := f.read(t).WorkflowSplit
		if got == nil || got.ArmBWorkflowID != f.armB || len(got.ExcludedAssigneeIDs) != 1 || len(got.ExcludedSquadIDs) != 1 {
			t.Fatalf("read after set by %s = %+v", who, got)
		}
		f.clear(t, who).Want(http.StatusOK)
		if got := f.read(t).WorkflowSplit; got != nil {
			t.Fatalf("read after clear by %s = %+v, want none", who, got)
		}
	}
}

func TestProjectWorkflowSplitOtherMemberForbidden(t *testing.T) {
	f := newSplitFixture(t)
	f.put(t, f.member, f.body(nil)).Want(http.StatusForbidden)
	f.put(t, f.admin, f.body(nil)).Want(http.StatusOK)
	f.clear(t, f.member).Want(http.StatusForbidden)
	if f.read(t).WorkflowSplit == nil {
		t.Fatal("forbidden clear removed the split")
	}
}

func TestProjectWorkflowSplitRejectsBadInput(t *testing.T) {
	f := newSplitFixture(t)
	thin := createTestWorkflow(t, "split-thin-"+workflowTestSuffix()) // lacks the project's "in_review"
	for name, over := range map[string]map[string]any{
		"unknown workflow":          {"arm_b_workflow_id": uuid.NewString()},
		"end before start":          {"starts_at": "2026-11-28T00:00:00Z", "ends_at": "2026-11-14T00:00:00Z"},
		"end equals start":          {"starts_at": "2026-11-14T00:00:00Z", "ends_at": "2026-11-14T00:00:00Z"},
		"arm B lacks project steps": {"arm_b_workflow_id": thin.ID},
		"bad excluded id":           {"excluded_assignee_ids": []string{"nope"}},
	} {
		t.Run(name, func(t *testing.T) {
			f.put(t, f.admin, f.body(over)).Want(http.StatusBadRequest)
			if f.read(t).WorkflowSplit != nil {
				t.Fatal("rejected request stored a split")
			}
		})
	}
}

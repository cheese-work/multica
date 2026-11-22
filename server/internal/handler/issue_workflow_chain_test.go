package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// chainFixture is an issue on step "todo" that agent A works in a running task,
// moving to step "in_progress", whose handler and chain_same_owner flag the
// case sets. Issue and project are removed with the test.
type chainFixture struct {
	issueID string
	taskID  string
	agentA  string
	agentB  string
}

type chainCase struct {
	flag         bool
	startHandler func(f *chainFixture) map[string]any // handler of the step the run started in
	endHandler   func(f *chainFixture) map[string]any // handler of the entered step
}

const chainBriefText = "Implement it, then hand over."

func newChainFixture(t *testing.T, c chainCase) *chainFixture {
	t.Helper()
	seedTestCatalog(t)
	f := &chainFixture{agentA: seededReadyAgentID(t)}
	runtimeID := handlerTestRuntimeID(t)
	f.agentB = dbfx.Agent(t, "chain other "+workflowTestSuffix(), runtimeID)

	start := func(f *chainFixture) map[string]any { return handToAgent(f.agentA, "Triage it.") }
	if c.startHandler != nil {
		start = c.startHandler
	}
	end := func(f *chainFixture) map[string]any { return handToAgent(f.agentA, chainBriefText) }
	if c.endHandler != nil {
		end = c.endHandler
	}
	projectID := createWorkflowTestProject(t, "Chain project "+workflowTestSuffix())
	dbfx.Exec(t, `UPDATE project SET lead_type = 'agent', lead_id = $2 WHERE id = $1`, projectID, f.agentA)
	setColumnHandoff(t, projectID, "todo", start(f)).Want(http.StatusOK)
	endStep := end(f)
	if c.flag {
		endStep["chain_same_owner"] = true
	}
	setColumnHandoff(t, projectID, "in_progress", endStep).Want(http.StatusOK)

	f.issueID = dbfx.Issue(t, "chain "+workflowTestSuffix(), testutil.Cols{
		"project_id": projectID, "status": "todo", "assignee_type": "agent", "assignee_id": f.agentA,
	})
	f.taskID = dbfx.Task(t, f.agentA, testutil.Cols{
		"issue_id": f.issueID, "runtime_id": runtimeID, "status": "running", "workflow_step": "todo",
		"started_at": testutil.Raw("now()"), "originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	return f
}

func (f *chainFixture) runHeaders() map[string]string {
	return map[string]string{"X-Agent-ID": f.agentA, "X-Task-ID": f.taskID}
}

func (f *chainFixture) moveAs(t *testing.T, headers map[string]string, body map[string]any) *testutil.Response {
	t.Helper()
	return issueWriters[0].write(t, f.issueID, body, headers)
}

func (f *chainFixture) runsFor(t *testing.T, agentID string) int {
	t.Helper()
	return len(pendingStepRuns(t, f.issueID, agentID))
}

func (f *chainFixture) assignee(t *testing.T) string {
	t.Helper()
	_, _, id := issueStatusAndAssignee(t, f.issueID)
	return id
}

func stepBriefOf(t *testing.T, resp *testutil.Response) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	brief, _ := out["step_brief"].(string)
	return brief
}

// With the flag off the move starts the handler's run, as before the flag.
func TestChainSameOwnerFlagOffStartsRun(t *testing.T) {
	f := newChainFixture(t, chainCase{flag: false})

	resp := f.moveAs(t, f.runHeaders(), map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if got := f.runsFor(t, f.agentA); got != 1 {
		t.Fatalf("runs for the handler = %d, want 1 with the flag off", got)
	}
	if brief := stepBriefOf(t, resp); brief != "" {
		t.Fatalf("step_brief = %q, want none when a run delivers the step", brief)
	}
}

// The same agent keeps working: no task row, the step text comes back in the
// response, and the run's own step moves along.
func TestChainSameOwnerFlagOnSuppressesRunAndReturnsBrief(t *testing.T) {
	f := newChainFixture(t, chainCase{flag: true})

	resp := f.moveAs(t, f.runHeaders(), map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if got := f.runsFor(t, f.agentA); got != 0 {
		t.Fatalf("runs for the handler = %d, want 0 on a chained move", got)
	}
	if status, _, _ := issueStatusAndAssignee(t, f.issueID); status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", status)
	}
	if got := f.assignee(t); got != f.agentA {
		t.Fatalf("assignee = %s, want agent A %s", got, f.agentA)
	}
	if brief := stepBriefOf(t, resp); !strings.Contains(brief, chainBriefText) {
		t.Fatalf("step_brief = %q, want the entered step's text %q", brief, chainBriefText)
	}
	var step string
	dbfx.QueryRow(t, `SELECT workflow_step FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&step)
	if step != "in_progress" {
		t.Fatalf("run step = %q, want in_progress (advanced with the chained move)", step)
	}
}

// A different final assignee is a different owner: its run starts.
func TestChainSameOwnerOtherAgentStartsRun(t *testing.T) {
	f := newChainFixture(t, chainCase{
		flag:       true,
		endHandler: func(f *chainFixture) map[string]any { return handToAgent(f.agentB, chainBriefText) },
	})

	resp := f.moveAs(t, f.runHeaders(), map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if got := f.runsFor(t, f.agentB); got != 1 {
		t.Fatalf("runs for the other agent = %d, want 1", got)
	}
	if brief := stepBriefOf(t, resp); brief != "" {
		t.Fatalf("step_brief = %q, want none when another agent's run delivers it", brief)
	}
}

// The same agent entering a step under another handler type is a role change:
// the run started under the project-lead step, so the agent's own run starts.
func TestChainSameOwnerRoleChangeStartsRun(t *testing.T) {
	f := newChainFixture(t, chainCase{
		flag: true,
		startHandler: func(f *chainFixture) map[string]any {
			return map[string]any{"handler": map[string]any{"type": "project_lead"}, "instructions": "Lead it."}
		},
	})

	f.moveAs(t, f.runHeaders(), map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if got := f.runsFor(t, f.agentA); got != 1 {
		t.Fatalf("runs for the handler = %d, want 1 after a role change", got)
	}
}

// A person moving the issue is not a run chaining its own work.
func TestChainSameOwnerMemberCallerStartsRun(t *testing.T) {
	f := newChainFixture(t, chainCase{flag: true})

	f.moveAs(t, nil, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if got := f.runsFor(t, f.agentA); got != 1 {
		t.Fatalf("runs for the handler = %d, want 1 for a member's move", got)
	}
}

// The batch write has no per-issue response to carry the brief, so it never
// chains: the run delivers the step text.
func TestChainSameOwnerBatchWriteStartsRun(t *testing.T) {
	f := newChainFixture(t, chainCase{flag: true})

	issueWriters[1].write(t, f.issueID, map[string]any{"status": "in_progress"}, f.runHeaders()).Want(http.StatusOK)

	if got := f.runsFor(t, f.agentA); got != 1 {
		t.Fatalf("runs for the handler = %d, want 1 on the batch write", got)
	}
}

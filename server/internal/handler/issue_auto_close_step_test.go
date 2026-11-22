package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7672 (CHE-7658 step 7): a step carries its own auto_close
// {"mode": "off|warn|block", "top_level": bool}. The workspace setting off is
// the kill switch; otherwise the step wins. top_level lets a root issue close.
// In the edge evaluator, a matched pr.merged -> done edge whose close check
// rejects takes its on_timeout exit once, at match time.

func stepAutoCloseSteps(auto map[string]any) []map[string]any {
	accepted := map[string]any{"status_key": "agent_accepted", "edges": []map[string]any{
		{"to": "done", "guard": "pr.merged", "timeout": "2h", "on_timeout": "agent_review"},
	}}
	if auto != nil {
		accepted["auto_close"] = auto
	}
	return []map[string]any{
		{"status_key": "in_progress"},
		{"status_key": "agent_review", "edges": []map[string]any{
			{"to": "in_progress", "guard": "agent_verdict(CHANGES)"},
			{"to": "agent_accepted", "guard": "agent_verdict(PASS)"},
		}},
		accepted,
		{"status_key": "blocked"},
		{"status_key": "done"},
	}
}

// newStepAutoClose is newCloseOrDispatch with the step's auto_close set and the
// workspace mode chosen separately ("" leaves the workspace key unset).
func newStepAutoClose(t *testing.T, workspaceMode string, auto map[string]any) autoClose {
	t.Helper()
	setWorkspacePRMergeStatusForTest(t, "none")
	c := newAutoClose(t, workspaceMode)
	wf := createTestWorkflowSteps(t, "Step auto close "+workflowTestSuffix(), stepAutoCloseSteps(auto))
	setProjectWorkflow(t, c.projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, c.projectID, "agent_accepted", handToAgent(c.acceptor, "Accept it.")).Want(http.StatusOK)
	return c
}

var stepBlock = map[string]any{"mode": "block"}

// mergeNow merges the issue's PR at head and makes its watcher due.
func (c autoClose) mergeNow(t *testing.T, prID string) {
	t.Helper()
	c.setPRState(t, prID, "merged")
	dbfx.Exec(t, `UPDATE github_pull_request SET base_ref = 'main', default_branch = 'main' WHERE id = $1`, prID)
	dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() WHERE issue_id = $1 AND system_rule = 'workflow_edges'`, c.issueID)
}

func (c autoClose) statusMoves(t *testing.T, to string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'status_changed' AND details->>'to' = $2`, c.issueID, to)
}

// First test: a merge that matches the pr.merged edge while the close check
// rejects takes the edge's on_timeout exit at once, before the 2 h deadline,
// and only once.
func TestStepAutoClose_MatchedMergeWithAFailingPredicateTakesOnTimeoutOnce(t *testing.T) {
	c := newStepAutoClose(t, "", stepBlock)
	c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
	prID := c.newPR(t, autoCloseHead)
	dbfx.Exec(t, `UPDATE issue SET metadata = '{}'::jsonb WHERE id = $1`, c.issueID) // post_close_work not none
	c.enter(t)
	c.wantHandlerRun(t)
	c.mergeNow(t, prID)
	tickEdges(t)
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "agent_review" {
		t.Fatalf("status = %q, want agent_review", status)
	}
	if n := c.statusMoves(t, "agent_review"); n != 1 {
		t.Fatalf("moves to agent_review = %d, want 1", n)
	}
	tickEdges(t)
	if n := c.statusMoves(t, "agent_review"); n != 1 {
		t.Fatalf("moves to agent_review after another tick = %d, want 1", n)
	}
	if n := c.statusMoves(t, "done"); n != 0 {
		t.Fatalf("moves to done = %d, want 0", n)
	}
}

// The same merge with every predicate holding still closes.
func TestStepAutoClose_MatchedMergeWithEveryPredicateHoldingClosesAsToday(t *testing.T) {
	c := newStepAutoClose(t, "", stepBlock)
	c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
	prID := c.newPR(t, autoCloseHead)
	c.enter(t)
	c.wantPending(t)
	c.mergeNow(t, prID)
	tickEdges(t)
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "done" {
		t.Fatalf("status = %q, want done", status)
	}
}

// The 2 h deadline with no merge takes on_timeout once.
func TestStepAutoClose_DeadlineWithNoMergeTakesOnTimeoutOnce(t *testing.T) {
	c := newStepAutoClose(t, "", stepBlock)
	c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
	c.newPR(t, autoCloseHead)
	c.enter(t)
	c.wantPending(t)
	dbfx.Exec(t, `UPDATE issue_wakeup SET created_at = now() - interval '3 hours', next_fire_at = now() WHERE issue_id = $1 AND system_rule = 'workflow_edges'`, c.issueID)
	tickEdges(t)
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "agent_review" {
		t.Fatalf("status = %q, want agent_review", status)
	}
	tickEdges(t)
	if n := c.statusMoves(t, "agent_review"); n != 1 {
		t.Fatalf("moves to agent_review = %d, want 1", n)
	}
}

// The step's mode wins over a workspace default; a step that sets nothing
// follows the workspace.
func TestStepAutoClose_StepModeWinsOverTheWorkspaceDefault(t *testing.T) {
	for name, tc := range map[string]struct {
		workspace string
		auto      map[string]any
		closes    bool
	}{
		"step block, workspace unset":    {"", stepBlock, true},
		"step block, workspace warn":     {"warn", stepBlock, true},
		"step block, workspace block":    {"block", stepBlock, true},
		"step off, workspace block":      {"block", map[string]any{"mode": "off"}, false},
		"step warn, workspace block":     {"block", map[string]any{"mode": "warn"}, false},
		"no step field, workspace warn":  {"warn", nil, false},
		"no step field, workspace block": {"block", nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			c := newStepAutoClose(t, tc.workspace, tc.auto)
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			c.enter(t)
			if tc.closes {
				c.wantClosed(t)
			} else {
				c.wantHandlerRun(t)
			}
		})
	}
}

// Workspace off is the kill switch: a step that says block closes nothing, and a
// matched merge moves the issue along its edge as before.
func TestStepAutoClose_WorkspaceOffHolds(t *testing.T) {
	t.Run("a clean issue is not closed", func(t *testing.T) {
		c := newStepAutoClose(t, "off", stepBlock)
		c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
		c.enter(t)
		c.wantHandlerRun(t)
	})
	t.Run("a matched merge moves along its edge", func(t *testing.T) {
		c := newStepAutoClose(t, "off", stepBlock)
		c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
		prID := c.newPR(t, autoCloseHead)
		dbfx.Exec(t, `UPDATE issue SET metadata = '{}'::jsonb WHERE id = $1`, c.issueID)
		c.enter(t)
		c.mergeNow(t, prID)
		tickEdges(t)
		if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "done" {
			t.Fatalf("status = %q, want done: with the workspace off the merge edge is unchanged", status)
		}
	})
}

// A stopped parent holds a child, with the step's mode and with top_level.
func TestStepAutoClose_StoppedParentHoldsAChild(t *testing.T) {
	for name, auto := range map[string]map[string]any{
		"step block":     stepBlock,
		"step top_level": {"mode": "block", "top_level": true},
	} {
		t.Run(name, func(t *testing.T) {
			c := newStepAutoClose(t, "", auto)
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			dbfx.Exec(t, `UPDATE issue SET metadata = '{"delivery_stop":"stopped"}'::jsonb WHERE id = $1`, c.parentID)
			c.enter(t)
			c.wantHandlerRun(t)
		})
	}
}

// top_level lets a root issue close; a root issue never closes without it.
func TestStepAutoClose_TopLevelClosesARootIssue(t *testing.T) {
	for name, tc := range map[string]struct {
		auto   map[string]any
		closes bool
	}{
		"top_level":     {map[string]any{"mode": "block", "top_level": true}, true},
		"not top_level": {stepBlock, false},
		"top_level off": {map[string]any{"mode": "block", "top_level": false}, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := newStepAutoClose(t, "", tc.auto)
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			dbfx.Exec(t, `UPDATE issue SET parent_issue_id = NULL WHERE id = $1`, c.issueID)
			c.enter(t)
			if tc.closes {
				c.wantClosed(t)
			} else {
				c.wantHandlerRun(t)
			}
		})
	}
}

// A malformed auto_close is refused on save; a valid one reads back.
func TestStepAutoClose_SaveValidatesAndReadsBack(t *testing.T) {
	setWorkspacePRMergeStatusForTest(t, "none")
	for name, auto := range map[string]any{
		"unknown mode":  map[string]any{"mode": "maybe"},
		"missing mode":  map[string]any{"top_level": true},
		"not an object": "block",
		"unknown key":   map[string]any{"mode": "block", "extra": 1},
	} {
		t.Run(name, func(t *testing.T) {
			createTestCustomStatus(t, "agent_review", "started")
			createTestCustomStatus(t, "agent_accepted", "done")
			steps := stepAutoCloseSteps(nil)
			steps[2]["auto_close"] = auto
			withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)
			testutil.Call(t, testHandler.CreateIssueWorkflow, workflowRequest("POST", "/api/issue-workflows", map[string]any{
				"name": "Bad auto close " + workflowTestSuffix(), "steps": steps,
			})).Want(http.StatusBadRequest)
		})
	}
	createTestCustomStatus(t, "agent_review", "started")
	createTestCustomStatus(t, "agent_accepted", "done")
	wf := createTestWorkflowSteps(t, "Auto close read back "+workflowTestSuffix(), stepAutoCloseSteps(map[string]any{"mode": "block", "top_level": true}))
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Steps []struct {
			StatusKey string         `json:"status_key"`
			AutoClose map[string]any `json:"auto_close"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for _, s := range back.Steps {
		if s.StatusKey != "agent_accepted" {
			if s.AutoClose != nil {
				t.Fatalf("step %s auto_close = %v, want none", s.StatusKey, s.AutoClose)
			}
			continue
		}
		if s.AutoClose["mode"] != "block" || s.AutoClose["top_level"] != true {
			t.Fatalf("agent_accepted auto_close = %v, want block with top_level", s.AutoClose)
		}
	}
}

// A merge that arrives after the issue entered pending-merge, with a predicate
// broken in between, takes the edge's on_timeout exit once. It does not
// dispatch the accepted-step handler (CHE-7672 review finding, pending path).
func TestStepAutoClose_PendingMergeWithAPredicateBrokenTakesOnTimeoutOnce(t *testing.T) {
	changes := map[string]func(t *testing.T, c autoClose, prID string){
		"a new head merges without a PASS": func(t *testing.T, c autoClose, prID string) {
			dbfx.Exec(t, `UPDATE github_pull_request SET head_sha = $2 WHERE id = $1`, prID, autoCloseHead2)
		},
		"a live wakeup appears": func(t *testing.T, c autoClose, prID string) {
			dbfx.Exec(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, instruction, kind, mode, enabled)
				SELECT gen_random_uuid(), workspace_id, id, $2, 'look again', 'event', 'continuous', true FROM issue WHERE id = $1`, c.issueID, c.agentID)
			dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, c.issueID)
		},
	}
	for change, apply := range changes {
		t.Run(change, func(t *testing.T) {
			c := newStepAutoClose(t, "", stepBlock)
			c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
			prID := c.newPR(t, autoCloseHead)
			c.enter(t)
			c.wantPending(t)
			apply(t, c, prID)
			c.mergeNow(t, prID)
			tickEdges(t)
			if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "agent_review" {
				t.Fatalf("status = %q, want agent_review", status)
			}
			if n := c.statusMoves(t, "agent_review"); n != 1 {
				t.Fatalf("moves to agent_review = %d, want 1", n)
			}
			if n := c.acceptorRuns(t); n != 0 {
				t.Fatalf("acceptor runs = %d, want 0: the handler must not start", n)
			}
			tickEdges(t)
			if n := c.statusMoves(t, "agent_review"); n != 1 {
				t.Fatalf("moves to agent_review after another tick = %d, want 1", n)
			}
		})
	}
}

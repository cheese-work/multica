package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6969 (CHE-6940 step 2): on entry to agent_accepted the server takes the
// step's done edge itself when a clean child issue needs no handler run.

var autoCloseSteps = []map[string]any{
	{"status_key": "in_progress"},
	{"status_key": "agent_review", "edges": []map[string]any{
		{"to": "in_progress", "guard": "agent_verdict(CHANGES)"},
		{"to": "agent_accepted", "guard": "agent_verdict(PASS)"},
	}},
	{"status_key": "agent_accepted", "edges": []map[string]any{{"to": "done", "guard": "agent_done"}}},
	{"status_key": "blocked"},
	{"status_key": "done"},
}

const (
	autoCloseHead  = "dddddddddddddddddddddddddddddddddddddddd"
	autoCloseHead2 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

type autoClose struct {
	reviewLoop
	acceptor, parentID string
}

func setAutoCloseMode(t *testing.T, mode string) {
	t.Helper()
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = $1 WHERE id = $2`, previous, testWorkspaceID)
	})
	if mode == "" {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'auto_close' WHERE id = $1`, testWorkspaceID)
		return
	}
	dbfx.Exec(t, `UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || jsonb_build_object('auto_close', jsonb_build_object('mode', $2::text)) WHERE id = $1`, testWorkspaceID, mode)
}

// newAutoClose plants a clean child issue at agent_review with a reviewer run,
// a parent, and `post_close_work` = none.
func newAutoClose(t *testing.T, mode string) autoClose {
	t.Helper()
	setAutoCloseMode(t, mode)
	createTestCustomStatus(t, "agent_review", "started")
	createTestCustomStatus(t, "agent_accepted", "done")
	reviewer := seededReadyAgentID(t)
	acceptor := createHandlerTestAgent(t, "auto-close-acceptor", nil)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, reviewer).Scan(&runtimeID)
	wf := createTestWorkflowSteps(t, "Auto close "+workflowTestSuffix(), autoCloseSteps)
	projectID := createWorkflowTestProject(t, "Auto close project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, projectID, "agent_review", handToAgent(reviewer, "Review it.")).Want(http.StatusOK)
	setColumnHandoff(t, projectID, "agent_accepted", handToAgent(acceptor, "Accept it.")).Want(http.StatusOK)
	parentID := dbfx.Issue(t, "auto close parent", testutil.Cols{"project_id": projectID, "status": "in_progress"})
	issueID := dbfx.Issue(t, "auto close child", testutil.Cols{
		"project_id": projectID, "status": "agent_review", "assignee_type": "agent", "assignee_id": reviewer,
		"parent_issue_id": parentID, "metadata": testutil.Raw(`'{"post_close_work":"none"}'::jsonb`),
	})
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })
	run := dbfx.Task(t, reviewer, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "workflow_step": "agent_review",
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
	// The issue's own text is older than any verdict a test seeds.
	dbfx.Exec(t, `UPDATE issue_description_revision SET created_at = now() - interval '2 hours' WHERE issue_id = $1`, issueID)
	return autoClose{reviewLoop{reviewer, runtimeID, issueID, projectID, run}, acceptor, parentID}
}

// comment plants a comment by author, created and last edited the given ages ago.
func (c autoClose) comment(t *testing.T, authorType, authorID, created, edited string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content, created_at, updated_at)
		SELECT workspace_id, id, $2, $3::uuid, 'plan text', now() - $4::interval, now() - $5::interval FROM issue WHERE id = $1`,
		c.issueID, authorType, authorID, created, edited)
}

// seedVerdict records an earlier verdict row, as a reviewer's write would.
func (c autoClose) seedVerdict(t *testing.T, action, findings, head, age string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
		SELECT workspace_id, id, 'agent', $2, $3, jsonb_build_object('findings', $4::text, 'pull_request_id', '', 'head_sha', $5::text), now() - $6::interval
		FROM issue WHERE id = $1`, c.issueID, c.agentID, action, findings, head, age)
}

// enter moves the issue into agent_accepted as a person would.
func (c autoClose) enter(t *testing.T) {
	t.Helper()
	updateIssueForTest(t, c.issueID, map[string]any{"status": "agent_accepted"}).Want(http.StatusOK)
}

func (c autoClose) acceptorRuns(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, c.issueID, c.acceptor)
}

func (c autoClose) activities(t *testing.T, action string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = $2`, c.issueID, action)
}

func (c autoClose) wantClosed(t *testing.T) {
	t.Helper()
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "done" {
		t.Fatalf("status = %q, want done", status)
	}
	if n := c.acceptorRuns(t); n != 0 {
		t.Fatalf("acceptor runs = %d, want 0", n)
	}
	if n := c.activities(t, "auto_closed"); n != 1 {
		t.Fatalf("auto_closed rows = %d, want 1", n)
	}
}

func (c autoClose) wantHandlerRun(t *testing.T) {
	t.Helper()
	if status, _, _ := issueStatusAndAssignee(t, c.issueID); status != "agent_accepted" {
		t.Fatalf("status = %q, want agent_accepted", status)
	}
	if n := c.acceptorRuns(t); n != 1 {
		t.Fatalf("acceptor runs = %d, want 1", n)
	}
	if n := c.activities(t, "auto_closed"); n != 0 {
		t.Fatalf("auto_closed rows = %d, want 0", n)
	}
}

// First test: the verdict outcome lives in the review_passed / review_changes_requested
// activity rows, and the action name alone is not the outcome: a PASS write may
// carry P1 findings (CHE-6753, 17 Nov 2026). A clean PASS is the newest verdict
// row with no P0/P1 finding.
func TestAutoClose_VerdictRecordIsTheNewestRowWithItsFindings(t *testing.T) {
	c := newAutoClose(t, "block")
	c.verdict(t, "agent_accepted", finding("P1")).Want(http.StatusOK)
	var action, findings string
	dbfx.QueryRow(t, `SELECT action, details->>'findings' FROM activity_log WHERE issue_id = $1 AND action IN ('review_passed', 'review_changes_requested')`, c.issueID).Scan(&action, &findings)
	if action != "review_passed" || !strings.Contains(findings, "P1") {
		t.Fatalf("stored verdict = %q with findings %q, want review_passed carrying the P1 finding", action, findings)
	}
	// So a PASS that carries P1 is not a clean PASS: the handler run stays.
	c.wantHandlerRun(t)
}

// Red test: a clean child enters agent_accepted. Today the handler's run starts.
func TestAutoClose_CleanChildClosesThroughDoneEdgeWithNoRun(t *testing.T) {
	c := newAutoClose(t, "block")
	c.verdict(t, "agent_accepted").Want(http.StatusOK)
	c.wantClosed(t)
	var details string
	dbfx.QueryRow(t, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'auto_closed'`, c.issueID).Scan(&details)
	for _, p := range []string{"done_edge", "has_parent", "verdict_pass_current", "no_open_child", "no_open_decision", "no_live_wakeup", "no_cheese_stop", "parent_not_stopped", "post_close_work_none"} {
		if !strings.Contains(details, p) {
			t.Errorf("auto_closed details %s do not name predicate %q", details, p)
		}
	}
}

func TestAutoClose_PRIssueClosesOnlyWhenMergedAtThePassHead(t *testing.T) {
	merged := func(t *testing.T, c autoClose, passHead, prHead, state string) {
		t.Helper()
		c.seedVerdict(t, "review_passed", "[]", passHead, "1 hour")
		prID := c.newPR(t, prHead)
		dbfx.Exec(t, `UPDATE github_pull_request SET state = $2 WHERE id = $1`, prID, state)
		c.enter(t)
	}
	t.Run("merged at the head the PASS names", func(t *testing.T) {
		c := newAutoClose(t, "block")
		merged(t, c, autoCloseHead, autoCloseHead, "merged")
		c.wantClosed(t)
	})
	t.Run("merged at another head", func(t *testing.T) {
		c := newAutoClose(t, "block")
		merged(t, c, autoCloseHead, autoCloseHead2, "merged")
		c.wantHandlerRun(t)
	})
	t.Run("still open", func(t *testing.T) {
		c := newAutoClose(t, "block")
		merged(t, c, autoCloseHead, autoCloseHead, "open")
		c.wantHandlerRun(t)
	})
}

// Negative tests: each keeps the handler run.
func TestAutoClose_EachFailedPredicateKeepsTheHandlerRun(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, c autoClose)
	}{
		"stale PASS: a newer candidate": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `INSERT INTO attachment (workspace_id, issue_id, uploader_type, uploader_id, filename, url, content_type, size_bytes)
				SELECT workspace_id, id, 'agent', $2, 'plan-r2.md', 'https://example.test/plan-r2.md', 'text/markdown', 1 FROM issue WHERE id = $1`, c.issueID, c.agentID)
		}},
		"stale PASS: a revised candidate in a comment": {func(t *testing.T, c autoClose) {
			c.comment(t, "agent", c.acceptor, "1 minute", "1 minute")
		}},
		"stale PASS: a candidate comment edited after the PASS": {func(t *testing.T, c autoClose) {
			c.comment(t, "agent", c.acceptor, "3 hours", "1 minute")
		}},
		"stale PASS: a revised description": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `UPDATE issue SET description = 'plan r2', revision = revision + 1 WHERE id = $1`, c.issueID)
		}},
		"CHANGES after PASS": {func(t *testing.T, c autoClose) {
			c.seedVerdict(t, "review_changes_requested", `[{"severity":"P1","summary":"x"}]`, "", "1 minute")
		}},
		"stale-verdict mark": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details)
				SELECT workspace_id, id, 'system', 'review_verdict_stale', '{}'::jsonb FROM issue WHERE id = $1`, c.issueID)
		}},
		"return to in_progress after the PASS": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details)
				SELECT workspace_id, id, 'system', 'status_changed', '{"from":"agent_review","to":"in_progress"}'::jsonb FROM issue WHERE id = $1`, c.issueID)
		}},
		"root issue": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `UPDATE issue SET parent_issue_id = NULL WHERE id = $1`, c.issueID)
		}},
		"open decision request": {func(t *testing.T, c autoClose) {
			session := dbfx.ChatSession(t, c.agentID, testutil.Cols{"creator_id": testUserID})
			id := dbfx.Insert(t, "decision_request", testutil.Cols{
				"workspace_id": testWorkspaceID, "issue_id": c.issueID, "asking_agent_id": c.agentID, "human_user_id": testUserID,
				"chat_session_id": session, "ask": "Waive?", "escalation_class": "reversible", "origin": "asked", "status": "open",
			})
			dbfx.Cleanup(t, `DELETE FROM decision_request WHERE id = $1`, id)
		}},
		"enabled wakeup": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, instruction, kind, mode, enabled)
				SELECT gen_random_uuid(), workspace_id, id, $2, 'look again', 'event', 'continuous', true FROM issue WHERE id = $1`, c.issueID, c.agentID)
			dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, c.issueID)
		}},
		"Blockers: Cheese stop": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `UPDATE issue SET description = E'Blockers: Cheese stop CHE-1\nrest' WHERE id = $1`, c.issueID)
		}},
		"parent delivery_stop stopped": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `UPDATE issue SET metadata = '{"delivery_stop":"stopped"}'::jsonb WHERE id = $1`, c.parentID)
		}},
		"no post_close_work": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `UPDATE issue SET metadata = '{}'::jsonb WHERE id = $1`, c.issueID)
		}},
		"post_close_work names work": {func(t *testing.T, c autoClose) {
			dbfx.Exec(t, `UPDATE issue SET metadata = '{"post_close_work":"deploy to C00"}'::jsonb WHERE id = $1`, c.issueID)
		}},
		"open child": {func(t *testing.T, c autoClose) {
			dbfx.Issue(t, "open child", testutil.Cols{"project_id": c.projectID, "status": "in_progress", "parent_issue_id": c.issueID})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newAutoClose(t, "block")
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			tc.setup(t, c)
			c.enter(t)
			c.wantHandlerRun(t)
		})
	}
}

// A no-PR candidate is the issue's text: description, comments, attachments.
// Only the reviewer's own comments and the platform's follow a PASS without
// changing the candidate.
func TestAutoClose_TextCandidateUnchangedSinceThePassCloses(t *testing.T) {
	c := newAutoClose(t, "block")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	c.comment(t, "agent", c.acceptor, "3 hours", "3 hours")
	c.comment(t, "agent", c.agentID, "1 minute", "1 minute")
	c.comment(t, "system", c.acceptor, "1 minute", "1 minute")
	c.enter(t)
	c.wantClosed(t)
}

// Freshness that cannot be read keeps the handler run.
func TestAutoClose_UnreadableVerdictKeepsTheHandlerRun(t *testing.T) {
	c := newAutoClose(t, "block")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	dbfx.Exec(t, `UPDATE activity_log SET details = '{"findings":"not json"}'::jsonb WHERE issue_id = $1 AND action = 'review_passed'`, c.issueID)
	c.enter(t)
	c.wantHandlerRun(t)
}

func TestAutoClose_WorkflowWithoutDoneEdgeIsNeverClosed(t *testing.T) {
	c := newAutoClose(t, "block")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	wf := createTestWorkflowSteps(t, "No done edge "+workflowTestSuffix(), []map[string]any{
		{"status_key": "in_progress"}, {"status_key": "agent_review"}, {"status_key": "agent_accepted"}, {"status_key": "blocked"}, {"status_key": "done"},
	})
	setProjectWorkflow(t, c.projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, c.projectID, "agent_accepted", handToAgent(c.acceptor, "Accept it.")).Want(http.StatusOK)
	c.enter(t)
	c.wantHandlerRun(t)
}

// warn: the run happens as today, and one row says what block would have done.
func TestAutoClose_WarnKeepsTheRunAndLogsTheWouldClose(t *testing.T) {
	for name, mode := range map[string]string{"warn": "warn", "unset reads as the default": ""} {
		t.Run(name, func(t *testing.T) {
			c := newAutoClose(t, mode)
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			c.enter(t)
			c.wantHandlerRun(t)
			if n := c.activities(t, "auto_close_would_close"); n != 1 {
				t.Fatalf("auto_close_would_close rows = %d, want 1", n)
			}
			var details string
			dbfx.QueryRow(t, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'auto_close_would_close'`, c.issueID).Scan(&details)
			if !strings.Contains(details, "run_id") {
				t.Fatalf("details %s do not name the run that followed", details)
			}
		})
	}
}

func TestAutoClose_OffAndUnreadableSettingRunAsToday(t *testing.T) {
	for name, value := range map[string]string{"off": `{"mode":"off"}`, "bare string": `"nonsense"`, "unknown mode": `{"mode":"nonsense"}`} {
		t.Run(name, func(t *testing.T) {
			c := newAutoClose(t, "")
			dbfx.Exec(t, `UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || jsonb_build_object('auto_close', $2::jsonb) WHERE id = $1`, testWorkspaceID, value)
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			c.enter(t)
			c.wantHandlerRun(t)
			if n := c.activities(t, "auto_close_would_close"); n != 0 {
				t.Fatalf("would_close rows = %d, want 0", n)
			}
		})
	}
}

// The auto_close field patches one settings key, takes off|warn|block, is owner
// only, and leaves every other key alone.
func TestAutoClose_SettingFieldPatchesOneKeyOwnerOnly(t *testing.T) {
	setAutoCloseMode(t, "") // restores the saved settings on cleanup
	dbfx.Exec(t, `UPDATE workspace SET settings = '{"compact_prompt":"Keep SHAs."}'::jsonb WHERE id = $1`, testWorkspaceID)
	patch := func(value any, want int) map[string]any {
		t.Helper()
		request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{"auto_close": value}), "id", testWorkspaceID)
		testutil.Call(t, testHandler.UpdateWorkspace, request).Want(want)
		var settings map[string]any
		var raw []byte
		dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&raw)
		if err := json.Unmarshal(raw, &settings); err != nil {
			t.Fatal(err)
		}
		return settings
	}
	for _, mode := range []string{"warn", "block", "off"} {
		settings := patch(map[string]any{"mode": mode}, http.StatusOK)
		if !reflect.DeepEqual(settings["auto_close"], map[string]any{"mode": mode}) || settings["compact_prompt"] != "Keep SHAs." {
			t.Fatalf("mode %s: settings = %v", mode, settings)
		}
	}
	for _, invalid := range []any{map[string]any{"mode": "ON"}, map[string]any{}, map[string]any{"mode": "warn", "x": 1}, "warn", 42} {
		if settings := patch(invalid, http.StatusBadRequest); !reflect.DeepEqual(settings["auto_close"], map[string]any{"mode": "off"}) {
			t.Fatalf("invalid %v changed the saved value: %v", invalid, settings["auto_close"])
		}
	}
	setRuntimeTestMemberRole(t, testUserID, "admin")
	t.Cleanup(func() { setRuntimeTestMemberRole(t, testUserID, "owner") })
	if settings := patch(map[string]any{"mode": "block"}, http.StatusForbidden); !reflect.DeepEqual(settings["auto_close"], map[string]any{"mode": "off"}) {
		t.Fatalf("a refused write changed auto_close: %v", settings["auto_close"])
	}
}

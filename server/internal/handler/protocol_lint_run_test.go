package handler

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestCompleteTask_PersistsProtocolLintRun_WithViolation is CHE-552's
// regression test for recordProtocolLintRun: a completing task whose
// already-persisted comment thread trips protocollint's reply-parent-
// mismatch assertion (CodeReplyParentMismatch) must produce a
// protocol_lint_run row recording that violation, not just a log line.
//
// The write-time gate (taskCoversReplyParent in comment.go) rejects a
// mismatched reply at CreateComment time, so a genuinely mismatched parent
// can only ever exist in this table via a path that bypassed that gate — this
// test constructs that scenario directly against the DB (as
// protocollint.go's own doc explains the "defense in depth" framing), the
// same way a write-time-gate regression would surface here.
func TestCompleteTask_PersistsProtocolLintRun_WithViolation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	var agentID, runtimeID string
	dbfx.QueryRow(t, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID)

	issueID := dbfx.Issue(t, "che-552 protocol lint violation fixture", testutil.Cols{
		"status": "in_progress",
		"number": 85201,
	})

	triggerCommentID := dbfx.Comment(t, issueID, "please take a look")

	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":         runtimeID,
		"issue_id":           issueID,
		"trigger_comment_id": triggerCommentID,
		"status":             "running",
		"started_at":         testutil.Raw("now()"),
	})

	// A comment authored by this run (source_task_id = taskID) but with no
	// parent_id at all — top-level, not covered by the trigger. This is
	// exactly checkReplyParent's first branch in protocollint.go.
	dbfx.Comment(t, issueID, "unrelated top-level reply from the run", testutil.Cols{
		"author_type":    "agent",
		"author_id":      agentID,
		"source_task_id": taskID,
	})

	// recordProtocolLintRun writes directly via h.Queries, bypassing
	// dbfx.Insert's cleanup tracking, so this row needs its own teardown.
	dbfx.Cleanup(t, `DELETE FROM protocol_lint_run WHERE task_id = $1`, taskID)

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+taskID+"/complete",
		map[string]any{"output": "Done."},
		testWorkspaceID, "legit-daemon")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.CompleteTask(w, req)
	if w.Code != 200 {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var violationCount int
	var violationCodes []string
	dbfx.QueryRow(t, `
		SELECT violation_count, violation_codes FROM protocol_lint_run
		WHERE task_id = $1
	`, taskID).Scan(&violationCount, &violationCodes)

	if violationCount != 1 {
		t.Fatalf("protocol_lint_run.violation_count = %d, want 1", violationCount)
	}
	if len(violationCodes) != 1 || violationCodes[0] != "reply_parent_mismatch" {
		t.Fatalf("protocol_lint_run.violation_codes = %v, want [reply_parent_mismatch]", violationCodes)
	}
}

// TestCompleteTask_PersistsProtocolLintRun_Clean is the companion regression
// test: an assignment-triggered completion with no comment activity to
// violate any assertion must still persist a protocol_lint_run row —
// violation_count = 0, violation_codes empty — so the report's denominator
// counts every checked turn, not only the ones with violations.
func TestCompleteTask_PersistsProtocolLintRun_Clean(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	var agentID, runtimeID string
	dbfx.QueryRow(t, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID)

	issueID := dbfx.Issue(t, "che-552 protocol lint clean fixture", testutil.Cols{
		"status": "in_progress",
		"number": 85202,
	})

	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID,
		"issue_id":   issueID,
		"status":     "running",
		"started_at": testutil.Raw("now()"),
	})

	dbfx.Cleanup(t, `DELETE FROM protocol_lint_run WHERE task_id = $1`, taskID)

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+taskID+"/complete",
		map[string]any{"output": "Done."},
		testWorkspaceID, "legit-daemon")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.CompleteTask(w, req)
	if w.Code != 200 {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var violationCount int
	var violationCodes []string
	dbfx.QueryRow(t, `
		SELECT violation_count, violation_codes FROM protocol_lint_run
		WHERE task_id = $1
	`, taskID).Scan(&violationCount, &violationCodes)

	if violationCount != 0 {
		t.Fatalf("protocol_lint_run.violation_count = %d, want 0", violationCount)
	}
	if len(violationCodes) != 0 {
		t.Fatalf("protocol_lint_run.violation_codes = %v, want empty", violationCodes)
	}
}

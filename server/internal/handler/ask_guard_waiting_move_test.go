package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// S5 (CHE-7290): an agent that moves an issue into in_review, blocked or
// need_decision must say who acts next with waiting_on ("member:<id>" or
// "none"), unless its task already made a typed ask. The rule never reads the
// text of earlier comments.

func setWaitingOnMode(t *testing.T, mode string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'), '{waiting_on_guard}', to_jsonb($1::text)) WHERE id = $2`,
		mode, testWorkspaceID); err != nil {
		t.Fatalf("set waiting_on_guard: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - 'waiting_on_guard' WHERE id = $1`, testWorkspaceID)
	})
}

func (f askGuardCommentFixture) move(t *testing.T, status string, extra map[string]any) *testutil.Response {
	t.Helper()
	body := map[string]any{"status": status}
	for k, v := range extra {
		body[k] = v
	}
	return agentUpdateIssue(t, f.issueID, f.agentID, f.taskID, body)
}

func (f askGuardCommentFixture) moveHits(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1 AND operation_kind = 'waiting_move'`, f.issueID)
}

func (f askGuardCommentFixture) status(t *testing.T) string {
	t.Helper()
	var s string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, f.issueID).Scan(&s)
	return s
}

// addWorkflowStatuses adds the custom statuses the Default workflow uses, which
// a test workspace does not seed.
func addWorkflowStatuses(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO issue_status (workspace_id, key, name, description, category, color, position) VALUES
		($1, 'need_decision', 'Need Decision', '', 'started', '#ff0000', 90),
		($1, 'agent_review', 'Agent Review', '', 'started', '#ff0000', 91)
		ON CONFLICT DO NOTHING`, testWorkspaceID)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM issue_status WHERE workspace_id = $1 AND key IN ('need_decision', 'agent_review')`, testWorkspaceID)
	})
}

// newWaitingMoveFixture starts the issue in in_progress, the status an agent
// moves from.
func newWaitingMoveFixture(t *testing.T) askGuardCommentFixture {
	t.Helper()
	addWorkflowStatuses(t)
	f := newAskGuardCommentFixture(t)
	f.setStatus(t, "in_progress")
	return f
}

// Class B of the CHE-6115 replay: the unmarked comment came first, while the
// issue was in_progress (S4 does not see it), and the same task then moved the
// issue. The move is what S5 judges; the earlier text is never read.
func TestWaitingMoveBlockRefusesMoveAfterEarlierUnmarkedComment(t *testing.T) {
	setWaitingOnMode(t, "block")
	for _, status := range []string{"in_review", "blocked", "need_decision"} {
		t.Run(status, func(t *testing.T) {
			f := newWaitingMoveFixture(t)
			f.post(t, map[string]any{"content": waitingPlainAsk}).Want(http.StatusCreated)
			res := f.move(t, status, nil).Want(http.StatusUnprocessableEntity)
			for _, fix := range []string{"--waiting-on member:", "--waiting-on none"} {
				if !strings.Contains(res.Text(), fix) {
					t.Errorf("422 body lacks %q: %s", fix, res.Text())
				}
			}
			if got := f.status(t); got != "in_progress" {
				t.Errorf("refused move changed status to %q", got)
			}
			if n := f.moveHits(t); n != 1 {
				t.Errorf("hits = %d, want 1", n)
			}
		})
	}
}

func TestWaitingMoveWarnRecordsHitAndMoves(t *testing.T) {
	setWaitingOnMode(t, "warn")
	f := newWaitingMoveFixture(t)
	f.post(t, map[string]any{"content": waitingPlainAsk}).Want(http.StatusCreated)
	f.move(t, "in_review", nil).Want(http.StatusOK)
	if got := f.status(t); got != "in_review" {
		t.Errorf("status = %q, want in_review", got)
	}
	if n := f.moveHits(t); n != 1 {
		t.Errorf("warn hits = %d, want 1", n)
	}
}

func TestWaitingMoveOffWritesNothing(t *testing.T) {
	f := newWaitingMoveFixture(t)
	f.move(t, "in_review", nil).Want(http.StatusOK)
	if n := f.moveHits(t); n != 0 {
		t.Errorf("off hits = %d, want 0", n)
	}
}

func TestWaitingMoveTypedTargetPasses(t *testing.T) {
	setWaitingOnMode(t, "block")
	for name, waitingOn := range map[string]func(f askGuardCommentFixture) string{
		"member": func(f askGuardCommentFixture) string { return "member:" + f.humanID },
		"none":   func(askGuardCommentFixture) string { return "none" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newWaitingMoveFixture(t)
			f.post(t, map[string]any{"content": waitingPlainAsk}).Want(http.StatusCreated)
			f.move(t, "in_review", map[string]any{"waiting_on": waitingOn(f)}).Want(http.StatusOK)
			if n := f.moveHits(t); n != 0 {
				t.Errorf("typed move wrote %d hits", n)
			}
		})
	}
}

// A task that already made a typed ask needs no second declaration.
func TestWaitingMoveTaskAskPasses(t *testing.T) {
	setWaitingOnMode(t, "block")
	t.Run("member mention", func(t *testing.T) {
		f := newWaitingMoveFixture(t)
		f.post(t, map[string]any{"content": waitingPlainAsk + " " + f.mention(), "notify_only": true}).Want(http.StatusCreated)
		f.move(t, "in_review", nil).Want(http.StatusOK)
	})
	t.Run("decision request", func(t *testing.T) {
		f := newWaitingMoveFixture(t)
		dbfx.Exec(t, `INSERT INTO comment (id, issue_id, workspace_id, author_type, author_id, content, type, source_task_id, decision_request_id, decision_comment_kind)
			VALUES ($1, $2, $3, 'agent', $4, 'ask', 'comment', $5, $6, 'ask')`,
			uuid.NewString(), f.issueID, testWorkspaceID, f.agentID, f.taskID, uuid.NewString())
		f.move(t, "in_review", nil).Want(http.StatusOK)
	})
	t.Run("member mention by another task does not count", func(t *testing.T) {
		f := newWaitingMoveFixture(t)
		otherTask := dbfx.Task(t, f.agentID, testutil.Cols{"issue_id": f.issueID, "status": "completed", "runtime_id": handlerTestRuntimeID(t)})
		f.postAs(t, otherTask, map[string]any{"content": f.mention(), "notify_only": true}).Want(http.StatusCreated)
		f.move(t, "in_review", nil).Want(http.StatusUnprocessableEntity)
	})
}

func TestWaitingMoveOtherMovesAndActorsPass(t *testing.T) {
	setWaitingOnMode(t, "block")
	for _, status := range []string{"todo", "agent_review", "done"} {
		t.Run("agent to "+status, func(t *testing.T) {
			f := newWaitingMoveFixture(t)
			f.move(t, status, nil).Want(http.StatusOK)
			if n := f.moveHits(t); n != 0 {
				t.Errorf("hits = %d, want 0", n)
			}
		})
	}
	t.Run("already waiting is not a move", func(t *testing.T) {
		f := newAskGuardCommentFixture(t)
		f.setStatus(t, "in_review")
		f.move(t, "in_review", nil).Want(http.StatusOK)
		if n := f.moveHits(t); n != 0 {
			t.Errorf("hits = %d, want 0", n)
		}
	})
	t.Run("member move", func(t *testing.T) {
		f := newWaitingMoveFixture(t)
		req := withURLParam(newRequest(http.MethodPut, "/api/issues/"+f.issueID, map[string]any{"status": "in_review"}), "id", f.issueID)
		testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
		if n := f.moveHits(t); n != 0 {
			t.Errorf("member move wrote %d hits", n)
		}
	})
}

func TestWaitingMoveBadWaitingOnIs400(t *testing.T) {
	f := newWaitingMoveFixture(t)
	for name, value := range map[string]string{
		"unknown word":     "nobody",
		"member not uuid":  "member:cheese",
		"member not in ws": "member:00000000-0000-4000-8000-000000000000",
		"empty member":     "member:",
	} {
		t.Run(name, func(t *testing.T) {
			f.move(t, "in_review", map[string]any{"waiting_on": value}).Want(http.StatusBadRequest)
			if got := f.status(t); got != "in_progress" {
				t.Errorf("refused move changed status to %q", got)
			}
		})
	}
}

// Chat-lane runs have no run to recover through: block only warns.
func TestWaitingMoveChatLaneWarnsOnly(t *testing.T) {
	setWaitingOnMode(t, "block")
	f := newWaitingMoveFixture(t)
	session := dbfx.ChatSession(t, f.agentID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, session, f.taskID)
	f.move(t, "in_review", nil).Want(http.StatusOK)
	if n := f.moveHits(t); n != 1 {
		t.Errorf("hits = %d, want 1", n)
	}
}

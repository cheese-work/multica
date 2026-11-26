package handler

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9761: FailTask accepts the daemon's timeout_checkpoint, posts one
// checkpoint comment per failed task before the retry is minted, and the
// retry's claim points at it.

const fakeSecret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

func timeoutCheckpointBody(warning map[string]any, actions []map[string]any) map[string]any {
	return map[string]any{
		"repos": []map[string]any{{
			"path": "multica", "branch": "agent/x/che-1", "head": strings.Repeat("a", 40), "ahead": 2, "dirty": 3,
		}},
		"last_actions":     actions,
		"total_tool_calls": 41,
		"warning":          warning,
	}
}

func failTaskWith(t *testing.T, taskID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+taskID+"/fail", body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	testHandler.FailTask(w, req)
	return w
}

type timeoutFixture struct {
	agentID, runtimeID, daemonID, issueID, taskID string
}

func newTimeoutFixture(t *testing.T) timeoutFixture {
	t.Helper()
	ctx := context.Background()
	agentID, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)
	issueID := dbfx.Issue(t, "timeout checkpoint fixture", testutil.Cols{"status": "in_progress"})
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "running",
		"started_at": testutil.Raw("now() - interval '60 minutes'"),
		"attempt":    1, "max_attempts": 2,
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
	})
	return timeoutFixture{agentID, runtimeID, daemonID, issueID, taskID}
}

func (f timeoutFixture) comments(t *testing.T, where string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT content FROM comment WHERE issue_id = $1 AND source_task_id = $2 `+where+` ORDER BY created_at`, f.issueID, f.taskID)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (f timeoutFixture) taskCount(t *testing.T) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, f.issueID).Scan(&n)
	return n
}

// (a) (b) (c) (d) (g) (h)
func TestFailTask_TimeoutCheckpoint_RetriedPostsOneCheckpoint(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	f := newTimeoutFixture(t)
	const errText = "codex timed out after 1h0m0s"
	body := map[string]any{
		"error": errText, "failure_reason": "timeout", "session_id": "sess-resume-1",
		"timeout_checkpoint": timeoutCheckpointBody(
			map[string]any{"state": "delivered", "at": "2026-11-25T02:25:00Z"},
			[]map[string]any{{"tool": "Bash", "input": "git push " + fakeSecret, "time": "2026-11-25T02:30:00Z"}},
		),
	}

	// A malformed duplicate must not add a comment or break the fail.
	if w := failTaskWith(t, f.taskID, body); w.Code != 200 {
		t.Fatalf("FailTask = %d: %s", w.Code, w.Body)
	}
	tasksAfterFirst := f.taskCount(t)
	if w := failTaskWith(t, f.taskID, body); w.Code != 200 {
		t.Fatalf("duplicate FailTask = %d: %s", w.Code, w.Body)
	}

	// (a) exactly one checkpoint, no failure notice; (b) duplicate adds none.
	all := f.comments(t, "")
	if len(all) != 1 {
		t.Fatalf("comments for the task = %d (%q), want exactly the checkpoint", len(all), all)
	}
	cp := all[0]
	if !strings.HasPrefix(cp, "Coverage: agent checkpoint requested at 09:25 ICT; daemon facts below\n") {
		t.Fatalf("first line wrong:\n%s", cp)
	}
	for _, want := range []string{strings.Repeat("a", 40), "agent/x/che-1", "(last 1 of 41 tool calls)"} {
		if !strings.Contains(cp, want) {
			t.Fatalf("checkpoint missing %q:\n%s", want, cp)
		}
	}
	if strings.Contains(cp, fakeSecret) {
		t.Fatalf("secret not redacted:\n%s", cp)
	}

	// (c) error and failure_reason are what FailTask stored before this change.
	var gotErr, gotReason string
	dbfx.QueryRow(t, `SELECT error, failure_reason FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&gotErr, &gotReason)
	if gotErr != errText || gotReason != "timeout" {
		t.Fatalf("error=%q failure_reason=%q, want %q / timeout", gotErr, gotReason, errText)
	}

	// (g) the comment starts no task: only the failed parent and its one retry exist.
	if tasksAfterFirst != 2 || f.taskCount(t) != 2 {
		t.Fatalf("tasks after fail = %d then %d, want 2 (parent + retry) both times", tasksAfterFirst, f.taskCount(t))
	}

	// (d) + (h): the retry claim points at the comment and keeps the session.
	claimed := claimTaskForRuntimeGuardWithCapabilities(t, f.runtimeID, f.daemonID, "")
	var commentID string
	dbfx.QueryRow(t, `SELECT id::text FROM comment WHERE issue_id = $1 AND source_task_id = $2`, f.issueID, f.taskID).Scan(&commentID)
	if !strings.Contains(claimed.CheckpointBlock, commentID) || !strings.Contains(claimed.CheckpointBlock, "posted after the warning at 09:25 ICT") {
		t.Fatalf("checkpoint_block lacks the pointer with comment id %s:\n%s", commentID, claimed.CheckpointBlock)
	}
	if claimed.PriorSessionID != "sess-resume-1" {
		t.Fatalf("retry PriorSessionID = %q, want the parent's session", claimed.PriorSessionID)
	}
}

// (f)
func TestFailTask_TimeoutCheckpoint_WarningNotDeliveredUsesDaemonFactsForm(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	f := newTimeoutFixture(t)
	actions := []map[string]any{
		{"tool": "Bash", "input": "export TOKEN=" + fakeSecret, "time": "2026-11-25T02:30:00Z"},
		{"tool": "Read", "input": "a.go"},
		{"tool": "Edit", "input": "b.go"},
	}
	w := failTaskWith(t, f.taskID, map[string]any{
		"error": "codex timed out after 1h0m0s", "failure_reason": "timeout",
		"timeout_checkpoint": timeoutCheckpointBody(map[string]any{"state": "not_delivered", "reason": "steer refused"}, actions),
	})
	if w.Code != 200 {
		t.Fatalf("FailTask = %d: %s", w.Code, w.Body)
	}
	got := f.comments(t, "")
	if len(got) != 1 || !strings.HasPrefix(got[0], "Coverage: daemon facts only\n") {
		t.Fatalf("comments = %q, want one daemon-facts-only checkpoint", got)
	}
	for _, want := range []string{"Bash", "Read", "Edit", "a.go", "b.go", "steer refused", "(last 3 of 41 tool calls)"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("checkpoint missing %q:\n%s", want, got[0])
		}
	}
	if strings.Contains(got[0], fakeSecret) {
		t.Fatalf("secret not redacted:\n%s", got[0])
	}
	claimed := claimTaskForRuntimeGuardWithCapabilities(t, f.runtimeID, f.daemonID, "")
	if !strings.Contains(claimed.CheckpointBlock, "daemon facts only") || !strings.Contains(claimed.CheckpointBlock, "normal thread scan") ||
		strings.Contains(claimed.CheckpointBlock, "your own checkpoint") {
		t.Fatalf("pointer must use the daemon-facts form:\n%s", claimed.CheckpointBlock)
	}
}

// (e) + old daemon.
func TestFailTask_TimeoutCheckpoint_MalformedDroppedAndAbsentKeepsOldPath(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, tc := range []struct {
		name  string
		field any
	}{
		{"wrong_types", map[string]any{"repos": "nope", "last_actions": 7, "warning": []int{1}}},
		{"not_an_object", "garbage"},
		{"field_absent", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTimeoutFixture(t)
			body := map[string]any{"error": "codex timed out after 1h0m0s", "failure_reason": "timeout"}
			if tc.field != nil {
				body["timeout_checkpoint"] = tc.field
			}
			if w := failTaskWith(t, f.taskID, body); w.Code != 200 {
				t.Fatalf("FailTask = %d: %s", w.Code, w.Body)
			}
			var status string
			dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&status)
			if status != "failed" {
				t.Fatalf("task status = %q, want failed", status)
			}
			// Neither a malformed nor an absent field posts a comment (the retry
			// is pending, so today's path posts no failure notice either) or a pointer.
			if got := f.comments(t, ""); len(got) != 0 {
				t.Fatalf("posted %q, want no comment", got)
			}
			claimed := claimTaskForRuntimeGuardWithCapabilities(t, f.runtimeID, f.daemonID, "")
			if strings.Contains(claimed.CheckpointBlock, "hit the run cap") {
				t.Fatalf("no checkpoint comment, so no pointer:\n%s", claimed.CheckpointBlock)
			}
		})
	}
}

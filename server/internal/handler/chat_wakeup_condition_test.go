package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1874 PR 2: a DM wakeup can wait for a fact instead of only the clock.
// fire_at is then the deadline; the first of "condition holds" and "deadline"
// starts the turn, once.

func (dm wakeupDM) createWithCondition(t *testing.T, condition map[string]any) *testutil.Response {
	t.Helper()
	return dm.request(t, http.MethodPost, "/api/chat/wakeups",
		map[string]any{"instruction": "Report what changed.", "after_seconds": 3600, "condition": condition}, testHandler.CreateChatWakeup)
}

// triggerText is the hidden input of the wakeup turn the DM started, or "".
func (dm wakeupDM) triggerText(t *testing.T) string {
	t.Helper()
	tasks := dm.wakeupTasks(t, dm.sessionID)
	if len(tasks) == 0 {
		return ""
	}
	var content string
	dbfx.QueryRow(t, `SELECT content FROM chat_message WHERE task_id = $1 AND role = 'user'`, tasks[0]).Scan(&content)
	return content
}

func TestChatWakeupFiresWhenTheWatchedIssueReachesTheStatus(t *testing.T) {
	dm := newWakeupDM(t)
	issue := dbfx.Issue(t, "Watched issue")
	var w wakeupJSON
	dm.createWithCondition(t, map[string]any{"issue": issue, "status": "done"}).Want(http.StatusCreated).JSON(&w)

	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("fired before the condition held: %v", got)
	}
	dbfx.Exec(t, `UPDATE issue SET status = 'done' WHERE id = $1`, issue)
	dm.tick(t)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks, want exactly 1 before the deadline", len(got))
	}
	if status, _ := wakeupStatus(t, w.ID); status != "fired" {
		t.Fatalf("status = %s, want fired", status)
	}
	if text := dm.triggerText(t); !strings.Contains(text, "condition") || !strings.Contains(text, "done") {
		t.Fatalf("prompt does not say the condition held: %s", text)
	}
}

func TestChatWakeupDeadlineFiresWhenTheConditionNeverHolds(t *testing.T) {
	dm := newWakeupDM(t)
	issue := dbfx.Issue(t, "Never done")
	var w wakeupJSON
	dm.createWithCondition(t, map[string]any{"issue": issue, "status": "done"}).Want(http.StatusCreated).JSON(&w)
	dm.makeDue(t, w.ID)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks at the deadline, want 1", len(got))
	}
	if text := dm.triggerText(t); !strings.Contains(text, "deadline") {
		t.Fatalf("prompt does not say the deadline was reached: %s", text)
	}
}

func TestChatWakeupCommentConditionIgnoresOlderComments(t *testing.T) {
	dm := newWakeupDM(t)
	issue := dbfx.Issue(t, "Commented issue")
	dbfx.Comment(t, issue, "before the wakeup")
	dm.createWithCondition(t, map[string]any{"issue": issue, "comment_by": map[string]any{"type": "member", "id": testUserID}}).Want(http.StatusCreated)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("an older comment fired the wakeup: %v", got)
	}
	dbfx.Comment(t, issue, "after the wakeup")
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after a new comment, want 1", len(got))
	}
}

func TestChatWakeupPullRequestChecksNeedANewResult(t *testing.T) {
	dm := newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "PR issue")
	pr := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "wakeup-test",
		"pr_number": time.Now().UnixMilli() % 100000, "title": "PR", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"),
		"snapshot_head_sha": "aaa", "checks_rollup_state": "SUCCESS",
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request(issue_id, pull_request_id) VALUES ($1, $2)`, issue, pr)
	dbfx.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, pr)

	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("checks finished before the wakeup fired it: %v", got)
	}
	dbfx.Exec(t, `UPDATE github_pull_request SET snapshot_head_sha = 'bbb', checks_rollup_state = 'FAILURE' WHERE id = $1`, pr)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after a new finished result, want 1", len(got))
	}
}

func TestChatWakeupConditionValidation(t *testing.T) {
	dm := newWakeupDM(t)
	issue := dbfx.Issue(t, "Valid issue")
	foreign := dbfx.Workspace(t, "Other workspace", "other-"+strings.ToLower(t.Name()))
	foreignIssue := dbfx.Issue(t, "Foreign issue", testutil.Cols{"workspace_id": foreign})
	for name, condition := range map[string]map[string]any{
		"unknown issue":              {"issue": "00000000-0000-4000-8000-000000000000", "status": "done"},
		"issue of another workspace": {"issue": foreignIssue, "status": "done"},
		"unknown status":             {"issue": issue, "status": "no-such-status"},
		"no predicate":               {"issue": issue},
		"two predicates":             {"issue": issue, "status": "done", "pr": "merged"},
		"bad pr event":               {"issue": issue, "pr": "opened"},
		"bad commenter":              {"issue": issue, "comment_by": map[string]any{"type": "bot", "id": testUserID}},
		"no issue":                   {"status": "done"},
	} {
		t.Run(name, func(t *testing.T) { dm.createWithCondition(t, condition).Want(http.StatusBadRequest) })
	}

	// An issue identifier works as well as a UUID, and the response echoes it.
	var identifier string
	dbfx.QueryRow(t, `SELECT ws.issue_prefix || '-' || i.number FROM issue i JOIN workspace ws ON ws.id = i.workspace_id WHERE i.id = $1`, issue).Scan(&identifier)
	body := dm.createWithCondition(t, map[string]any{"issue": identifier, "status": "done"}).Want(http.StatusCreated).Text()
	if !strings.Contains(body, issue) {
		t.Fatalf("response does not carry the resolved issue id: %s", body)
	}
}

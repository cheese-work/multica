package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
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
		"snapshot_head_sha": "aaa", "checks_rollup_state": "SUCCESS", "snapshot_fetched_at": testutil.Raw("now()"),
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

// Regressions for OCR-1874-5 and OCR-1874-6.

func TestChatWakeupEvaluatesEveryWaitingConditionNotOnlyTheFirstBatch(t *testing.T) {
	dm := newWakeupDM(t)
	unmet := dbfx.Issue(t, "Never done")
	satisfied := dbfx.Issue(t, "Already done", testutil.Cols{"status": "done"})
	var identifier string
	dbfx.QueryRow(t, `SELECT ws.issue_prefix || '-' || i.number FROM issue i JOIN workspace ws ON ws.id = i.workspace_id WHERE i.id = $1`, unmet).Scan(&identifier)
	// 50 unmet conditions with earlier deadlines than the satisfied one.
	for i := 0; i < 50; i++ {
		dbfx.Exec(t, `INSERT INTO chat_wakeup (workspace_id, chat_session_id, agent_id, created_by, instruction, fire_at, condition)
			VALUES ($1, $2, $3, $4, 'unmet', now() + interval '30 minutes' + make_interval(secs => $5), $6::jsonb)`,
			testWorkspaceID, dm.sessionID, dm.agentID, testUserID, i,
			`{"issue_id":"`+unmet+`","identifier":"`+identifier+`","check":{"type":"issue_field","field":"status","value":"done"}}`)
	}
	// The satisfied one lives in another DM, so the per-DM cap does not apply.
	other := dm.otherDM(t)
	var w wakeupJSON
	other.createWithCondition(t, map[string]any{"issue": satisfied, "status": "done"}).Want(http.StatusCreated).JSON(&w)

	dm.tick(t)
	if got := other.wakeupTasks(t, other.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after one tick, want the satisfied 51st condition to fire", len(got))
	}
	if status, _ := wakeupStatus(t, w.ID); status != "fired" {
		t.Fatalf("satisfied wakeup status = %s, want fired", status)
	}
}

func linkedPullRequest(t *testing.T, issue, head, checks string) string {
	t.Helper()
	pr := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "wakeup-test",
		"pr_number": time.Now().UnixNano() % 1000000, "title": "PR", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"),
		"snapshot_head_sha": head, "checks_rollup_state": checks, "snapshot_fetched_at": testutil.Raw("now()"),
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request(issue_id, pull_request_id) VALUES ($1, $2)`, issue, pr)
	dbfx.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, pr)
	return pr
}

// linkedUnsnapshottedPullRequest is a pull request that is linked but has no API
// snapshot yet: the mirror knows its head, and nothing about its checks.
func linkedUnsnapshottedPullRequest(t *testing.T, issue string) string {
	t.Helper()
	pr := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "wakeup-test",
		"pr_number": time.Now().UnixNano() % 1000000, "title": "PR", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"), "head_sha": "aaa",
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request(issue_id, pull_request_id) VALUES ($1, $2)`, issue, pr)
	dbfx.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, pr)
	return pr
}

// snapshotArrives models a successful snapshot refresh: the rollup state, the
// fetch time and one finished check run with a node id.
func snapshotArrives(t *testing.T, pr, rollup, externalID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE github_pull_request SET snapshot_head_sha = 'aaa', checks_rollup_state = $2, snapshot_fetched_at = now() WHERE id = $1`, pr, rollup)
	setContexts(t, pr, "aaa", []int{0}, checkContext{name: "build", url: "https://ci.example/build/1", conclusion: "success", externalID: externalID})
}

func TestChatWakeupPullRequestChecksSeeARerunOnTheSameHead(t *testing.T) {
	dm := newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "Rerun issue")
	pr := linkedPullRequest(t, issue, "aaa", "SUCCESS")
	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated)

	dm.tick(t)
	dbfx.Exec(t, `UPDATE github_pull_request SET checks_rollup_state = 'PENDING' WHERE id = $1`, pr)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("fired while the rerun was still pending: %v", got)
	}
	dbfx.Exec(t, `UPDATE github_pull_request SET checks_rollup_state = 'SUCCESS' WHERE id = $1`, pr)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after the same-head rerun finished, want 1", len(got))
	}
}

func TestChatWakeupUnlinkingAnOldPullRequestIsNotANewResult(t *testing.T) {
	dm := newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "Unlink issue")
	first := linkedPullRequest(t, issue, "aaa", "SUCCESS")
	linkedPullRequest(t, issue, "bbb", "FAILURE")
	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated)

	dm.tick(t)
	dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, first)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("unlinking an old finished pull request fired the wakeup: %v", got)
	}
}

// OCR-1874-6, residual: a rerun on the same head that starts and finishes
// between two scheduler ticks is never seen running. Every rerun creates new
// check runs with new job ids, so the details URLs are the durable identity of
// a finished result.
func setCheckRuns(t *testing.T, pr, head, conclusion string, jobIDs ...string) {
	t.Helper()
	dbfx.Exec(t, `DELETE FROM github_pull_request_check_run WHERE pr_id = $1`, pr)
	for i, job := range jobIDs {
		dbfx.Exec(t, `INSERT INTO github_pull_request_check_run (pr_id, head_sha, ordinal, name, status, conclusion, details_url, external_id)
			VALUES ($1, $2, $3, $4, 'completed', $5, $6, $7)`, pr, head, i, "job-"+strconv.Itoa(i), conclusion, "https://github.com/o/r/actions/runs/1/job/"+job, "CR_"+job)
	}
	dbfx.Cleanup(t, `DELETE FROM github_pull_request_check_run WHERE pr_id = $1`, pr)
}

func TestChatWakeupPullRequestChecksSeeARerunThatFinishedBetweenPolls(t *testing.T) {
	dm := newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "Fast rerun issue")
	pr := linkedPullRequest(t, issue, "aaa", "SUCCESS")
	setCheckRuns(t, pr, "aaa", "success", "111", "112")
	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated)

	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("fired on the result that existed when the wakeup was set: %v", got)
	}
	// The whole rerun happens between two ticks: new job ids, same head, same SUCCESS.
	setCheckRuns(t, pr, "aaa", "success", "211", "212")
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after a same-head rerun finished between ticks, want 1", len(got))
	}
}

// OCR-1874-6, second round: the identity of a finished result must not depend
// on the order GitHub returns the contexts in, and must name a result even when
// the name, state, URL and timestamp of a rerun match the old one. The GraphQL
// node id does: every rerun creates new check runs and every status posted
// creates a new status object.
type checkContext struct {
	name, url, conclusion, externalID string
	statusContext                     bool
}

func setContexts(t *testing.T, pr, head string, order []int, contexts ...checkContext) {
	t.Helper()
	dbfx.Exec(t, `DELETE FROM github_pull_request_check_run WHERE pr_id = $1`, pr)
	for i, c := range contexts {
		var external any
		if c.externalID != "" {
			external = c.externalID
		}
		dbfx.Exec(t, `INSERT INTO github_pull_request_check_run (pr_id, head_sha, ordinal, name, status, conclusion, details_url, is_status_context, external_id)
			VALUES ($1, $2, $3, $4, 'completed', $5, $6, $7, $8)`, pr, head, order[i], c.name, c.conclusion, c.url, c.statusContext, external)
	}
	dbfx.Cleanup(t, `DELETE FROM github_pull_request_check_run WHERE pr_id = $1`, pr)
}

func newChecksWakeup(t *testing.T, contexts ...checkContext) (dm wakeupDM, pr string, order []int, issue string) {
	t.Helper()
	dm = newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue = dbfx.Issue(t, "Checks issue")
	pr = linkedPullRequest(t, issue, "aaa", "SUCCESS")
	for i := range contexts {
		order = append(order, i)
	}
	setContexts(t, pr, "aaa", order, contexts...)
	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("fired on the result that existed when the wakeup was set: %v", got)
	}
	return dm, pr, order, issue
}

func TestChatWakeupPullRequestChecksIgnoreAReorderingOfTheSameContexts(t *testing.T) {
	a := checkContext{name: "build", url: "https://github.com/o/r/actions/runs/1/job/1", conclusion: "success", externalID: "CR_1"}
	b := checkContext{name: "test", url: "https://github.com/o/r/actions/runs/1/job/2", conclusion: "success", externalID: "CR_2"}
	dm, pr, _, _ := newChecksWakeup(t, a, b)
	// The same two finished results come back in the other order.
	setContexts(t, pr, "aaa", []int{1, 0}, a, b)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("a reordering of the same finished contexts fired the wakeup: %v", got)
	}
}

// A snapshot refresh that rewrites the same results is not a new result.
func TestChatWakeupPullRequestChecksIgnoreARefreshThatRewritesTheSameResults(t *testing.T) {
	a := checkContext{name: "ci/legacy", url: "https://ci.example/build/7", conclusion: "success", externalID: "SC_7", statusContext: true}
	dm, pr, order, _ := newChecksWakeup(t, a)
	setContexts(t, pr, "aaa", order, a)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("a refresh that rewrote the same result fired the wakeup: %v", got)
	}
}

// The strongest case: the reposted status has the same name, state, target URL
// and creation second as the old one. Only its node id differs.
func TestChatWakeupPullRequestChecksSeeAStatusRerunWithTheSameURLAndTimestamp(t *testing.T) {
	status := checkContext{name: "ci/legacy", url: "https://ci.example/build/7", conclusion: "success", externalID: "SC_7", statusContext: true}
	dm, pr, order, _ := newChecksWakeup(t, status)
	status.externalID = "SC_8"
	setContexts(t, pr, "aaa", order, status)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after a reposted status with identical fields, want 1", len(got))
	}
}

// Check data written before node ids were stored cannot name a result, so a
// baseline taken from it could later either fire on a metadata-only backfill or
// swallow a real change. The wakeup is refused until a refresh stored the ids.
func TestChatWakeupChecksConditionWaitsForCheckDataThatCarriesIds(t *testing.T) {
	dm := newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "Legacy rows issue")
	pr := linkedPullRequest(t, issue, "aaa", "SUCCESS")
	legacy := checkContext{name: "build", url: "https://github.com/o/r/actions/runs/1/job/1", conclusion: "success"}
	setContexts(t, pr, "aaa", []int{0}, legacy)

	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusConflict)
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_wakeup WHERE chat_session_id = $1`, dm.sessionID); n != 0 {
		t.Fatalf("a refused wakeup left %d rows", n)
	}

	// The refresh stores the ids. The wakeup can be set now, and a real rerun fires it.
	legacy.externalID = "CR_1"
	setContexts(t, pr, "aaa", []int{0}, legacy)
	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("fired on the result that existed when the wakeup was set: %v", got)
	}
	legacy.externalID = "CR_2"
	setContexts(t, pr, "aaa", []int{0}, legacy)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after a real rerun, want 1", len(got))
	}
}

// OCR-1874-7: a checks condition set before the pull request's first snapshot
// has no baseline. The first snapshot may reveal a result that finished before
// the wakeup existed, which must not fire it, or one that finished after, which
// must. The data cannot say which, so the wakeup waits for the snapshot: creating
// it answers 409 and enqueues a refresh, and the retry takes a precise baseline.
// With the old behavior the condition was created and fired on the old result.
func TestChatWakeupCreatedBeforeTheFirstSnapshotDoesNotFireOnAnExistingResult(t *testing.T) {
	dm := newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "First snapshot issue")
	pr := linkedUnsnapshottedPullRequest(t, issue)
	create := func() *testutil.Response {
		return dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"})
	}

	if first := create(); first.Code == http.StatusConflict {
		if n := dbfx.Count(t, `SELECT count(*) FROM chat_wakeup WHERE chat_session_id = $1`, dm.sessionID); n != 0 {
			t.Fatalf("a refused wakeup left %d rows", n)
		}
		snapshotArrives(t, pr, "SUCCESS", "CR_existing") // the refresh the 409 asked for
		create().Want(http.StatusCreated)
	} else {
		first.Want(http.StatusCreated)
		snapshotArrives(t, pr, "SUCCESS", "CR_existing") // the old behavior: created first
	}
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("%d wakeup task(s) fired from the first snapshot of a result that existed when the wakeup was set, want none", len(got))
	}
	// A genuinely new result after that still fires.
	snapshotArrives(t, pr, "SUCCESS", "CR_rerun")
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after a genuine new result, want 1", len(got))
	}
}

// A pull request whose snapshot exists but has no checks yet has nothing to
// adopt: its first finished result is new, and fires.
func TestChatWakeupOnAPullRequestWithNoChecksYetFiresOnTheFirstResult(t *testing.T) {
	dm := newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "No checks yet issue")
	pr := linkedUnsnapshottedPullRequest(t, issue)
	// A snapshot was fetched and the pull request has no checks (statusCheckRollup was null).
	dbfx.Exec(t, `UPDATE github_pull_request SET snapshot_head_sha = 'aaa', snapshot_fetched_at = now() WHERE id = $1`, pr)
	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("fired before any check finished: %v", got)
	}
	snapshotArrives(t, pr, "SUCCESS", "CR_first")
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after the first finished result, want 1", len(got))
	}
}

// The 409 names the pull requests to refresh, so the handler can enqueue them.
// The enqueue itself is not covered: the refresh manager is not enabled in the
// handler tests, so the handler skips it.
func TestChecksNotReadyErrorNamesThePullRequestsToRefresh(t *testing.T) {
	dm := newWakeupDM(t)
	issue := dbfx.Issue(t, "Refresh address issue")
	pr := linkedUnsnapshottedPullRequest(t, issue)
	var number int32
	dbfx.QueryRow(t, `SELECT pr_number FROM github_pull_request WHERE id = $1`, pr).Scan(&number)

	_, err := testHandler.chatWakeups().Create(context.Background(), parseUUID(dm.sessionID), parseUUID(dm.taskID), "x", time.Now().Add(time.Hour), time.Now(),
		&service.ChatWakeupConditionInput{Issue: parseUUID(issue), PR: "checks_finished"})
	var notReady service.ChecksNotReadyError
	if !errors.As(err, &notReady) || len(notReady.PRs) != 1 {
		t.Fatalf("err = %v, want a ChecksNotReadyError naming one pull request", err)
	}
	if got := notReady.PRs[0]; got.InstallationID != 1 || got.Owner != "multica-ai" || got.Repo != "wakeup-test" || got.Number != number {
		t.Fatalf("address = %+v", got)
	}
}

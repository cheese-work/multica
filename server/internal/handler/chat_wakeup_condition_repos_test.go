package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1874 LUNA-P1-1: the checks baseline and the armed list were keyed by the
// pull request number alone, but linked pull requests of different repositories
// can share a number. One repository's baseline then overwrote the other's and
// a result that already existed looked new.

func linkPullRequest(t *testing.T, issue, owner, repo string, number int64, head, state string) string {
	t.Helper()
	pr := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": owner, "repo_name": repo,
		"pr_number": number, "title": "PR", "state": "open", "html_url": "https://example.test/" + owner + "/" + repo,
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"),
		"snapshot_head_sha": head, "checks_rollup_state": state, "snapshot_fetched_at": testutil.Raw("now()"),
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request(issue_id, pull_request_id) VALUES ($1, $2)`, issue, pr)
	dbfx.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, pr)
	return pr
}

// twoRepositoriesSameNumber links one finished pull request in each of two
// repositories, both numbered alike, to one issue and sets a checks wakeup.
func twoRepositoriesSameNumber(t *testing.T) (dm wakeupDM, wakeupID, prA, prB string, number int64) {
	t.Helper()
	dm = newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "Two repositories")
	number = time.Now().UnixMilli() % 100000
	prA = linkPullRequest(t, issue, "multica-ai", "repo-a", number, "aaa", "SUCCESS")
	prB = linkPullRequest(t, issue, "multica-ai", "repo-b", number, "bbb", "FAILURE")
	var w wakeupJSON
	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated).JSON(&w)
	return dm, w.ID, prA, prB, number
}

func TestChatWakeupChecksKeepSameNumberedPullRequestsOfTwoRepositoriesApart(t *testing.T) {
	dm, _, _, prB, _ := twoRepositoriesSameNumber(t)

	dm.tick(t)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("results that existed before the wakeup fired it: %v", got)
	}

	dbfx.Exec(t, `UPDATE github_pull_request SET snapshot_head_sha = 'ccc', checks_rollup_state = 'SUCCESS' WHERE id = $1`, prB)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after a new result in one repository, want 1", len(got))
	}
}

func TestChatWakeupArmedListKeepsSameNumberedPullRequestsOfTwoRepositoriesApart(t *testing.T) {
	dm := newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "Running in one repository")
	number := time.Now().UnixMilli() % 100000
	running := linkPullRequest(t, issue, "multica-ai", "repo-a", number, "aaa", "PENDING")
	linkPullRequest(t, issue, "multica-ai", "repo-b", number, "bbb", "SUCCESS")
	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated)

	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("a run in one repository armed the finished result of the other: %v", got)
	}
	dbfx.Exec(t, `UPDATE github_pull_request SET checks_rollup_state = 'SUCCESS' WHERE id = $1`, running)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks after the running checks finished, want 1", len(got))
	}
}

// legacyKeys rewrites the stored condition the way the previous build wrote it:
// both maps keyed by the pull request number alone.
func legacyKeys(t *testing.T, wakeupID string, number int64, baseline string, armed bool) {
	t.Helper()
	var raw []byte
	dbfx.QueryRow(t, `SELECT condition FROM chat_wakeup WHERE id = $1`, wakeupID).Scan(&raw)
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	key := strconv.FormatInt(number, 10)
	stored["baseline"] = map[string]string{key: baseline}
	delete(stored, "armed")
	if armed {
		stored["armed"] = []string{key}
	}
	legacy, _ := json.Marshal(stored)
	dbfx.Exec(t, `UPDATE chat_wakeup SET condition = $2 WHERE id = $1`, wakeupID, string(legacy))
}

// Bare-number state cannot say which repository it described, and the pull
// requests linked now do not say which were linked when it was written, so it is
// never attributed: not even when exactly one linked pull request has the number.
func TestChatWakeupChecksNeverAttributeLegacyNumberKeyedState(t *testing.T) {
	dm := newWakeupDM(t)
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })
	issue := dbfx.Issue(t, "One repository")
	number := time.Now().UnixMilli() % 100000
	pr := linkPullRequest(t, issue, "multica-ai", "repo-a", number, "aaa", "SUCCESS")
	var w wakeupJSON
	dm.createWithCondition(t, map[string]any{"issue": issue, "pr": "checks_finished"}).Want(http.StatusCreated).JSON(&w)
	legacyKeys(t, w.ID, number, "aaa:SUCCESS:", false)

	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("a legacy baseline fired: %v", got)
	}
	var raw []byte
	dbfx.QueryRow(t, `SELECT condition FROM chat_wakeup WHERE id = $1`, w.ID).Scan(&raw)
	if containsKey(raw, "multica-ai/repo-a#"+strconv.FormatInt(number, 10)) {
		t.Fatalf("legacy state was attributed to a repository by its link count: %s", raw)
	}

	dbfx.Exec(t, `UPDATE github_pull_request SET snapshot_head_sha = 'bbb', checks_rollup_state = 'FAILURE' WHERE id = $1`, pr)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("legacy state decided a checks result: %v", got)
	}
	// The deadline still ends the wait.
	dm.makeDue(t, w.ID)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks at the deadline, want 1", len(got))
	}
}

func TestChatWakeupChecksDoNotGuessAnAmbiguousLegacyBaseline(t *testing.T) {
	dm, wakeupID, prA, _, number := twoRepositoriesSameNumber(t)
	legacyKeys(t, wakeupID, number, "aaa:SUCCESS:", false)

	dm.tick(t)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("a legacy baseline shared by two repositories fired: %v", got)
	}
	// The deadline still ends the wait, so nothing is lost for good.
	dbfx.Exec(t, `UPDATE github_pull_request SET snapshot_head_sha = 'zzz' WHERE id = $1`, prA)
	dm.makeDue(t, wakeupID)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 1 {
		t.Fatalf("%d wakeup tasks at the deadline, want 1", len(got))
	}
}

func containsKey(raw []byte, key string) bool {
	var stored struct {
		Baseline map[string]string `json:"baseline"`
	}
	if json.Unmarshal(raw, &stored) != nil {
		return false
	}
	_, ok := stored.Baseline[key]
	return ok
}

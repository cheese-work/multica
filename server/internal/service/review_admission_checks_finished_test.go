package service

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-4982: a `--until-pr checks` wakeup (checks_finished) fires when checks
// finish red as well as green. A reviewer's run on a red head is the wasted
// review-deferred loop: it must start no task and leave a deferral on record.
// A green head still starts exactly one run.
func TestAdmissionChecksFinishedWakeupDefersOnRedAndStartsOnGreen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		derived bool
	}{{"named by a person", false}, {"armed by an agent run", true}} {
		t.Run(tc.name, func(t *testing.T) { checksFinishedWakeup(t, tc.derived) })
	}
}

func checksFinishedWakeup(t *testing.T, derived bool) {
	f, s, issue, agent := conditionFixture(t)
	s.Tasks.PRSnapshotsEnabled = true
	prev := admissionChecks
	admissionChecks = []string{"ci"}
	t.Cleanup(func() { admissionChecks = prev })
	enableAdmission(t, true)
	setAdmissionReviewers(t, agent)
	issueID := util.UUIDToString(issue)
	f.Exec(t, `UPDATE issue SET status = 'in_review', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, agent)
	pr := f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1, "repo_owner": "o", "repo_name": "r",
		"pr_number": int(time.Now().UnixNano() % 1000000000), "title": "t", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": time.Now(), "pr_updated_at": time.Now(),
		"head_sha": "bbb", "snapshot_head_sha": "aaa", "checks_rollup_state": "SUCCESS", "api_merge_state_status": "CLEAN",
	})
	f.Cleanup(t, "DELETE FROM github_pull_request_check_run WHERE pr_id=$1", pr)
	f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
	f.Cleanup(t, `DELETE FROM review_admission_deferral WHERE issue_id = $1`, issueID)
	f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Review once checks finish",
		Condition: condition(t, map[string]any{"type": "pull_request", "event": "checks_finished"})})
	if derived {
		// An agent armed the rule from a task: the run is derived, not named.
		f.Exec(t, "UPDATE issue_wakeup SET source_task_id=$2 WHERE id=$1", w.ID, util.UUIDToString(dbid.NewV7()))
	}
	wakeTick(t, f, s, w.ID) // baseline: the old head's result is not news

	// The new head's checks finish red.
	f.Exec(t, `INSERT INTO github_pull_request_check_run(pr_id,head_sha,ordinal,name,status,conclusion) VALUES($1,'bbb',0,'ci','completed','failure')`, pr)
	f.Exec(t, "UPDATE github_pull_request SET snapshot_head_sha='bbb',checks_rollup_state='FAILURE' WHERE id=$1", pr)
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", w.ID)
	due, err := f.q.LocklessWakeup(context.Background(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.dispatchFromTick(context.Background(), due)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("a checks_finished wake on a red head started %d runs, want 0", n)
	}
	var head, trigger string
	if err := f.Pool.QueryRow(context.Background(),
		`SELECT head_sha, trigger FROM review_admission_deferral WHERE issue_id = $1`, issueID).Scan(&head, &trigger); err != nil {
		t.Fatalf("no deferral on record for the red head: %v", err)
	}
	if head != "bbb" || trigger != "wakeup" {
		t.Fatalf("deferral head %q trigger %q, want bbb and wakeup", head, trigger)
	}

	// The same head turns green: one run.
	f.Exec(t, "UPDATE github_pull_request_check_run SET conclusion='success' WHERE pr_id=$1 AND name='ci'", pr)
	f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='SUCCESS' WHERE id=$1", pr)
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("a checks_finished wake on a green head started %d runs, want 1", n)
	}
}

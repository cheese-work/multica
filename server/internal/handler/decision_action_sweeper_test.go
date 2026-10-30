package handler

import "testing"

// An approval whose process died between COMMIT and the post-commit work
// leaves post_commit_done_at empty. The worker finishes that work later, but
// never while the approving request could still be doing it itself.
func TestWorkerFinishesPostCommitWorkOfAnOrphanedApproval(test *testing.T) {
	resetDecisionLease(test)
	action := newPendingSecretAction(test)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{"KEY": secretSentinel}), 200)
	stampIsSet := `SELECT count(*) FROM decision_action WHERE id = $1 AND post_commit_done_at IS NOT NULL`

	dbfx.Exec(test, `UPDATE decision_action SET post_commit_done_at = NULL WHERE id = $1`, action.actionID)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if n := dbfx.Count(test, stampIsSet, action.actionID); n != 0 {
		test.Fatal("worker took over post-commit work of an approval that just committed")
	}

	dbfx.Exec(test, `UPDATE decision_action SET executed_at = clock_timestamp() - interval '2 minutes' WHERE id = $1`, action.actionID)
	decisionPass(test, runner)
	if n := dbfx.Count(test, stampIsSet, action.actionID); n != 1 {
		test.Fatal("worker did not finish the orphaned approval's post-commit work")
	}
}

package handler

import (
	"testing"
)

// Luna's overlay tests for LUNA-P1-1 (CHE-1874 verdict 01a15d0e-dfb5-7ac6-80b8-d951e2f8c595), kept as received.
// They share the helpers of chat_wakeup_condition_repos_test.go.

func TestR18AmbiguousLegacyBaselineDoesNotBecomeUniqueAfterUnlink(t *testing.T) {
	dm, wakeupID, prA, _, number := twoRepositoriesSameNumber(t)
	legacyKeys(t, wakeupID, number, "aaa:SUCCESS:", false)
	dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prA)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("ambiguous legacy baseline fired after unlink: %v", got)
	}
}

func TestR18AmbiguousLegacyArmedStateDoesNotBecomeUniqueAfterUnlink(t *testing.T) {
	dm, wakeupID, prA, _, number := twoRepositoriesSameNumber(t)
	legacyKeys(t, wakeupID, number, "bbb:FAILURE:", true)
	dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prA)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("ambiguous legacy armed state fired after unlink: %v", got)
	}
}

// The same transition when the wakeup was evaluated once while both pull
// requests were still linked.
func TestChatWakeupAmbiguousLegacyBaselineStaysInertWhenAFirstEvaluationPrecedesTheUnlink(t *testing.T) {
	dm, wakeupID, prA, _, number := twoRepositoriesSameNumber(t)
	legacyKeys(t, wakeupID, number, "aaa:SUCCESS:", false)
	dm.tick(t)
	dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prA)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("ambiguous legacy baseline fired after an evaluation and an unlink: %v", got)
	}
}

func TestChatWakeupAmbiguousLegacyArmedStateStaysInertWhenAFirstEvaluationPrecedesTheUnlink(t *testing.T) {
	dm, wakeupID, prA, _, number := twoRepositoriesSameNumber(t)
	legacyKeys(t, wakeupID, number, "bbb:FAILURE:", true)
	dm.tick(t)
	dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prA)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("ambiguous legacy armed state fired after an evaluation and an unlink: %v", got)
	}
}

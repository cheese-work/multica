package handler

import (
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6175: a derived run that follows a progress event passes the weekly run
// guard once; a run with no new event stays refused.

// progressEvent records one progress event on the case's issue at the given
// time. Each is shaped as production writes it.
type progressEvent func(t *testing.T, c budgetCase, at time.Time)

func prEvent(kind string) progressEvent {
	return func(t *testing.T, c budgetCase, at time.Time) {
		dbfx.Insert(t, "issue_pr_event", testutil.Cols{
			"workspace_id": testWorkspaceID, "issue_id": c.issueID, "pull_request_id": uuid.NewString(),
			"kind": kind, "head_sha": uuid.NewString(), "observed_at": at, "dedupe_key": uuid.NewString(),
		})
	}
}

// newlyLinkedPR links a new open PR through the production link query. The first
// snapshot of a newly linked PR emits no head_changed event, so the link row
// is the only record of its head.
func newlyLinkedPR(t *testing.T, c budgetCase, _ time.Time) {
	linkPR(t, c.issueID, int32(6175000+rand.Intn(900000)), uuid.NewString())
	dbfx.Cleanup(t, `DELETE FROM issue_pull_request WHERE issue_id = $1`, c.issueID)
}

func verdict(action string) progressEvent {
	return func(t *testing.T, c budgetCase, at time.Time) {
		dbfx.Insert(t, "activity_log", testutil.Cols{
			"workspace_id": testWorkspaceID, "issue_id": c.issueID, "actor_type": "agent", "actor_id": c.agentID,
			"action": action, "details": `{}`, "created_at": at,
		})
	}
}

func answeredDecision(t *testing.T, c budgetCase, at time.Time) {
	req := dbfx.Insert(t, "decision_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": c.issueID, "asking_agent_id": c.agentID, "human_user_id": testUserID,
		"chat_session_id": uuid.NewString(), "escalation_class": "reversible", "origin": "asked", "status": "answered",
	})
	dbfx.Insert(t, "decision_answer", testutil.Cols{
		"decision_request_id": req, "answered_by_task_id": uuid.NewString(), "source_chat_session_id": uuid.NewString(),
		"chat_message_ids": "{" + uuid.NewString() + "}", "content_snapshot": "go", "effective_at": at,
	})
}

func TestWeeklyRunGuardProgressEventAdmitsOneRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name  string
		event progressEvent
	}{
		{"new PR head (AC1)", prEvent("head_changed")},
		{"newly linked PR, no head_changed event (AC1)", newlyLinkedPR},
		{"merged PR (AC2)", prEvent("merged")},
		{"signed PASS verdict (AC3)", verdict("review_passed")},
		{"signed CHANGES verdict (AC3)", verdict("review_changes_requested")},
		{"answered decision request (AC4)", answeredDecision},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := weeklyCase(t, int64(6175100+i))
			seedRuns(t, c, c.agentID, 31, 2*24*time.Hour)
			c.weeklyTripped(t) // no event: refused
			tc.event(t, c, time.Now().Add(-time.Minute))
			if err := c.admit(t); err != nil {
				t.Fatalf("run after a progress event = %v, want admitted", err)
			}
			c.weeklyTripped(t) // the same event admits no second run
		})
	}
}

func TestWeeklyRunGuardRefusesEventOlderThanTheAgentsLastRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := weeklyCase(t, 6175110)
	prEvent("head_changed")(t, c, time.Now().Add(-3*24*time.Hour))
	seedRuns(t, c, c.agentID, 31, 2*24*time.Hour) // newest run is after the event
	c.weeklyTripped(t)
}

func TestWeeklyRunGuardIgnoresNonProgressEvents(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := weeklyCase(t, 6175111)
	seedRuns(t, c, c.agentID, 31, 2*24*time.Hour)
	for _, kind := range []string{"conflict", "checks_failed", "closed", "draft", "ready"} {
		prEvent(kind)(t, c, time.Now().Add(-time.Minute))
	}
	c.weeklyTripped(t)
}

func TestWeeklyRunGuardProgressNeverPassesTheHardCeiling(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := weeklyCase(t, 6175112)
	seedRuns(t, c, c.agentID, 2*service.DefaultWeeklyRunGuardMaxRuns, 2*24*time.Hour)
	prEvent("merged")(t, c, time.Now().Add(-time.Minute))
	c.weeklyTripped(t)
}

func TestWeeklyRunGuardProgressKeepsTheLimitAndTheLoopBreaker(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	if service.DefaultWeeklyRunGuardMaxRuns != 30 {
		t.Fatalf("default limit = %d, want 30 (AC6)", service.DefaultWeeklyRunGuardMaxRuns)
	}
	c := weeklyCase(t, 6175113)
	t.Setenv("MULTICA_LOOP_BREAKER_MAX_RUNS", "5")
	seedRuns(t, c, c.agentID, 31, 10*time.Minute)
	prEvent("merged")(t, c, time.Now().Add(-time.Minute))
	if err := c.enqueue(t); !errors.Is(err, service.ErrLoopBreaker) {
		t.Fatalf("run = %v, want ErrLoopBreaker: the breaker runs before the weekly guard (AC5)", err)
	}
}

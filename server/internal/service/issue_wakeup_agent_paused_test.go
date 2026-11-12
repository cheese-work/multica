package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-5066: a rule whose agent is paused starts nothing; what arrives meanwhile
// is dropped, not replayed, and the skip is logged on the issue timeline.

func setAgentPaused(t *testing.T, f principalFixture, agent string, paused bool) {
	t.Helper()
	if paused {
		f.Exec(t, "UPDATE agent SET scheduling_paused_at=now() WHERE id=$1", agent)
		return
	}
	f.Exec(t, "UPDATE agent SET scheduling_paused_at=NULL, scheduling_paused_by=NULL WHERE id=$1", agent)
}

func TestPausedAgentSkipsWakeupAndLogsIt(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "Look at it"})
	setAgentPaused(t, f, agent, true)
	f.Comment(t, util.UUIDToString(issue), "arrives while the agent is paused")
	wakeDispatch(t, s, w)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("paused agent started %d runs", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_skipped' AND details->>'reason_code'='agent_paused'`, util.UUIDToString(issue)); n != 1 {
		t.Fatalf("agent_paused skip activities = %d, want 1", n)
	}
	setAgentPaused(t, f, agent, false)
	wakeDispatch(t, s, w)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("resume replayed %d dropped inputs", n)
	}
	f.Comment(t, util.UUIDToString(issue), "arrives after resume")
	wakeDispatch(t, s, w)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("after resume the rule started %d runs, want 1", n)
	}
}
